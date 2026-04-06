package service

import (
	"bufio"
	"bytes"
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
	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"
	"tailscale.com/client/local"
	"tailscale.com/tsnet"

	"srv/internal/config"
	"srv/internal/format"
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
	commandMu   sync.Mutex
	commandCond *sync.Cond
	commandOnce sync.Once
	snapshotOn  bool
	inFlight    int
	mu          sync.Mutex
	lockMu      sync.Mutex
	locks       map[string]*instanceLockEntry
}

type instanceLockEntry struct {
	mu   sync.Mutex
	refs int
}

type commandResult struct {
	stdout   string
	stderr   string
	exitCode int
}

type logTarget string

type logsRequest struct {
	name   string
	target logTarget
	follow bool
}

const (
	logTargetAll         logTarget = "all"
	logTargetSerial      logTarget = "serial"
	logTargetFirecracker logTarget = "firecracker"
	defaultLogTailLines            = 40
	maxLogChunkBytes               = 1024 * 1024
	mib                            = int64(1024 * 1024)
)

var (
	logFollowPollInterval      = 250 * time.Millisecond
	logFollowKeepAliveInterval = time.Minute
)

type terminalWriterState uint8

const (
	terminalWriterStateText terminalWriterState = iota
	terminalWriterStateEscape
	terminalWriterStateCSI
	terminalWriterStateOSC
	terminalWriterStateOSCEscape
	terminalWriterStateEscapeString
	terminalWriterStateEscapeStringEscape
)

type terminalSafeWriter struct {
	dst   io.Writer
	state terminalWriterState
}

func New(cfg config.Config, logger *slog.Logger) (*App, error) {
	for _, dir := range []string{cfg.DataDirAbs(), cfg.StateDir(), cfg.ImagesDir(), cfg.InstancesDir(), cfg.BackupsDir(), cfg.TSNetDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := os.Chmod(cfg.InstancesDir(), 0o770); err != nil {
		return nil, fmt.Errorf("set instances dir permissions: %w", err)
	}
	if err := os.Chmod(cfg.BackupsDir(), 0o770); err != nil {
		return nil, fmt.Errorf("set backups dir permissions: %w", err)
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

func (a *App) lockInstance(name string) func() {
	a.lockMu.Lock()
	if a.locks == nil {
		a.locks = make(map[string]*instanceLockEntry)
	}
	entry := a.locks[name]
	if entry == nil {
		entry = &instanceLockEntry{}
		a.locks[name] = entry
	}
	entry.refs++
	a.lockMu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		a.lockMu.Lock()
		defer a.lockMu.Unlock()
		entry.refs--
		if entry.refs == 0 {
			delete(a.locks, name)
		}
	}
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
	lease, err := a.beginCommand()
	if err != nil {
		_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
		_ = sess.Exit(1)
		return
	}
	defer lease.Release()
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
		lease.Release()
		a.waitForSnapshotBarrierToLift()
		if derr := a.store.RecordCommand(context.Background(), audit); derr != nil {
			a.log.Error("record command audit", "err", derr)
		}
	}

	if len(args) == 0 {
		err := errors.New("shell sessions are disabled; use an exec request such as: ssh srv list")
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
	if args[0] == "logs" {
		req, err := parseLogsArgs(args)
		if err != nil {
			_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
			finalize(actor, true, reason, err)
			_ = sess.Exit(2)
			return
		}
		if req.follow {
			lease.Release()
		}
		exitCode, err := a.handleLogsSession(sess, actor, req)
		finalize(actor, true, reason, err)
		if exitCode == 0 && err != nil {
			exitCode = 1
		}
		_ = sess.Exit(exitCode)
		return
	}
	if args[0] == "export" {
		exitCode, err := a.handleExportSession(sess, actor, args)
		finalize(actor, true, reason, err)
		if exitCode == 0 && err != nil {
			exitCode = 1
		}
		_ = sess.Exit(exitCode)
		return
	}
	if args[0] == "import" {
		exitCode, err := a.handleImportSession(sess, actor, args)
		finalize(actor, true, reason, err)
		if exitCode == 0 && err != nil {
			exitCode = 1
		}
		_ = sess.Exit(exitCode)
		return
	}
	if args[0] == "snapshot" {
		result, err := a.cmdSnapshot(sess.Context(), args, lease)
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
	case "backup":
		return a.cmdBackup(ctx, actor, args)
	case "list":
		return a.cmdList(ctx, actor)
	case "inspect":
		return a.cmdInspect(ctx, actor, args)
	case "restore":
		return a.cmdRestore(ctx, actor, args)
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

	unlock := a.lockInstance(name)
	defer unlock()

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
		format.BinarySize(effectiveInstanceRootFSSizeBytes(inst)),
	)
	return commandResult{stdout: stdout, exitCode: 0}, nil
}

func (a *App) cmdBackup(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	action, name, err := parseBackupArgs(args)
	if err != nil {
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}
	if _, err := a.lookupVisibleInstance(ctx, actor, name); err != nil {
		return missingInstanceResult("backup", name, err)
	}

	switch action {
	case "create":
		unlock := a.lockInstance(name)
		defer unlock()

		if _, err := a.lookupVisibleInstance(ctx, actor, name); err != nil {
			return missingInstanceResult("backup", name, err)
		}

		info, err := a.provisioner.CreateBackup(ctx, name)
		if err != nil {
			return commandResult{stderr: fmt.Sprintf("backup %s: %v\n", name, err), exitCode: 1}, err
		}
		stdout := fmt.Sprintf(
			"backup-created: %s\nbackup-id: %s\ncreated-at: %s\nrootfs-size: %s\npath: %s\n",
			name,
			info.ID,
			info.CreatedAt.Format(time.RFC3339),
			format.BinarySize(info.RootFSSizeBytes),
			info.Path,
		)
		return commandResult{stdout: stdout, exitCode: 0}, nil
	case "list":
		backups, err := a.provisioner.ListBackups(ctx, name)
		if err != nil {
			return commandResult{stderr: fmt.Sprintf("backup list %s: %v\n", name, err), exitCode: 1}, err
		}
		if len(backups) == 0 {
			return commandResult{stdout: fmt.Sprintf("no backups for %s\n", name), exitCode: 0}, nil
		}

		rows := make([][]string, 0, len(backups))
		for _, backup := range backups {
			logs := make([]string, 0, 2)
			if backup.HasSerialLog {
				logs = append(logs, "serial")
			}
			if backup.HasFirecrackerLog {
				logs = append(logs, "firecracker")
			}
			rows = append(rows, []string{
				backup.ID,
				backup.CreatedAt.Format(time.RFC3339),
				format.BinarySize(backup.RootFSSizeBytes),
				strconv.FormatInt(backup.VCPUCount, 10),
				fmt.Sprintf("%d MiB", backup.MemoryMiB),
				strings.Join(logs, ","),
			})
		}

		tableOutput, err := renderTextTable([]string{"ID", "Created At", "RootFS Size", "VCPUs", "Memory", "Logs"}, rows)
		if err != nil {
			return commandResult{stderr: fmt.Sprintf("render backup list: %v\n", err), exitCode: 1}, err
		}
		return commandResult{stdout: fmt.Sprintf("backups for %s:\n%s", name, tableOutput), exitCode: 0}, nil
	default:
		return commandResult{stderr: backupUsage() + "\n", exitCode: 2}, errors.New(backupUsage())
	}
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

	rows := make([][]string, 0, len(instances))
	for _, inst := range instances {
		rows = append(rows, []string{
			inst.Name,
			string(inst.State),
			fmt.Sprintf("%d", effectiveInstanceVCPUCount(inst, a.cfg)),
			format.BinarySize(effectiveInstanceMemoryMiB(inst, a.cfg) * mib),
			format.BinarySize(effectiveInstanceRootFSSizeBytes(inst)),
			inst.TailscaleIP,
			inst.TailscaleName,
		})
	}

	tableOutput, err := renderTextTable([]string{"Name", "State", "VCPUs", "Memory", "RootFS Size", "Tailscale IP", "Tailscale Name"}, rows)
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("render instance list: %v\n", err), exitCode: 1}, err
	}
	return commandResult{stdout: tableOutput, exitCode: 0}, nil
}

func renderTextTable(headers []string, rows [][]string) (string, error) {
	var b bytes.Buffer
	displayHeaders := make([]string, len(headers))
	for i, header := range headers {
		displayHeaders[i] = strings.ToUpper(header)
	}
	table := tablewriter.NewTable(&b, tablewriter.WithHeaderAutoFormat(tw.Off))
	table.Header(displayHeaders)
	if err := table.Bulk(rows); err != nil {
		return "", err
	}
	if err := table.Render(); err != nil {
		return "", err
	}
	return b.String(), nil
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
		b.WriteString(fmt.Sprintf("rootfs-size: %s\n", format.BinarySize(size)))
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
	b.WriteString(fmt.Sprintf("logs-serial: ssh %s logs %s serial\n", a.cfg.Hostname, inst.Name))
	b.WriteString(fmt.Sprintf("logs-firecracker: ssh %s logs %s firecracker\n", a.cfg.Hostname, inst.Name))
	if hint := inspectDebugHint(inst); hint != "" {
		b.WriteString(fmt.Sprintf("debug-hint: %s\n", hint))
	}
	b.WriteString("events:\n")
	for _, evt := range events {
		b.WriteString(fmt.Sprintf("- %s [%s] %s\n", evt.CreatedAt.Format(time.RFC3339), evt.Type, evt.Message))
	}
	return commandResult{stdout: b.String(), exitCode: 0}, nil
}

func (a *App) cmdLogsRequest(ctx context.Context, actor model.Actor, req logsRequest) (commandResult, error) {
	inst, err := a.lookupVisibleInstance(ctx, actor, req.name)
	if err != nil {
		return missingInstanceResult("logs", req.name, err)
	}

	stdout, err := formatLogOutput(inst, req.target)
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("logs %s: %v\n", req.name, err), exitCode: 1}, err
	}
	return commandResult{stdout: stdout, exitCode: 0}, nil
}

func (a *App) handleLogsSession(sess gssh.Session, actor model.Actor, req logsRequest) (int, error) {
	if !req.follow {
		result, err := a.cmdLogsRequest(sess.Context(), actor, req)
		if result.stdout != "" {
			_, _ = io.WriteString(sess, result.stdout)
		}
		if result.stderr != "" {
			_, _ = io.WriteString(sess.Stderr(), result.stderr)
		}
		return result.exitCode, err
	}

	inst, err := a.lookupVisibleInstance(sess.Context(), actor, req.name)
	if err != nil {
		result, err := missingInstanceResult("logs", req.name, err)
		if result.stderr != "" {
			_, _ = io.WriteString(sess.Stderr(), result.stderr)
		}
		return result.exitCode, err
	}

	err = streamLogOutput(sess.Context(), sess, inst, req.target, func() error {
		_, err := sess.SendRequest("keepalive@openssh.com", true, nil)
		if sess.Context().Err() != nil {
			return nil
		}
		return err
	})
	if err == nil || sess.Context().Err() != nil {
		return 0, nil
	}
	wrapped := fmt.Errorf("logs %s: %w", req.name, err)
	_, _ = io.WriteString(sess.Stderr(), wrapped.Error()+"\n")
	return 1, wrapped
}

func (a *App) handleExportSession(sess gssh.Session, actor model.Actor, args []string) (int, error) {
	name, err := parseExportArgs(args)
	if err != nil {
		_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
		return 2, err
	}

	unlock := a.lockInstance(name)
	defer unlock()

	if _, err := a.lookupVisibleInstance(sess.Context(), actor, name); err != nil {
		result, err := missingInstanceResult("export", name, err)
		if result.stderr != "" {
			_, _ = io.WriteString(sess.Stderr(), result.stderr)
		}
		return result.exitCode, err
	}

	if _, err := a.provisioner.ExportInstance(sess.Context(), name, sess); err != nil {
		wrapped := fmt.Errorf("export %s: %w", name, err)
		_, _ = io.WriteString(sess.Stderr(), wrapped.Error()+"\n")
		return 1, wrapped
	}
	return 0, nil
}

func (a *App) handleImportSession(sess gssh.Session, actor model.Actor, args []string) (int, error) {
	if err := parseImportArgs(args); err != nil {
		_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
		return 2, err
	}

	artifactInfo, stream, err := provision.PeekPortableArtifactInfo(sess)
	if err != nil {
		wrapped := fmt.Errorf("import: %w", err)
		_, _ = io.WriteString(sess.Stderr(), wrapped.Error()+"\n")
		return 1, wrapped
	}

	unlock := a.lockInstance(artifactInfo.Name)
	defer unlock()

	reporter := newImportProgressReporter(sess.Stderr(), 350*time.Millisecond)
	inst, info, err := a.provisioner.ImportInstance(sess.Context(), actor, stream, reporter.Update)
	reporter.Close(err == nil)
	if err != nil {
		wrapped := fmt.Errorf("import: %w", err)
		_, _ = io.WriteString(sess.Stderr(), wrapped.Error()+"\n")
		return 1, wrapped
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("imported: %s\n", inst.Name))
	if info.Name != "" && info.Name != inst.Name {
		b.WriteString(fmt.Sprintf("source-name: %s\n", info.Name))
	}
	b.WriteString(fmt.Sprintf("state: %s\n", inst.State))
	b.WriteString(fmt.Sprintf("rootfs-size: %s\n", format.BinarySize(effectiveInstanceRootFSSizeBytes(inst))))
	b.WriteString(fmt.Sprintf("exported-at: %s\n", info.ExportedAt.Format(time.RFC3339)))
	if inst.TailscaleName != "" {
		b.WriteString(fmt.Sprintf("tailscale-name: %s\n", inst.TailscaleName))
	}
	if inst.TailscaleIP != "" {
		b.WriteString(fmt.Sprintf("tailscale-ip: %s\n", inst.TailscaleIP))
	}
	_, _ = io.WriteString(sess, b.String())
	return 0, nil
}

type importProgressReporter struct {
	mu           sync.Mutex
	w            io.Writer
	interval     time.Duration
	current      provision.ImportProgress
	haveProgress bool
	lastLen      int
	rendered     bool
	stopOnce     sync.Once
	stopCh       chan struct{}
	doneCh       chan struct{}
}

func newImportProgressReporter(w io.Writer, interval time.Duration) *importProgressReporter {
	r := &importProgressReporter{
		w:        w,
		interval: interval,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	go r.loop()
	return r
}

func (r *importProgressReporter) loop() {
	defer close(r.doneCh)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.render(false)
		case <-r.stopCh:
			return
		}
	}
}

func (r *importProgressReporter) Update(progress provision.ImportProgress) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.current = progress
	r.haveProgress = true
	r.mu.Unlock()
}

func (r *importProgressReporter) Close(success bool) {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		close(r.stopCh)
		<-r.doneCh
		if success {
			r.render(true)
			return
		}
		r.endLine()
	})
}

func (r *importProgressReporter) render(final bool) {
	r.mu.Lock()
	if !r.haveProgress {
		r.mu.Unlock()
		return
	}
	line := formatImportProgress(r.current)
	padding := ""
	if diff := r.lastLen - len(line); diff > 0 {
		padding = strings.Repeat(" ", diff)
	}
	r.lastLen = len(line)
	r.rendered = true
	r.mu.Unlock()

	if final {
		_, _ = io.WriteString(r.w, "\r"+line+padding+"\n")
		return
	}
	_, _ = io.WriteString(r.w, "\r"+line+padding)
}

func (r *importProgressReporter) endLine() {
	r.mu.Lock()
	rendered := r.rendered
	r.mu.Unlock()
	if rendered {
		_, _ = io.WriteString(r.w, "\n")
	}
}

func formatImportProgress(progress provision.ImportProgress) string {
	completed := max(progress.CompletedBytes, int64(0))
	total := max(progress.TotalBytes, int64(0))
	percent := int64(100)
	if total > 0 {
		if completed > total {
			completed = total
		}
		percent = (completed * 100) / total
	}
	return fmt.Sprintf(
		"import %s %s / %s (%d%%)",
		progress.Name,
		format.BinarySize(completed),
		format.BinarySize(total),
		percent,
	)
}

func helpResult() commandResult {
	var b strings.Builder
	b.WriteString("usage: ssh srv <command>\n\n")

	type helpEntry struct {
		command     string
		description string
	}

	groups := []struct {
		name    string
		entries []helpEntry
	}{
		{
			name: "instances",
			entries: []helpEntry{
				{"new <name>", "Create a new microvm instance"},
				{"list", "List all instances"},
				{"inspect <name>", "Show instance details and recent events"},
				{"start <name>", "Start a stopped instance"},
				{"stop <name>", "Stop a running instance"},
				{"restart <name>", "Restart an instance"},
				{"resize <name>", "Change instance resources"},
				{"delete <name>", "Delete an instance"},
			},
		},
		{
			name: "backup",
			entries: []helpEntry{
				{"backup create <name>", "Create a backup of an instance"},
				{"backup list <name>", "List backups for an instance"},
				{"restore <name> <backup-id>", "Restore an instance from a backup"},
				{"export <name>", "Export instance as a portable archive to stdout"},
				{"import", "Import instance from stdin"},
			},
		},
		{
			name: "diagnostics",
			entries: []helpEntry{
				{"logs <name> [target]", "View instance logs (serial|firecracker)"},
				{"logs -f <name> <target>", "Follow logs in real time"},
			},
		},
		{
			name: "admin",
			entries: []helpEntry{
				{"snapshot create", "Create a read-only btrfs data snapshot"},
			},
		},
	}

	for _, group := range groups {
		b.WriteString(group.name + "\n")
		rows := make([][]string, 0, len(group.entries))
		for _, e := range group.entries {
			rows = append(rows, []string{e.command, e.description})
		}
		tableOutput, err := renderTextTable([]string{"command", "description"}, rows)
		if err != nil {
			for _, e := range group.entries {
				b.WriteString(fmt.Sprintf("  %-35s %s\n", e.command, e.description))
			}
		} else {
			b.WriteString(tableOutput)
		}
		b.WriteString("\n")
	}

	b.WriteString("new and resize options:\n")
	optionRows := [][]string{
		{"--cpus N", "Number of vCPUs"},
		{"--ram SIZE", "Memory (e.g. 512m, 2g)"},
		{"--rootfs-size SIZE", "Root filesystem size (e.g. 4g, 10g)"},
	}
	optionOutput, err := renderTextTable([]string{"flag", "description"}, optionRows)
	if err != nil {
		b.WriteString("  --cpus N             Number of vCPUs\n")
		b.WriteString("  --ram SIZE           Memory (e.g. 512m, 2g)\n")
		b.WriteString("  --rootfs-size SIZE   Root filesystem size (e.g. 4g, 10g)\n")
	} else {
		b.WriteString(optionOutput)
	}

	return commandResult{stdout: b.String(), exitCode: 0}
}

func parseLogsArgs(args []string) (logsRequest, error) {
	if len(args) < 2 {
		return logsRequest{}, errors.New(logsUsage())
	}

	req := logsRequest{target: logTargetAll}
	for _, arg := range args[1:] {
		switch arg {
		case "-f", "--follow":
			if req.follow {
				return logsRequest{}, fmt.Errorf("%s specified more than once\n%s", arg, logsUsage())
			}
			req.follow = true
		case string(logTargetSerial):
			if req.target != logTargetAll {
				return logsRequest{}, fmt.Errorf("unexpected argument %q\n%s", arg, logsUsage())
			}
			req.target = logTargetSerial
		case string(logTargetFirecracker):
			if req.target != logTargetAll {
				return logsRequest{}, fmt.Errorf("unexpected argument %q\n%s", arg, logsUsage())
			}
			req.target = logTargetFirecracker
		default:
			if strings.HasPrefix(arg, "-") {
				return logsRequest{}, fmt.Errorf("unknown option %q\n%s", arg, logsUsage())
			}
			if req.name != "" {
				return logsRequest{}, fmt.Errorf("unexpected argument %q\n%s", arg, logsUsage())
			}
			req.name = arg
		}
	}

	if req.name == "" {
		return logsRequest{}, errors.New(logsUsage())
	}
	if req.follow && req.target == logTargetAll {
		return logsRequest{}, fmt.Errorf("follow requires an explicit log target\n%s", logsUsage())
	}
	return req, nil
}

func logsUsage() string {
	return "usage: logs [-f|--follow] <name> [serial|firecracker]"
}

func formatLogSection(label, path string) (string, error) {
	var b strings.Builder
	if _, _, err := writeLogSection(logContentWriter(&b, label), label, path); err != nil {
		return "", err
	}
	return b.String(), nil
}

func streamLogOutput(ctx context.Context, w io.Writer, inst model.Instance, target logTarget, keepAlive func() error) error {
	label, path, err := resolveSingleLogTarget(inst, target)
	if err != nil {
		return err
	}
	w = logContentWriter(w, label)

	_, offset, err := writeLogSection(w, label, path)
	if err != nil {
		return err
	}
	return followLogFile(ctx, w, path, offset, keepAlive)
}

func resolveSingleLogTarget(inst model.Instance, target logTarget) (string, string, error) {
	switch target {
	case logTargetSerial:
		return "serial", inst.SerialLogPath, nil
	case logTargetFirecracker:
		return "firecracker", inst.LogPath, nil
	default:
		return "", "", errors.New("follow requires an explicit log target")
	}
}

func writeLogSection(w io.Writer, label, path string) ([]string, int64, error) {
	lines, offset, exists, err := readLastLines(path, defaultLogTailLines)
	if err != nil {
		return nil, 0, err
	}
	if _, err := io.WriteString(w, fmt.Sprintf("%s-log: %s\n", label, path)); err != nil {
		return nil, 0, err
	}
	switch {
	case !exists:
		_, err = io.WriteString(w, "(log file has not been created yet)\n")
	case len(lines) == 0:
		_, err = io.WriteString(w, "(log is empty)\n")
	default:
		for _, line := range lines {
			if _, err = io.WriteString(w, line); err != nil {
				break
			}
		}
	}
	if err != nil {
		return nil, 0, err
	}
	return lines, offset, nil
}

func followLogFile(ctx context.Context, w io.Writer, path string, offset int64, keepAlive func() error) error {
	pollTicker := time.NewTicker(logFollowPollInterval)
	defer pollTicker.Stop()
	keepAliveTicker := time.NewTicker(logFollowKeepAliveInterval)
	defer keepAliveTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-pollTicker.C:
			if err := writeLogUpdates(w, path, &offset); err != nil {
				return err
			}
		case <-keepAliveTicker.C:
			if keepAlive == nil {
				continue
			}
			if err := keepAlive(); err != nil {
				return err
			}
		}
	}
}

func writeLogUpdates(w io.Writer, path string, offset *int64) error {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			*offset = 0
			return nil
		}
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}
	if *offset > info.Size() {
		*offset = 0
	}
	if _, err := file.Seek(*offset, io.SeekStart); err != nil {
		return err
	}

	written, err := io.Copy(w, file)
	*offset += written
	return err
}

func readLastLines(path string, limit int) ([]string, int64, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, false, nil
		}
		return nil, 0, false, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLogChunkBytes)
	scanner.Split(scanLogChunks)

	lines := make([]string, 0, limit)
	var offset int64
	for scanner.Scan() {
		chunk := scanner.Text()
		offset += int64(len(chunk))
		if len(lines) == limit {
			copy(lines, lines[1:])
			lines[len(lines)-1] = chunk
			continue
		}
		lines = append(lines, chunk)
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, false, err
	}
	return lines, offset, true, nil
}

func scanLogChunks(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i+1], nil
	}
	if len(data) >= maxLogChunkBytes {
		return maxLogChunkBytes, data[:maxLogChunkBytes], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func logContentWriter(w io.Writer, label string) io.Writer {
	if label == string(logTargetSerial) {
		return &terminalSafeWriter{dst: w}
	}
	return w
}

func (w *terminalSafeWriter) Write(p []byte) (int, error) {
	buf := make([]byte, 0, len(p))
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		if _, err := w.dst.Write(buf); err != nil {
			return err
		}
		buf = buf[:0]
		return nil
	}

	for _, b := range p {
		switch w.state {
		case terminalWriterStateText:
			switch {
			case b == 0x1b:
				if err := flush(); err != nil {
					return 0, err
				}
				w.state = terminalWriterStateEscape
			case b < 0x20 || b == 0x7f:
				switch b {
				case '\n', '\r', '\t':
					buf = append(buf, b)
				}
			default:
				buf = append(buf, b)
			}
		case terminalWriterStateEscape:
			switch b {
			case '[':
				w.state = terminalWriterStateCSI
			case ']':
				w.state = terminalWriterStateOSC
			case 'P', 'X', '^', '_':
				w.state = terminalWriterStateEscapeString
			default:
				w.state = terminalWriterStateText
			}
		case terminalWriterStateCSI:
			if b >= 0x40 && b <= 0x7e {
				w.state = terminalWriterStateText
			}
		case terminalWriterStateOSC:
			switch b {
			case 0x07:
				w.state = terminalWriterStateText
			case 0x1b:
				w.state = terminalWriterStateOSCEscape
			}
		case terminalWriterStateOSCEscape:
			if b == '\\' {
				w.state = terminalWriterStateText
			} else if b != 0x1b {
				w.state = terminalWriterStateOSC
			}
		case terminalWriterStateEscapeString:
			if b == 0x1b {
				w.state = terminalWriterStateEscapeStringEscape
			}
		case terminalWriterStateEscapeStringEscape:
			if b == '\\' {
				w.state = terminalWriterStateText
			} else if b != 0x1b {
				w.state = terminalWriterStateEscapeString
			}
		}
	}

	if err := flush(); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (a *App) cmdRestore(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	name, backupID, err := parseRestoreArgs(args)
	if err != nil {
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	unlock := a.lockInstance(name)
	defer unlock()

	if _, err := a.lookupVisibleInstance(ctx, actor, name); err != nil {
		return missingInstanceResult("restore", name, err)
	}

	inst, info, err := a.provisioner.RestoreBackup(ctx, name, backupID)
	if err != nil {
		stderr := fmt.Sprintf("restore %s: %v\n", name, err)
		if inst.Name != "" {
			stderr += instanceDebugHints(a.cfg.Hostname, inst)
		}
		return commandResult{stderr: stderr, exitCode: 1}, err
	}

	stdout := fmt.Sprintf(
		"restored: %s\nbackup-id: %s\nstate: %s\nrootfs-size: %s\nbackup-created-at: %s\n",
		inst.Name,
		info.ID,
		inst.State,
		format.BinarySize(effectiveInstanceRootFSSizeBytes(inst)),
		info.CreatedAt.Format(time.RFC3339),
	)
	return commandResult{stdout: stdout, exitCode: 0}, nil
}

func (a *App) cmdStart(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	if len(args) != 2 {
		err := errors.New("usage: start <name>")
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	unlock := a.lockInstance(args[1])
	defer unlock()

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

	unlock := a.lockInstance(args[1])
	defer unlock()

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

	unlock := a.lockInstance(args[1])
	defer unlock()

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

	unlock := a.lockInstance(args[1])
	defer unlock()

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
	if command == "snapshot" {
		if !a.isAdmin(actor) {
			return false, fmt.Sprintf("%s is not in SRV_ADMIN_USERS", actor.UserLogin)
		}
		if len(a.cfg.AllowedUsers) == 0 {
			return true, fmt.Sprintf("%s allowed to run snapshot as admin", actor.UserLogin)
		}
		for _, user := range a.cfg.AllowedUsers {
			if strings.EqualFold(user, actor.UserLogin) {
				return true, fmt.Sprintf("%s allowed to run snapshot as admin", actor.UserLogin)
			}
		}
		return false, fmt.Sprintf("%s is not in SRV_ALLOWED_USERS", actor.UserLogin)
	}
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

func parseBackupArgs(args []string) (string, string, error) {
	if len(args) != 3 {
		return "", "", errors.New(backupUsage())
	}
	switch args[1] {
	case "create", "list":
		return args[1], args[2], nil
	default:
		return "", "", fmt.Errorf("unknown backup action %q\n%s", args[1], backupUsage())
	}
}

func parseRestoreArgs(args []string) (string, string, error) {
	if len(args) != 3 {
		return "", "", errors.New(restoreUsage())
	}
	if strings.TrimSpace(args[2]) == "" {
		return "", "", errors.New(restoreUsage())
	}
	return args[1], args[2], nil
}

func parseExportArgs(args []string) (string, error) {
	if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
		return "", errors.New(exportUsage())
	}
	return args[1], nil
}

func parseImportArgs(args []string) error {
	if len(args) != 1 {
		return errors.New(importUsage())
	}
	return nil
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

func exportUsage() string {
	return "usage: export <name>"
}

func importUsage() string {
	return "usage: import"
}

func newUsage() string {
	return "usage: new <name> [--cpus N] [--ram SIZE] [--rootfs-size SIZE]"
}

func resizeUsage() string {
	return "usage: resize <name> [--cpus N] [--ram SIZE] [--rootfs-size SIZE]"
}

func backupUsage() string {
	return "usage: backup <create|list> <name>"
}

func restoreUsage() string {
	return "usage: restore <name> <backup-id>"
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
	return fmt.Sprintf("inspect: ssh %s inspect %s\nlogs-serial: ssh %s logs %s serial\nlogs-firecracker: ssh %s logs %s firecracker\n", hostname, inst.Name, hostname, inst.Name, hostname, inst.Name)
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
