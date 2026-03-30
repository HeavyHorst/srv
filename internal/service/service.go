package service

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"
	"tailscale.com/client/local"
	"tailscale.com/tsnet"

	"srv/internal/config"
	"srv/internal/model"
	"srv/internal/provision"
	"srv/internal/store"
)

type App struct {
	cfg         config.Config
	log         *slog.Logger
	store       *store.Store
	provisioner *provision.Provisioner
	tailscale   *tsnet.Server
	localAPI    *local.Client
	sshServer   *gssh.Server
	mu          sync.Mutex
}

type commandResult struct {
	stdout   string
	stderr   string
	exitCode int
}

type logTarget string

const (
	logTargetAll         logTarget = "all"
	logTargetSerial      logTarget = "serial"
	logTargetFirecracker logTarget = "firecracker"
	defaultLogTailLines            = 40
	mib                            = int64(1024 * 1024)
)

func New(cfg config.Config, logger *slog.Logger) (*App, error) {
	for _, dir := range []string{cfg.DataDirAbs(), cfg.StateDir(), cfg.ImagesDir(), cfg.InstancesDir(), cfg.TSNetDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := os.Chmod(cfg.InstancesDir(), 0o770); err != nil {
		return nil, fmt.Errorf("set instances dir permissions: %w", err)
	}

	st, err := store.Open(cfg.DatabasePath())
	if err != nil {
		return nil, err
	}

	prov, err := provision.New(cfg, logger, st)
	if err != nil {
		_ = st.Close()
		return nil, err
	}

	return &App{cfg: cfg, log: logger, store: st, provisioner: prov}, nil
}

func (a *App) Run(ctx context.Context) error {
	defer func() {
		if err := a.store.Close(); err != nil {
			a.log.Error("close store", "err", err)
		}
	}()

	hostSigner, err := ensureHostSigner(a.cfg.HostKeyPath())
	if err != nil {
		return fmt.Errorf("ensure host signer: %w", err)
	}

	a.tailscale = &tsnet.Server{
		Dir:           a.cfg.TSNetDir(),
		Hostname:      a.cfg.Hostname,
		AuthKey:       a.cfg.TailscaleAuthKey,
		ClientID:      a.cfg.TailscaleClientID,
		ClientSecret:  a.cfg.TailscaleClientSecret,
		ControlURL:    a.cfg.TailscaleControlURL,
		AdvertiseTags: a.cfg.TailscaleAdvertiseTags,
		UserLogf: func(format string, args ...any) {
			a.log.Info("tailscale", "msg", fmt.Sprintf(format, args...))
		},
	}

	status, err := a.tailscale.Up(ctx)
	if err != nil {
		return fmt.Errorf("bring tsnet node up: %w", err)
	}
	a.log.Info("tailscale ready", "backend_state", status.BackendState)

	a.localAPI, err = a.tailscale.LocalClient()
	if err != nil {
		return fmt.Errorf("open tailscale local api client: %w", err)
	}

	listener, err := a.tailscale.Listen("tcp", a.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", a.cfg.ListenAddr, err)
	}

	a.sshServer = &gssh.Server{
		Handler:     a.handleSession,
		HostSigners: []gssh.Signer{hostSigner},
		IdleTimeout: 5 * time.Minute,
		MaxTimeout:  30 * time.Minute,
		PtyCallback: func(ctx gssh.Context, pty gssh.Pty) bool { return false },
		SessionRequestCallback: func(sess gssh.Session, requestType string) bool {
			return requestType != "shell" && requestType != "subsystem"
		},
	}

	g, groupCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		err := a.sshServer.Serve(listener)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("serve ssh: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		a.restoreInstances(groupCtx)
		return nil
	})
	g.Go(func() error {
		<-groupCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if a.sshServer != nil {
			_ = a.sshServer.Shutdown(shutdownCtx)
		}
		if a.tailscale != nil {
			_ = a.tailscale.Close()
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	return nil
}

func (a *App) restoreInstances(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.provisioner.RestoreInstances(ctx); err != nil && ctx.Err() == nil {
		a.log.Error("restore instances on startup", "err", err)
	}
}

func (a *App) handleSession(sess gssh.Session) {
	started := time.Now().UTC()
	args := sess.Command()
	command := ""
	if len(args) > 0 {
		command = args[0]
	}
	argsJSON, _ := json.Marshal(args)
	audit := model.CommandAudit{
		CreatedAt:  started,
		RemoteAddr: sess.RemoteAddr().String(),
		SSHUser:    sess.User(),
		Command:    command,
		ArgsJSON:   string(argsJSON),
	}

	finalize := func(actor model.Actor, allowed bool, reason string, err error) {
		audit.ActorUser = actor.UserLogin
		audit.ActorDisplayName = actor.DisplayName
		audit.ActorNode = actor.NodeName
		audit.Allowed = allowed
		audit.Reason = reason
		audit.DurationMS = time.Since(started).Milliseconds()
		if err != nil {
			audit.ErrorText = err.Error()
		}
		if derr := a.store.RecordCommand(context.Background(), audit); derr != nil {
			a.log.Error("record command audit", "err", derr)
		}
	}

	if len(args) == 0 {
		err := errors.New("shell sessions are disabled; use an exec request such as: ssh root@srv list")
		_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
		finalize(model.Actor{SSHUser: sess.User(), RemoteAddr: sess.RemoteAddr().String()}, false, "shell denied", err)
		_ = sess.Exit(2)
		return
	}

	actor, err := a.resolveActor(sess.Context(), sess)
	if err != nil {
		_, _ = io.WriteString(sess.Stderr(), fmt.Sprintf("resolve tailscale identity: %v\n", err))
		finalize(model.Actor{SSHUser: sess.User(), RemoteAddr: sess.RemoteAddr().String()}, false, "whois failed", err)
		_ = sess.Exit(1)
		return
	}

	allowed, reason := a.authorize(actor, command)
	if err := a.store.RecordAuthz(context.Background(), model.AuthzDecision{
		CreatedAt:  started,
		ActorUser:  actor.UserLogin,
		ActorNode:  actor.NodeName,
		RemoteAddr: actor.RemoteAddr,
		Action:     command,
		Allowed:    allowed,
		Reason:     reason,
	}); err != nil {
		a.log.Error("record authz decision", "err", err)
	}
	if !allowed {
		err := fmt.Errorf("not authorized: %s", reason)
		_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
		finalize(actor, false, reason, err)
		_ = sess.Exit(1)
		return
	}

	result, err := a.dispatch(sess.Context(), actor, args)
	if result.stdout != "" {
		_, _ = io.WriteString(sess, result.stdout)
	}
	if result.stderr != "" {
		_, _ = io.WriteString(sess.Stderr(), result.stderr)
	}
	finalize(actor, true, reason, err)
	if result.exitCode == 0 && err != nil {
		result.exitCode = 1
	}
	_ = sess.Exit(result.exitCode)
}

func (a *App) dispatch(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	switch args[0] {
	case "new":
		return a.cmdNew(ctx, actor, args)
	case "resize":
		return a.cmdResize(ctx, actor, args)
	case "list":
		return a.cmdList(ctx, actor)
	case "inspect":
		return a.cmdInspect(ctx, actor, args)
	case "logs":
		return a.cmdLogs(ctx, actor, args)
	case "start":
		return a.cmdStart(ctx, actor, args)
	case "stop":
		return a.cmdStop(ctx, actor, args)
	case "restart":
		return a.cmdRestart(ctx, actor, args)
	case "delete":
		return a.cmdDelete(ctx, actor, args)
	case "help":
		return helpResult(), nil
	default:
		return commandResult{stderr: fmt.Sprintf("unknown command %q\n", args[0]), exitCode: 2}, fmt.Errorf("unknown command %q", args[0])
	}
}

func (a *App) cmdNew(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	name, opts, err := parseNewArgs(args)
	if err != nil {
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	inst, err := a.provisioner.Create(ctx, name, actor, opts)
	if err != nil {
		stderr := fmt.Sprintf("create %s: %v\n", name, err)
		if inst.Name != "" {
			stderr += instanceDebugHints(a.cfg.Hostname, inst)
		}
		return commandResult{stderr: stderr, exitCode: 1}, err
	}

	return lifecycleReadyResult("created", inst), nil
}

func (a *App) cmdResize(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	name, opts, err := parseResizeArgs(args)
	if err != nil {
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, err := a.lookupVisibleInstance(ctx, actor, name); err != nil {
		return missingInstanceResult("resize", name, err)
	}

	inst, err := a.provisioner.Resize(ctx, name, opts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = fmt.Errorf("instance %q does not exist", name)
		}
		stderr := fmt.Sprintf("resize %s: %v\n", name, err)
		if inst.Name != "" {
			stderr += instanceDebugHints(a.cfg.Hostname, inst)
		}
		return commandResult{stderr: stderr, exitCode: 1}, err
	}

	stdout := fmt.Sprintf(
		"resized: %s\nstate: %s\nvcpus: %d\nmemory: %d MiB\nrootfs-size: %s\n",
		inst.Name,
		inst.State,
		effectiveInstanceVCPUCount(inst, a.cfg),
		effectiveInstanceMemoryMiB(inst, a.cfg),
		formatBinarySize(effectiveInstanceRootFSSizeBytes(inst)),
	)
	return commandResult{stdout: stdout, exitCode: 0}, nil
}

func (a *App) cmdList(ctx context.Context, actor model.Actor) (commandResult, error) {
	instances, err := a.store.ListInstances(ctx, false)
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("list instances: %v\n", err), exitCode: 1}, err
	}
	instances = a.visibleInstances(actor, instances)
	if len(instances) == 0 {
		return commandResult{stdout: "no instances\n", exitCode: 0}, nil
	}

	var b strings.Builder
	for _, inst := range instances {
		line := fmt.Sprintf("%s\t%s", inst.Name, inst.State)
		if inst.TailscaleIP != "" {
			line += fmt.Sprintf("\t%s", inst.TailscaleIP)
		}
		if inst.TailscaleName != "" {
			line += fmt.Sprintf("\t%s", inst.TailscaleName)
		}
		b.WriteString(line + "\n")
	}
	return commandResult{stdout: b.String(), exitCode: 0}, nil
}

func (a *App) cmdInspect(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	if len(args) != 2 {
		err := errors.New("usage: inspect <name>")
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	inst, err := a.lookupVisibleInstance(ctx, actor, args[1])
	if err != nil {
		return missingInstanceResult("inspect", args[1], err)
	}
	events, err := a.store.ListEvents(ctx, inst.ID, 10)
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("load events: %v\n", err), exitCode: 1}, err
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("name: %s\n", inst.Name))
	b.WriteString(fmt.Sprintf("state: %s\n", inst.State))
	b.WriteString(fmt.Sprintf("created-by: %s via %s\n", inst.CreatedByUser, inst.CreatedByNode))
	b.WriteString(fmt.Sprintf("created-at: %s\n", inst.CreatedAt.Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("updated-at: %s\n", inst.UpdatedAt.Format(time.RFC3339)))
	if vcpus := effectiveInstanceVCPUCount(inst, a.cfg); vcpus > 0 {
		b.WriteString(fmt.Sprintf("vcpus: %d\n", vcpus))
	}
	if mem := effectiveInstanceMemoryMiB(inst, a.cfg); mem > 0 {
		b.WriteString(fmt.Sprintf("memory: %d MiB\n", mem))
	}
	b.WriteString(fmt.Sprintf("rootfs: %s\n", inst.RootFSPath))
	if size := effectiveInstanceRootFSSizeBytes(inst); size > 0 {
		b.WriteString(fmt.Sprintf("rootfs-size: %s\n", formatBinarySize(size)))
	}
	b.WriteString(fmt.Sprintf("firecracker-pid: %d\n", inst.FirecrackerPID))
	b.WriteString(fmt.Sprintf("tap-device: %s\n", inst.TapDevice))
	b.WriteString(fmt.Sprintf("network: %s\n", inst.NetworkCIDR))
	b.WriteString(fmt.Sprintf("host-ip: %s\n", inst.HostAddr))
	b.WriteString(fmt.Sprintf("guest-ip: %s\n", inst.GuestAddr))
	if inst.TailscaleName != "" {
		b.WriteString(fmt.Sprintf("tailscale-name: %s\n", inst.TailscaleName))
	}
	if inst.TailscaleIP != "" {
		b.WriteString(fmt.Sprintf("tailscale-ip: %s\n", inst.TailscaleIP))
	}
	if inst.LastError != "" {
		b.WriteString(fmt.Sprintf("last-error: %s\n", inst.LastError))
	}
	if inst.DeletedAt != nil {
		b.WriteString(fmt.Sprintf("deleted-at: %s\n", inst.DeletedAt.Format(time.RFC3339)))
	}
	b.WriteString(fmt.Sprintf("logs-serial: ssh root@%s logs %s serial\n", a.cfg.Hostname, inst.Name))
	b.WriteString(fmt.Sprintf("logs-firecracker: ssh root@%s logs %s firecracker\n", a.cfg.Hostname, inst.Name))
	if hint := inspectDebugHint(inst); hint != "" {
		b.WriteString(fmt.Sprintf("debug-hint: %s\n", hint))
	}
	b.WriteString("events:\n")
	for _, evt := range events {
		b.WriteString(fmt.Sprintf("- %s [%s] %s\n", evt.CreatedAt.Format(time.RFC3339), evt.Type, evt.Message))
	}
	return commandResult{stdout: b.String(), exitCode: 0}, nil
}

func (a *App) cmdLogs(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	name, target, err := parseLogsArgs(args)
	if err != nil {
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	inst, err := a.lookupVisibleInstance(ctx, actor, name)
	if err != nil {
		return missingInstanceResult("logs", name, err)
	}

	stdout, err := formatLogOutput(inst, target)
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("logs %s: %v\n", name, err), exitCode: 1}, err
	}
	return commandResult{stdout: stdout, exitCode: 0}, nil
}

func (a *App) cmdStart(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	if len(args) != 2 {
		err := errors.New("usage: start <name>")
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, err := a.lookupVisibleInstance(ctx, actor, args[1]); err != nil {
		return missingInstanceResult("start", args[1], err)
	}

	inst, err := a.provisioner.Start(ctx, args[1])
	if err != nil {
		stderr := fmt.Sprintf("start %s: %v\n", args[1], err)
		if inst.Name != "" {
			stderr += instanceDebugHints(a.cfg.Hostname, inst)
		}
		return commandResult{stderr: stderr, exitCode: 1}, err
	}
	return lifecycleReadyResult("started", inst), nil
}

func (a *App) cmdStop(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	if len(args) != 2 {
		err := errors.New("usage: stop <name>")
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, err := a.lookupVisibleInstance(ctx, actor, args[1]); err != nil {
		return missingInstanceResult("stop", args[1], err)
	}

	inst, err := a.provisioner.Stop(ctx, args[1])
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = fmt.Errorf("instance %q does not exist", args[1])
		}
		return commandResult{stderr: fmt.Sprintf("stop %s: %v\n", args[1], err), exitCode: 1}, err
	}
	return commandResult{stdout: fmt.Sprintf("stopped: %s\nstate: %s\n", inst.Name, inst.State), exitCode: 0}, nil
}

func (a *App) cmdRestart(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	if len(args) != 2 {
		err := errors.New("usage: restart <name>")
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, err := a.lookupVisibleInstance(ctx, actor, args[1]); err != nil {
		return missingInstanceResult("restart", args[1], err)
	}

	if _, err := a.provisioner.Stop(ctx, args[1]); err != nil && !strings.Contains(err.Error(), "already stopped") {
		if errors.Is(err, sql.ErrNoRows) {
			err = fmt.Errorf("instance %q does not exist", args[1])
		}
		return commandResult{stderr: fmt.Sprintf("restart %s: %v\n", args[1], err), exitCode: 1}, err
	}
	inst, err := a.provisioner.Start(ctx, args[1])
	if err != nil {
		stderr := fmt.Sprintf("restart %s: %v\n", args[1], err)
		if inst.Name != "" {
			stderr += instanceDebugHints(a.cfg.Hostname, inst)
		}
		return commandResult{stderr: stderr, exitCode: 1}, err
	}
	return lifecycleReadyResult("restarted", inst), nil
}

func (a *App) cmdDelete(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	if len(args) != 2 {
		err := errors.New("usage: delete <name>")
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, err := a.lookupVisibleInstance(ctx, actor, args[1]); err != nil {
		return missingInstanceResult("delete", args[1], err)
	}

	inst, err := a.provisioner.Delete(ctx, args[1])
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = fmt.Errorf("instance %q does not exist", args[1])
		}
		return commandResult{stderr: fmt.Sprintf("delete %s: %v\n", args[1], err), exitCode: 1}, err
	}
	return commandResult{stdout: fmt.Sprintf("deleted: %s\nstate: %s\n", inst.Name, inst.State), exitCode: 0}, nil
}

func helpResult() commandResult {
	return commandResult{stdout: "commands: new <name> [--cpus N] [--ram SIZE] [--rootfs-size SIZE], resize <name> [--cpus N] [--ram SIZE] [--rootfs-size SIZE], list, inspect <name>, logs <name> [serial|firecracker], start <name>, stop <name>, restart <name>, delete <name>\n", exitCode: 0}
}

func lifecycleReadyResult(action string, inst model.Instance) commandResult {
	stdout := fmt.Sprintf("%s: %s\nstate: %s\ntailscale-name: %s\ntailscale-ip: %s\nconnect: ssh root@%s\n", action, inst.Name, inst.State, inst.TailscaleName, inst.TailscaleIP, inst.Name)
	return commandResult{stdout: stdout, exitCode: 0}
}

func (a *App) resolveActor(ctx context.Context, sess gssh.Session) (model.Actor, error) {
	remote := sess.RemoteAddr().String()
	who, err := a.localAPI.WhoIs(ctx, remote)
	if err != nil {
		return model.Actor{}, err
	}
	if who == nil || who.Node == nil || who.UserProfile == nil {
		return model.Actor{}, errors.New("tailscale whois response was incomplete")
	}
	return model.Actor{
		UserLogin:   who.UserProfile.LoginName,
		DisplayName: who.UserProfile.DisplayName,
		NodeName:    trimNodeName(who.Node.ComputedName, who.Node.Name),
		RemoteAddr:  remote,
		SSHUser:     sess.User(),
	}, nil
}

func (a *App) authorize(actor model.Actor, command string) (bool, string) {
	if len(a.cfg.AllowedUsers) == 0 {
		return true, "allowed because SRV_ALLOWED_USERS is empty"
	}
	for _, user := range a.cfg.AllowedUsers {
		if strings.EqualFold(user, actor.UserLogin) {
			return true, fmt.Sprintf("%s allowed to run %s", actor.UserLogin, command)
		}
	}
	return false, fmt.Sprintf("%s is not in SRV_ALLOWED_USERS", actor.UserLogin)
}

func (a *App) isAdmin(actor model.Actor) bool {
	for _, user := range a.cfg.AdminUsers {
		if strings.EqualFold(user, actor.UserLogin) {
			return true
		}
	}
	return false
}

func (a *App) canAccessInstance(actor model.Actor, inst model.Instance) bool {
	if a.isAdmin(actor) {
		return true
	}
	return strings.EqualFold(inst.CreatedByUser, actor.UserLogin)
}

func (a *App) visibleInstances(actor model.Actor, instances []model.Instance) []model.Instance {
	if a.isAdmin(actor) {
		return instances
	}
	visible := make([]model.Instance, 0, len(instances))
	for _, inst := range instances {
		if a.canAccessInstance(actor, inst) {
			visible = append(visible, inst)
		}
	}
	return visible
}

func (a *App) lookupVisibleInstance(ctx context.Context, actor model.Actor, name string) (model.Instance, error) {
	inst, err := a.store.GetInstance(ctx, name)
	if err != nil {
		return model.Instance{}, err
	}
	if !a.canAccessInstance(actor, inst) {
		return model.Instance{}, sql.ErrNoRows
	}
	return inst, nil
}

func trimNodeName(primary, fallback string) string {
	if primary != "" {
		return strings.TrimSuffix(primary, ".")
	}
	return strings.TrimSuffix(fallback, ".")
}

func parseNewArgs(args []string) (string, provision.CreateOptions, error) {
	return parseSizedInstanceArgs(args, newUsage())
}

func parseResizeArgs(args []string) (string, provision.CreateOptions, error) {
	name, opts, err := parseSizedInstanceArgs(args, resizeUsage())
	if err != nil {
		return "", provision.CreateOptions{}, err
	}
	if opts == (provision.CreateOptions{}) {
		return "", provision.CreateOptions{}, fmt.Errorf("resize requires at least one of --cpus, --ram, or --rootfs-size\n%s", resizeUsage())
	}
	return name, opts, nil
}

func parseLogsArgs(args []string) (string, logTarget, error) {
	if len(args) < 2 || len(args) > 3 {
		return "", "", errors.New(logsUsage())
	}
	if len(args) == 2 {
		return args[1], logTargetAll, nil
	}
	switch logTarget(args[2]) {
	case logTargetSerial:
		return args[1], logTargetSerial, nil
	case logTargetFirecracker:
		return args[1], logTargetFirecracker, nil
	default:
		return "", "", fmt.Errorf("unknown log target %q\n%s", args[2], logsUsage())
	}
}

func parseSizedInstanceArgs(args []string, usage string) (string, provision.CreateOptions, error) {
	if len(args) < 2 {
		return "", provision.CreateOptions{}, errors.New(usage)
	}

	var (
		name           string
		opts           provision.CreateOptions
		seenCPUs       bool
		seenRAM        bool
		seenRootFSSize bool
	)

	for i := 1; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			if name != "" {
				return "", provision.CreateOptions{}, fmt.Errorf("unexpected argument %q\n%s", arg, usage)
			}
			name = arg
			continue
		}

		key, value, hasValue := strings.Cut(arg, "=")
		if !hasValue {
			i++
			if i >= len(args) {
				return "", provision.CreateOptions{}, fmt.Errorf("missing value for %s\n%s", key, usage)
			}
			value = args[i]
		}

		switch key {
		case "--cpus":
			if seenCPUs {
				return "", provision.CreateOptions{}, fmt.Errorf("%s specified more than once\n%s", key, usage)
			}
			seenCPUs = true
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return "", provision.CreateOptions{}, fmt.Errorf("parse %s: %w\n%s", key, err, usage)
			}
			opts.VCPUCount = parsed
		case "--ram":
			if seenRAM {
				return "", provision.CreateOptions{}, fmt.Errorf("%s specified more than once\n%s", key, usage)
			}
			seenRAM = true
			parsed, err := parseSize(value, mib)
			if err != nil {
				return "", provision.CreateOptions{}, fmt.Errorf("parse %s: %v\n%s", key, err, usage)
			}
			opts.MemoryMiB = bytesToMiBCeil(parsed)
		case "--rootfs-size":
			if seenRootFSSize {
				return "", provision.CreateOptions{}, fmt.Errorf("%s specified more than once\n%s", key, usage)
			}
			seenRootFSSize = true
			parsed, err := parseSize(value, mib)
			if err != nil {
				return "", provision.CreateOptions{}, fmt.Errorf("parse %s: %v\n%s", key, err, usage)
			}
			opts.RootFSSizeBytes = parsed
		default:
			return "", provision.CreateOptions{}, fmt.Errorf("unknown option %q\n%s", key, usage)
		}
	}

	if name == "" {
		return "", provision.CreateOptions{}, errors.New(usage)
	}
	return name, opts, nil
}

func newUsage() string {
	return "usage: new <name> [--cpus N] [--ram SIZE] [--rootfs-size SIZE]"
}

func resizeUsage() string {
	return "usage: resize <name> [--cpus N] [--ram SIZE] [--rootfs-size SIZE]"
}

func logsUsage() string {
	return "usage: logs <name> [serial|firecracker]"
}

func parseSize(value string, defaultUnit int64) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("size cannot be empty")
	}

	idx := 0
	for idx < len(value) && value[idx] >= '0' && value[idx] <= '9' {
		idx++
	}
	if idx == 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}

	number, err := strconv.ParseInt(value[:idx], 10, 64)
	if err != nil {
		return 0, err
	}
	if number <= 0 {
		return 0, errors.New("size must be positive")
	}

	multiplier, ok := map[string]int64{
		"":    defaultUnit,
		"b":   1,
		"k":   1 << 10,
		"kb":  1 << 10,
		"kib": 1 << 10,
		"m":   1 << 20,
		"mb":  1 << 20,
		"mib": 1 << 20,
		"g":   1 << 30,
		"gb":  1 << 30,
		"gib": 1 << 30,
		"t":   1 << 40,
		"tb":  1 << 40,
		"tib": 1 << 40,
	}[strings.ToLower(strings.TrimSpace(value[idx:]))]
	if !ok {
		return 0, fmt.Errorf("unknown size unit %q", strings.TrimSpace(value[idx:]))
	}
	if number > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("size %q is too large", value)
	}
	return number * multiplier, nil
}

func bytesToMiBCeil(sizeBytes int64) int64 {
	return (sizeBytes + mib - 1) / mib
}

func effectiveInstanceVCPUCount(inst model.Instance, cfg config.Config) int64 {
	if inst.VCPUCount > 0 {
		return inst.VCPUCount
	}
	return cfg.VCPUCount
}

func effectiveInstanceMemoryMiB(inst model.Instance, cfg config.Config) int64 {
	if inst.MemoryMiB > 0 {
		return inst.MemoryMiB
	}
	return cfg.MemoryMiB
}

func effectiveInstanceRootFSSizeBytes(inst model.Instance) int64 {
	if inst.RootFSSizeBytes > 0 {
		return inst.RootFSSizeBytes
	}
	info, err := os.Stat(inst.RootFSPath)
	if err != nil {
		return 0
	}
	return info.Size()
}

func formatBinarySize(sizeBytes int64) string {
	if sizeBytes < 1024 {
		return fmt.Sprintf("%d B", sizeBytes)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	size := float64(sizeBytes)
	unit := "B"
	for _, next := range units {
		size /= 1024
		unit = next
		if size < 1024 {
			break
		}
	}
	return fmt.Sprintf("%.1f %s", size, unit)
}

func missingInstanceResult(command, name string, err error) (commandResult, error) {
	if errors.Is(err, sql.ErrNoRows) {
		err = fmt.Errorf("instance %q does not exist", name)
	}
	return commandResult{stderr: fmt.Sprintf("%s %s: %v\n", command, name, err), exitCode: 1}, err
}

func inspectDebugHint(inst model.Instance) string {
	if inst.State == model.StateAwaitingTailnet {
		return "guest has not finished initial tailnet bootstrap; start with the serial log"
	}
	if inst.State == model.StateFailed || inst.LastError != "" {
		return "boot and runtime failures usually show up first in the serial log, then in the Firecracker log"
	}
	return ""
}

func instanceDebugHints(hostname string, inst model.Instance) string {
	return fmt.Sprintf("inspect: ssh root@%s inspect %s\nlogs-serial: ssh root@%s logs %s serial\nlogs-firecracker: ssh root@%s logs %s firecracker\n", hostname, inst.Name, hostname, inst.Name, hostname, inst.Name)
}

func formatLogOutput(inst model.Instance, target logTarget) (string, error) {
	sections := make([]string, 0, 2)
	if target == logTargetAll || target == logTargetSerial {
		section, err := formatLogSection("serial", inst.SerialLogPath)
		if err != nil {
			return "", err
		}
		sections = append(sections, section)
	}
	if target == logTargetAll || target == logTargetFirecracker {
		section, err := formatLogSection("firecracker", inst.LogPath)
		if err != nil {
			return "", err
		}
		sections = append(sections, section)
	}
	return strings.Join(sections, "\n"), nil
}

func formatLogSection(label, path string) (string, error) {
	lines, exists, err := readLastLines(path, defaultLogTailLines)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("%s-log: %s\n", label, path))
	switch {
	case !exists:
		b.WriteString("(log file has not been created yet)\n")
	case len(lines) == 0:
		b.WriteString("(log is empty)\n")
	default:
		for _, line := range lines {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return b.String(), nil
}

func readLastLines(path string, limit int) ([]string, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lines := make([]string, 0, limit)
	for scanner.Scan() {
		if len(lines) == limit {
			copy(lines, lines[1:])
			lines[len(lines)-1] = scanner.Text()
			continue
		}
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}
	return lines, true, nil
}

func ensureHostSigner(path string) (ssh.Signer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err == nil {
		key, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read host key: %w", err)
		}
		return ssh.ParsePrivateKey(key)
	}

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate host key: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("marshal host key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write host key: %w", err)
	}
	return ssh.ParsePrivateKey(pemBytes)
}
