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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	gssh "github.com/gliderlabs/ssh"
	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"
	"tailscale.com/client/local"
	"tailscale.com/tsnet"

	"srv/internal/config"
	"srv/internal/format"
	"srv/internal/host"
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
	zenGateway  *zenGatewayManager
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

type outputFormat uint8

const (
	outputFormatText outputFormat = iota
	outputFormatJSON
)

type commandRequest struct {
	args   []string
	format outputFormat
}

type commandActionJSON struct {
	Action   string              `json:"action"`
	Instance instanceSummaryJSON `json:"instance"`
}

type instanceSummaryJSON struct {
	Name            string `json:"name"`
	State           string `json:"state"`
	VCPUCount       int64  `json:"vcpu_count,omitempty"`
	MemoryMiB       int64  `json:"memory_mib,omitempty"`
	RootFSSizeBytes int64  `json:"rootfs_size_bytes,omitempty"`
	TailscaleName   string `json:"tailscale_name,omitempty"`
	TailscaleIP     string `json:"tailscale_ip,omitempty"`
	Connect         string `json:"connect,omitempty"`
}

type inspectResponseJSON struct {
	Instance inspectInstanceJSON `json:"instance"`
	Events   []inspectEventJSON  `json:"events"`
}

type inspectInstanceJSON struct {
	Name            string          `json:"name"`
	State           string          `json:"state"`
	CreatedByUser   string          `json:"created_by_user,omitempty"`
	CreatedByNode   string          `json:"created_by_node,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	VCPUCount       int64           `json:"vcpu_count,omitempty"`
	MemoryMiB       int64           `json:"memory_mib,omitempty"`
	RootFSPath      string          `json:"rootfs_path"`
	RootFSSizeBytes int64           `json:"rootfs_size_bytes,omitempty"`
	FirecrackerPID  int             `json:"firecracker_pid"`
	TapDevice       string          `json:"tap_device,omitempty"`
	NetworkCIDR     string          `json:"network_cidr,omitempty"`
	HostIP          string          `json:"host_ip,omitempty"`
	GuestIP         string          `json:"guest_ip,omitempty"`
	ZenGateway      string          `json:"zen_gateway,omitempty"`
	TailscaleName   string          `json:"tailscale_name,omitempty"`
	TailscaleIP     string          `json:"tailscale_ip,omitempty"`
	LastError       string          `json:"last_error,omitempty"`
	DeletedAt       *time.Time      `json:"deleted_at,omitempty"`
	Logs            inspectLogsJSON `json:"logs"`
	DebugHint       string          `json:"debug_hint,omitempty"`
}

type inspectLogsJSON struct {
	SerialCommand      string `json:"serial_command"`
	FirecrackerCommand string `json:"firecracker_command"`
}

type inspectEventJSON struct {
	CreatedAt time.Time `json:"created_at"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
}

type backupJSON struct {
	ID                string    `json:"id"`
	Name              string    `json:"name,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	Path              string    `json:"path,omitempty"`
	RootFSSizeBytes   int64     `json:"rootfs_size_bytes,omitempty"`
	VCPUCount         int64     `json:"vcpu_count,omitempty"`
	MemoryMiB         int64     `json:"memory_mib,omitempty"`
	HasSerialLog      bool      `json:"has_serial_log"`
	HasFirecrackerLog bool      `json:"has_firecracker_log"`
}

type backupCreateResponseJSON struct {
	Action   string     `json:"action"`
	Instance string     `json:"instance"`
	Backup   backupJSON `json:"backup"`
}

type backupListResponseJSON struct {
	Instance string       `json:"instance"`
	Backups  []backupJSON `json:"backups"`
}

type restoreResponseJSON struct {
	Action   string              `json:"action"`
	Instance instanceSummaryJSON `json:"instance"`
	Backup   backupJSON          `json:"backup"`
}

type statusResponseJSON = host.CapacitySummary
type statusInstancesJSON = host.CapacityInstances
type statusResourceJSON = host.CapacityResource

type logTarget string

type logsRequest struct {
	name   string
	target logTarget
	follow bool
}

type topRequest struct {
	interval time.Duration
}

type topSnapshot struct {
	capturedAt time.Time
	instances  []topInstanceSnapshot
}

type topInstanceSnapshot struct {
	Name               string
	State              string
	VCPUCount          int64
	CPUUsageUsec       uint64
	MemoryCurrentBytes int64
	MemoryLimitBytes   int64
	DiskAllocatedBytes int64
	DiskLimitBytes     int64
	NetRXBytes         uint64
	NetTXBytes         uint64
	Uptime             time.Duration
	HasLiveMetrics     bool
	HasNetStats        bool
	HasUptime          bool
}

type topRenderedRow struct {
	Values  []string
	Styles  []topCellStyle
	SortCPU float64
	HasCPU  bool
	Name    string
}

type topCellStyle uint8

const (
	logTargetAll         logTarget = "all"
	logTargetSerial      logTarget = "serial"
	logTargetFirecracker logTarget = "firecracker"
	defaultLogTailLines            = 40
	maxLogChunkBytes               = 1024 * 1024
	mib                            = int64(1024 * 1024)
	defaultTopInterval             = time.Second
)

const (
	topCellStyleNone topCellStyle = iota
	topCellStyleOK
	topCellStyleWarning
	topCellStyleCritical
	topCellStyleMuted
	topCellStyleHeader
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

func parseCommandRequest(args []string) (commandRequest, error) {
	req := commandRequest{args: args, format: outputFormatText}
	if len(args) == 0 || args[0] != "--json" {
		return req, nil
	}
	if len(args) == 1 {
		return commandRequest{}, errors.New("usage: ssh srv [--json] <command>")
	}
	req.args = args[1:]
	req.format = outputFormatJSON
	return req, nil
}

func jsonResult(payload any) (commandResult, error) {
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("marshal json output: %v\n", err), exitCode: 1}, err
	}
	return commandResult{stdout: string(encoded) + "\n", exitCode: 0}, nil
}

func unsupportedJSONResult(command string) (commandResult, error) {
	err := fmt.Errorf("%s does not support --json", command)
	return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
}

func maybeUnsupportedJSONCommand(command string, format outputFormat) (commandResult, error, bool) {
	if format != outputFormatJSON {
		return commandResult{}, nil, false
	}
	switch command {
	case "logs", "export", "import", "snapshot", "help", "top":
		result, err := unsupportedJSONResult(command)
		return result, err, true
	default:
		return commandResult{}, nil, false
	}
}

func instanceSummaryPayload(cfg config.Config, inst model.Instance, includeConnect bool) instanceSummaryJSON {
	payload := instanceSummaryJSON{
		Name:            inst.Name,
		State:           inst.State,
		VCPUCount:       effectiveInstanceVCPUCount(inst, cfg),
		MemoryMiB:       effectiveInstanceMemoryMiB(inst, cfg),
		RootFSSizeBytes: effectiveInstanceRootFSSizeBytes(inst),
		TailscaleName:   inst.TailscaleName,
		TailscaleIP:     inst.TailscaleIP,
	}
	if includeConnect {
		payload.Connect = fmt.Sprintf("ssh root@%s", inst.Name)
	}
	return payload
}

func backupPayload(info provision.BackupInfo) backupJSON {
	return backupJSON{
		ID:                info.ID,
		Name:              info.Name,
		CreatedAt:         info.CreatedAt,
		Path:              info.Path,
		RootFSSizeBytes:   info.RootFSSizeBytes,
		VCPUCount:         info.VCPUCount,
		MemoryMiB:         info.MemoryMiB,
		HasSerialLog:      info.HasSerialLog,
		HasFirecrackerLog: info.HasFirecrackerLog,
	}
}

func (a *App) inspectPayload(inst model.Instance, events []model.InstanceEvent) inspectResponseJSON {
	debugHint := inspectDebugHint(inst)
	payload := inspectResponseJSON{
		Instance: inspectInstanceJSON{
			Name:            inst.Name,
			State:           inst.State,
			CreatedByUser:   inst.CreatedByUser,
			CreatedByNode:   inst.CreatedByNode,
			CreatedAt:       inst.CreatedAt,
			UpdatedAt:       inst.UpdatedAt,
			VCPUCount:       effectiveInstanceVCPUCount(inst, a.cfg),
			MemoryMiB:       effectiveInstanceMemoryMiB(inst, a.cfg),
			RootFSPath:      inst.RootFSPath,
			RootFSSizeBytes: effectiveInstanceRootFSSizeBytes(inst),
			FirecrackerPID:  inst.FirecrackerPID,
			TapDevice:       inst.TapDevice,
			NetworkCIDR:     inst.NetworkCIDR,
			HostIP:          inst.HostAddr,
			GuestIP:         inst.GuestAddr,
			ZenGateway:      a.zenGatewayBaseURL(inst),
			TailscaleName:   inst.TailscaleName,
			TailscaleIP:     inst.TailscaleIP,
			LastError:       inst.LastError,
			DeletedAt:       inst.DeletedAt,
			Logs: inspectLogsJSON{
				SerialCommand:      fmt.Sprintf("ssh %s logs %s serial", a.cfg.Hostname, inst.Name),
				FirecrackerCommand: fmt.Sprintf("ssh %s logs %s firecracker", a.cfg.Hostname, inst.Name),
			},
			DebugHint: debugHint,
		},
		Events: make([]inspectEventJSON, 0, len(events)),
	}
	for _, evt := range events {
		payload.Events = append(payload.Events, inspectEventJSON{
			CreatedAt: evt.CreatedAt,
			Type:      evt.Type,
			Message:   evt.Message,
		})
	}
	return payload
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

	zenGateway, err := newZenGatewayManager(cfg, logger)
	if err != nil {
		return nil, err
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

	return &App{cfg: cfg, log: logger, store: st, provisioner: prov, zenGateway: zenGateway}, nil
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
		PtyCallback: func(ctx gssh.Context, pty gssh.Pty) bool { return true },
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
		if a.zenGateway != nil {
			a.zenGateway.Close()
		}
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
	err := a.provisioner.RestoreInstances(ctx)
	a.mu.Unlock()
	a.syncZenGatewayBestEffort()
	if err != nil && ctx.Err() == nil {
		a.log.Error("restore instances on startup", "err", err)
	}
}

func (a *App) handleSession(sess gssh.Session) {
	started := time.Now().UTC()
	rawArgs := sess.Command()
	command := ""
	if len(rawArgs) > 0 {
		command = rawArgs[0]
	}
	lease, err := a.beginCommand()
	if err != nil {
		_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
		_ = sess.Exit(1)
		return
	}
	defer lease.Release()
	argsJSON, _ := json.Marshal(rawArgs)
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

	if len(rawArgs) == 0 {
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

	req, err := parseCommandRequest(rawArgs)
	if err != nil {
		_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
		finalize(actor, false, "invalid request", err)
		_ = sess.Exit(2)
		return
	}
	command = req.args[0]
	audit.Command = command

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
	if result, err, rejected := maybeUnsupportedJSONCommand(req.args[0], req.format); rejected {
		if result.stderr != "" {
			_, _ = io.WriteString(sess.Stderr(), result.stderr)
		}
		finalize(actor, true, reason, err)
		_ = sess.Exit(result.exitCode)
		return
	}
	if req.args[0] == "logs" {
		logsReq, err := parseLogsArgs(req.args)
		if err != nil {
			_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
			finalize(actor, true, reason, err)
			_ = sess.Exit(2)
			return
		}
		if logsReq.follow {
			lease.Release()
		}
		exitCode, err := a.handleLogsSession(sess, actor, logsReq)
		finalize(actor, true, reason, err)
		if exitCode == 0 && err != nil {
			exitCode = 1
		}
		_ = sess.Exit(exitCode)
		return
	}
	if req.args[0] == "top" {
		lease.Release()
		exitCode, err := a.handleTopSession(sess, actor, req.args)
		finalize(actor, true, reason, err)
		if exitCode == 0 && err != nil {
			exitCode = 1
		}
		_ = sess.Exit(exitCode)
		return
	}
	if req.args[0] == "export" {
		exitCode, err := a.handleExportSession(sess, actor, req.args)
		finalize(actor, true, reason, err)
		if exitCode == 0 && err != nil {
			exitCode = 1
		}
		_ = sess.Exit(exitCode)
		return
	}
	if req.args[0] == "import" {
		exitCode, err := a.handleImportSession(sess, actor, req.args)
		finalize(actor, true, reason, err)
		if exitCode == 0 && err != nil {
			exitCode = 1
		}
		_ = sess.Exit(exitCode)
		return
	}
	if req.args[0] == "snapshot" {
		result, err := a.cmdSnapshot(sess.Context(), req.args, lease)
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

	result, err := a.dispatch(sess.Context(), actor, req)
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

func (a *App) dispatch(ctx context.Context, actor model.Actor, req commandRequest) (commandResult, error) {
	switch req.args[0] {
	case "new":
		return a.cmdNew(ctx, actor, req.args, req.format)
	case "resize":
		return a.cmdResize(ctx, actor, req.args, req.format)
	case "backup":
		return a.cmdBackup(ctx, actor, req.args, req.format)
	case "list":
		return a.cmdList(ctx, actor, req.format)
	case "inspect":
		return a.cmdInspect(ctx, actor, req.args, req.format)
	case "status":
		return a.cmdStatus(ctx, actor, req.args, req.format)
	case "restore":
		return a.cmdRestore(ctx, actor, req.args, req.format)
	case "start":
		return a.cmdStart(ctx, actor, req.args, req.format)
	case "stop":
		return a.cmdStop(ctx, actor, req.args, req.format)
	case "restart":
		return a.cmdRestart(ctx, actor, req.args, req.format)
	case "delete":
		return a.cmdDelete(ctx, actor, req.args, req.format)
	case "help":
		if result, err, rejected := maybeUnsupportedJSONCommand(req.args[0], req.format); rejected {
			return result, err
		}
		return helpResult(), nil
	default:
		return commandResult{stderr: fmt.Sprintf("unknown command %q\n", req.args[0]), exitCode: 2}, fmt.Errorf("unknown command %q", req.args[0])
	}
}

func (a *App) cmdNew(ctx context.Context, actor model.Actor, args []string, outFormat outputFormat) (commandResult, error) {
	name, opts, err := parseNewArgs(args)
	if err != nil {
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	a.mu.Lock()
	defer func() {
		a.mu.Unlock()
		a.syncZenGatewayBestEffort()
	}()

	inst, err := a.provisioner.Create(ctx, name, actor, opts)
	if err != nil {
		stderr := fmt.Sprintf("create %s: %v\n", name, err)
		if inst.Name != "" {
			stderr += instanceDebugHints(a.cfg.Hostname, inst)
		}
		return commandResult{stderr: stderr, exitCode: 1}, err
	}

	if outFormat == outputFormatJSON {
		return jsonResult(commandActionJSON{Action: "created", Instance: instanceSummaryPayload(a.cfg, inst, true)})
	}
	return lifecycleReadyResult("created", inst), nil
}

func (a *App) cmdResize(ctx context.Context, actor model.Actor, args []string, outFormat outputFormat) (commandResult, error) {
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
	if outFormat == outputFormatJSON {
		return jsonResult(commandActionJSON{Action: "resized", Instance: instanceSummaryPayload(a.cfg, inst, false)})
	}
	return commandResult{stdout: stdout, exitCode: 0}, nil
}

func (a *App) cmdBackup(ctx context.Context, actor model.Actor, args []string, outFormat outputFormat) (commandResult, error) {
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
		if outFormat == outputFormatJSON {
			return jsonResult(backupCreateResponseJSON{Action: "backup-created", Instance: name, Backup: backupPayload(info)})
		}
		return commandResult{stdout: stdout, exitCode: 0}, nil
	case "list":
		backups, err := a.provisioner.ListBackups(ctx, name)
		if err != nil {
			return commandResult{stderr: fmt.Sprintf("backup list %s: %v\n", name, err), exitCode: 1}, err
		}
		if outFormat == outputFormatJSON {
			payload := backupListResponseJSON{Instance: name, Backups: make([]backupJSON, 0, len(backups))}
			for _, backup := range backups {
				payload.Backups = append(payload.Backups, backupPayload(backup))
			}
			return jsonResult(payload)
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

func (a *App) cmdList(ctx context.Context, actor model.Actor, outFormat outputFormat) (commandResult, error) {
	instances, err := a.store.ListInstances(ctx, false)
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("list instances: %v\n", err), exitCode: 1}, err
	}
	instances = a.visibleInstances(actor, instances)
	if outFormat == outputFormatJSON {
		payload := struct {
			Instances []instanceSummaryJSON `json:"instances"`
		}{Instances: make([]instanceSummaryJSON, 0, len(instances))}
		for _, inst := range instances {
			payload.Instances = append(payload.Instances, instanceSummaryPayload(a.cfg, inst, false))
		}
		return jsonResult(payload)
	}
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
		displayHeaders[i] = "\x1b[1m" + strings.ToUpper(header) + "\x1b[0m"
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

func (a *App) cmdInspect(ctx context.Context, actor model.Actor, args []string, outFormat outputFormat) (commandResult, error) {
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
	if outFormat == outputFormatJSON {
		return jsonResult(a.inspectPayload(inst, events))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\n", inst.Name)
	fmt.Fprintf(&b, "state: %s\n", inst.State)
	fmt.Fprintf(&b, "created-by: %s via %s\n", inst.CreatedByUser, inst.CreatedByNode)
	fmt.Fprintf(&b, "created-at: %s\n", inst.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "updated-at: %s\n", inst.UpdatedAt.Format(time.RFC3339))
	if vcpus := effectiveInstanceVCPUCount(inst, a.cfg); vcpus > 0 {
		fmt.Fprintf(&b, "vcpus: %d\n", vcpus)
	}
	if mem := effectiveInstanceMemoryMiB(inst, a.cfg); mem > 0 {
		fmt.Fprintf(&b, "memory: %d MiB\n", mem)
	}
	fmt.Fprintf(&b, "rootfs: %s\n", inst.RootFSPath)
	if size := effectiveInstanceRootFSSizeBytes(inst); size > 0 {
		fmt.Fprintf(&b, "rootfs-size: %s\n", format.BinarySize(size))
	}
	fmt.Fprintf(&b, "firecracker-pid: %d\n", inst.FirecrackerPID)
	fmt.Fprintf(&b, "tap-device: %s\n", inst.TapDevice)
	fmt.Fprintf(&b, "network: %s\n", inst.NetworkCIDR)
	fmt.Fprintf(&b, "host-ip: %s\n", inst.HostAddr)
	fmt.Fprintf(&b, "guest-ip: %s\n", inst.GuestAddr)
	if gatewayURL := a.zenGatewayBaseURL(inst); gatewayURL != "" {
		fmt.Fprintf(&b, "zen-gateway: %s\n", gatewayURL)
	}
	if inst.TailscaleName != "" {
		fmt.Fprintf(&b, "tailscale-name: %s\n", inst.TailscaleName)
	}
	if inst.TailscaleIP != "" {
		fmt.Fprintf(&b, "tailscale-ip: %s\n", inst.TailscaleIP)
	}
	if inst.LastError != "" {
		fmt.Fprintf(&b, "last-error: %s\n", inst.LastError)
	}
	if inst.DeletedAt != nil {
		fmt.Fprintf(&b, "deleted-at: %s\n", inst.DeletedAt.Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "logs-serial: ssh %s logs %s serial\n", a.cfg.Hostname, inst.Name)
	fmt.Fprintf(&b, "logs-firecracker: ssh %s logs %s firecracker\n", a.cfg.Hostname, inst.Name)
	if hint := inspectDebugHint(inst); hint != "" {
		fmt.Fprintf(&b, "debug-hint: %s\n", hint)
	}
	b.WriteString("events:\n")
	for _, evt := range events {
		fmt.Fprintf(&b, "- %s [%s] %s\n", evt.CreatedAt.Format(time.RFC3339), evt.Type, evt.Message)
	}
	return commandResult{stdout: b.String(), exitCode: 0}, nil
}

func (a *App) cmdStatus(ctx context.Context, actor model.Actor, args []string, outFormat outputFormat) (commandResult, error) {
	if len(args) != 1 {
		err := errors.New("usage: status")
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}
	summary, err := a.provisioner.CapacitySummary(ctx)
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("status: %v\n", err), exitCode: 1}, err
	}
	if outFormat == outputFormatJSON {
		return jsonResult(summary)
	}

	const col1Width = 12
	const valueWidth = 52

	var rows [][2]string

	rows = append(rows, [2]string{"SERVER", summary.Hostname})
	rows = append(rows, [2]string{"OS", summary.OS.Name})
	rows = append(rows, [2]string{"KERNEL", fmt.Sprintf("%s (%s)", summary.OS.Kernel, summary.OS.Arch)})

	if summary.CPU.Model != "" {
		rows = append(rows, [2]string{"CPU", summary.CPU.Model})
		coreInfo := fmt.Sprintf("%d vCPU", summary.CPU.LogicalCores)
		if summary.CPU.PhysicalCores > 0 && summary.CPU.PhysicalCores != summary.CPU.LogicalCores {
			coreInfo = fmt.Sprintf("%d vCPU / %d cores", summary.CPU.LogicalCores, summary.CPU.PhysicalCores)
		}
		rows = append(rows, [2]string{"", coreInfo})

		loadBar1m := formatStatusLoadBar(valueWidth, summary.CPU.LogicalCores, summary.CPU.Load1m)
		loadBar5m := formatStatusLoadBar(valueWidth, summary.CPU.LogicalCores, summary.CPU.Load5m)
		loadBar15m := formatStatusLoadBar(valueWidth, summary.CPU.LogicalCores, summary.CPU.Load15m)
		rows = append(rows, [2]string{"LOAD 1m", loadBar1m})
		rows = append(rows, [2]string{"LOAD 5m", loadBar5m})
		rows = append(rows, [2]string{"LOAD 15m", loadBar15m})
	}

	rows = append(rows, [2]string{"INSTANCES", formatStatusInstanceSummary(summary.Instances)})

	for _, resource := range summary.Capacity {
		label := strings.ToUpper(resource.Resource)
		pct := 0
		if resource.Budget > 0 {
			pct = int(float64(resource.Allocated) / float64(resource.Budget) * 100)
		}
		valueLine := formatStatusResourceLine(resource, pct)
		rows = append(rows, [2]string{label, valueLine})

		bar := formatStatusBar(valueWidth, resource.Allocated, resource.Budget)
		rows = append(rows, [2]string{"", bar})

		for _, detail := range resource.Details {
			rows = append(rows, [2]string{strings.ToUpper(detail.Label), detail.Value})
		}
	}

	var b strings.Builder

	b.WriteString(boxTop(col1Width, valueWidth))
	for i, row := range rows {
		label := row[0]
		value := row[1]
		if label != "" && i > 0 {
			if prevLabel := previousStatusLabel(rows, i-1); prevLabel != "" && !skipStatusSeparator(prevLabel, label) {
				b.WriteString(boxSeparator(col1Width, valueWidth))
			}
		}
		b.WriteString(boxRow(label, value, col1Width, valueWidth))
	}
	b.WriteString(boxBottom(col1Width, valueWidth))

	return commandResult{stdout: b.String(), exitCode: 0}, nil
}

func boxTop(labelWidth, valueWidth int) string {
	return "┌" + strings.Repeat("─", labelWidth+2) + "┬" + strings.Repeat("─", valueWidth+2) + "┐\n"
}

func boxBottom(labelWidth, valueWidth int) string {
	return "└" + strings.Repeat("─", labelWidth+2) + "┴" + strings.Repeat("─", valueWidth+2) + "┘\n"
}

func boxSeparator(labelWidth, valueWidth int) string {
	return "├" + strings.Repeat("─", labelWidth+2) + "┼" + strings.Repeat("─", valueWidth+2) + "┤\n"
}

func previousStatusLabel(rows [][2]string, i int) string {
	for ; i >= 0; i-- {
		if rows[i][0] != "" {
			return rows[i][0]
		}
	}
	return ""
}

func skipStatusSeparator(prevLabel, label string) bool {
	if prevLabel == "OS" && label == "KERNEL" {
		return true
	}
	if prevLabel == "CPU" && label == "LOAD 1m" {
		return true
	}
	if strings.HasPrefix(prevLabel, "LOAD ") && strings.HasPrefix(label, "LOAD ") {
		return true
	}
	if prevRank := statusStorageLabelRank(prevLabel); prevRank >= 0 {
		if labelRank := statusStorageLabelRank(label); labelRank > prevRank {
			return true
		}
	}
	return false
}

func statusStorageLabelRank(label string) int {
	switch label {
	case "DISK":
		return 0
	case "BTRFS":
		return 1
	case "MDADM":
		return 2
	default:
		return -1
	}
}

func boxRow(label, value string, labelWidth, valueWidth int) string {
	valueRunes := []rune(value)
	if len(valueRunes) > valueWidth {
		if valueWidth <= 3 {
			value = string([]rune("...")[:valueWidth])
		} else {
			value = string(valueRunes[:valueWidth-3]) + "..."
		}
	}
	if label == "" {
		return fmt.Sprintf("│ %-*s│ %-*s │\n", labelWidth+1, "", valueWidth, value)
	}
	boldLabel := "\x1b[1m" + label + "\x1b[0m"
	labelPad := strings.Repeat(" ", labelWidth-utf8.RuneCountInString(label)+1)
	return fmt.Sprintf("│ %s%s│ %-*s │\n", boldLabel, labelPad, valueWidth, value)
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

func (a *App) handleTopSession(sess gssh.Session, actor model.Actor, args []string) (int, error) {
	req, err := parseTopArgs(args)
	if err != nil {
		_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
		return 2, err
	}
	if err := requirePTY(sess, "top"); err != nil {
		_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
		return 1, err
	}
	if a.provisioner == nil {
		err := errors.New("top is unavailable: provisioner is unavailable")
		_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
		return 1, err
	}

	quitCh := make(chan struct{}, 1)
	go watchTopExitKey(sess, quitCh)

	_, _ = io.WriteString(sess, "\x1b[?1049h\x1b[?25l")
	defer func() {
		_, _ = io.WriteString(sess, "\x1b[?25h\x1b[?1049l")
	}()

	startedAt := time.Now()
	var prev *topSnapshot
	for {
		snapshot, err := a.readTopSnapshot(sess.Context(), actor)
		if err != nil {
			wrapped := fmt.Errorf("top: %w", err)
			_, _ = io.WriteString(sess.Stderr(), wrapped.Error()+"\n")
			return 1, wrapped
		}
		screen := renderTopScreen(snapshot, prev, time.Since(startedAt), req.interval)
		if _, err := io.WriteString(sess, "\x1b[H\x1b[J"+screen); err != nil {
			if sess.Context().Err() != nil {
				return 0, nil
			}
			return 1, err
		}
		prev = &snapshot
		if _, err := sess.SendRequest("keepalive@openssh.com", true, nil); err != nil && sess.Context().Err() == nil {
			return 1, err
		}

		select {
		case <-sess.Context().Done():
			return 0, nil
		case <-quitCh:
			return 0, nil
		case <-time.After(req.interval):
		}
	}
}

func watchTopExitKey(r io.Reader, quitCh chan<- struct{}) {
	buf := make([]byte, 1)
	for {
		if _, err := r.Read(buf); err != nil {
			return
		}
		if buf[0] != 'q' && buf[0] != 'Q' && buf[0] != 0x03 {
			continue
		}
		select {
		case quitCh <- struct{}{}:
		default:
		}
		return
	}
}

func requirePTY(sess gssh.Session, command string) error {
	if _, _, ok := sess.Pty(); ok {
		return nil
	}
	return fmt.Errorf("%s requires a PTY (run with ssh -t)", command)
}

func rejectPTY(sess gssh.Session, command string) error {
	if _, _, ok := sess.Pty(); !ok {
		return nil
	}
	return fmt.Errorf("%s does not support PTY sessions", command)
}

func (a *App) readTopSnapshot(ctx context.Context, actor model.Actor) (topSnapshot, error) {
	instances, err := a.store.ListInstances(ctx, false)
	if err != nil {
		return topSnapshot{}, fmt.Errorf("list instances: %w", err)
	}
	instances = a.visibleInstances(actor, instances)
	snapshot := topSnapshot{
		capturedAt: time.Now(),
		instances:  make([]topInstanceSnapshot, len(instances)),
	}

	g, ctx := errgroup.WithContext(ctx)
	for i, inst := range instances {
		i, inst := i, inst
		g.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			sample := topInstanceSnapshot{
				Name:               inst.Name,
				State:              inst.State,
				VCPUCount:          effectiveInstanceVCPUCount(inst, a.cfg),
				DiskAllocatedBytes: topAllocatedFileBytes(inst.RootFSPath),
				DiskLimitBytes:     effectiveInstanceRootFSSizeBytes(inst),
			}
			if rx, tx, ok := readInterfaceByteCounters(inst.TapDevice); ok {
				// Host TAP rx is guest transmit, and host TAP tx is guest receive.
				sample.NetRXBytes = tx
				sample.NetTXBytes = rx
				sample.HasNetStats = true
			}
			if inst.FirecrackerPID > 0 {
				if uptime, ok := topInstanceUptime(inst, snapshot.capturedAt); ok {
					sample.Uptime = uptime
					sample.HasUptime = true
				}
				metrics, err := a.provisioner.ReadInstanceMetrics(ctx, inst.Name)
				if err == nil {
					sample.CPUUsageUsec = metrics.CPUUsageUsec
					sample.MemoryCurrentBytes = metrics.MemoryCurrentBytes
					sample.MemoryLimitBytes = metrics.MemoryLimitBytes
					sample.HasLiveMetrics = true
				}
			}
			if sample.MemoryLimitBytes == 0 {
				sample.MemoryLimitBytes = effectiveInstanceMemoryMiB(inst, a.cfg) * mib
			}
			snapshot.instances[i] = sample
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return topSnapshot{}, err
	}
	return snapshot, nil
}

func renderTopScreen(snapshot topSnapshot, prev *topSnapshot, sessionAge, interval time.Duration) string {
	headers := []string{"VM", "STATUS", "CPU%", "MEM", "DISK", "NET RX", "NET TX", "UPTIME"}
	rows := buildTopRenderedRows(snapshot, prev)
	if len(rows) == 0 {
		rows = append(rows, topRenderedRow{
			Values: []string{"no visible instances", "-", "-", "-", "-", "-", "-", "-"},
			Styles: []topCellStyle{topCellStyleMuted, topCellStyleMuted, topCellStyleMuted, topCellStyleMuted, topCellStyleMuted, topCellStyleMuted, topCellStyleMuted, topCellStyleMuted},
			Name:   "no visible instances",
		})
	}
	widths := []int{18, 16, 7, 24, 25, 12, 12, 8}

	var b strings.Builder
	fmt.Fprintf(
		&b,
		"%s  %s %s  %s %s  %s %d  %s\n",
		topEmphasize("srv top"),
		topMuted("uptime"),
		formatTopDuration(sessionAge),
		topMuted("refresh"),
		interval.Round(time.Millisecond),
		topMuted("visible"),
		len(snapshot.instances),
		topMuted("press q to exit"),
	)
	b.WriteString(buildTopSummary(snapshot, prev))
	b.WriteString("\n\n")
	b.WriteString(topBoxLine("┌", "┬", "┐", widths))
	b.WriteString(topBoxRow(headers, []topCellStyle{topCellStyleHeader, topCellStyleHeader, topCellStyleHeader, topCellStyleHeader, topCellStyleHeader, topCellStyleHeader, topCellStyleHeader, topCellStyleHeader}, widths))
	b.WriteString(topBoxLine("├", "┼", "┤", widths))
	for _, row := range rows {
		b.WriteString(topBoxRow(row.Values, row.Styles, widths))
	}
	b.WriteString(topBoxLine("└", "┴", "┘", widths))
	b.WriteString(topMuted("MEM = live/configured RAM. DISK = host allocated/configured rootfs.\n"))
	return b.String()
}

func buildTopRenderedRows(snapshot topSnapshot, prev *topSnapshot) []topRenderedRow {
	rows := make([]topRenderedRow, 0, len(snapshot.instances))
	var prevByName map[string]topInstanceSnapshot
	prevElapsed := 0.0
	if prev != nil {
		prevElapsed = snapshot.capturedAt.Sub(prev.capturedAt).Seconds()
		if prevElapsed > 0 {
			prevByName = make(map[string]topInstanceSnapshot, len(prev.instances))
			for _, inst := range prev.instances {
				prevByName[inst.Name] = inst
			}
		}
	}
	for _, inst := range snapshot.instances {
		statusValue, statusStyle := formatTopStatus(inst)
		row := topRenderedRow{
			Values: []string{
				inst.Name,
				statusValue,
				"-",
				formatTopMemory(inst),
				formatTopDisk(inst),
				"-",
				"-",
				formatTopUptime(inst),
			},
			Styles: []topCellStyle{
				topCellStyleNone,
				statusStyle,
				topCellStyleMuted,
				topMemorySeverity(inst),
				topDiskSeverity(inst),
				topCellStyleMuted,
				topCellStyleMuted,
				topUptimeStyle(inst),
			},
			Name: inst.Name,
		}
		if prevInst, ok := prevByName[inst.Name]; ok {
			if cpuPct, ok := topCPUPercent(prevInst, inst, prevElapsed); ok {
				row.Values[2] = formatTopCPU(cpuPct)
				row.Styles[2] = topCPUSeverity(inst, cpuPct)
				row.SortCPU = cpuPct
				row.HasCPU = true
			}
			if rxRate, ok := topByteRate(prevInst.NetRXBytes, inst.NetRXBytes, prevElapsed, inst.HasNetStats && prevInst.HasNetStats); ok {
				row.Values[5] = formatTopRate(rxRate)
				row.Styles[5] = topCellStyleNone
			}
			if txRate, ok := topByteRate(prevInst.NetTXBytes, inst.NetTXBytes, prevElapsed, inst.HasNetStats && prevInst.HasNetStats); ok {
				row.Values[6] = formatTopRate(txRate)
				row.Styles[6] = topCellStyleNone
			}
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		left, right := rows[i], rows[j]
		if left.HasCPU != right.HasCPU {
			return left.HasCPU
		}
		if left.HasCPU && right.HasCPU && left.SortCPU != right.SortCPU {
			return left.SortCPU > right.SortCPU
		}
		if left.Values[1] != right.Values[1] {
			return left.Values[1] < right.Values[1]
		}
		return left.Name < right.Name
	})
	return rows
}

func buildTopSummary(snapshot topSnapshot, prev *topSnapshot) string {
	running := 0
	stopped := 0
	transition := 0
	failed := 0

	hotCPUName := "-"
	hotCPUPct := 0.0
	hotCPUStyle := topCellStyleMuted
	hotCPUOK := false

	hotMemName := "-"
	hotMemRatio := 0.0
	hotMemStyle := topCellStyleMuted
	hotMemOK := false

	hotDiskName := "-"
	hotDiskRatio := 0.0
	hotDiskStyle := topCellStyleMuted
	hotDiskOK := false

	var prevByName map[string]topInstanceSnapshot
	prevElapsed := 0.0
	if prev != nil {
		prevElapsed = snapshot.capturedAt.Sub(prev.capturedAt).Seconds()
		if prevElapsed > 0 {
			prevByName = make(map[string]topInstanceSnapshot, len(prev.instances))
			for _, inst := range prev.instances {
				prevByName[inst.Name] = inst
			}
		}
	}

	totalRXRate := 0.0
	totalTXRate := 0.0
	hasRXRate := false
	hasTXRate := false

	for _, inst := range snapshot.instances {
		switch inst.State {
		case model.StateReady:
			running++
		case model.StateStopped:
			stopped++
		case model.StateProvisioning, model.StateAwaitingTailnet, model.StateDeleting:
			transition++
		case model.StateFailed:
			failed++
		}

		if ratio, ok := topUsageRatio(inst.MemoryCurrentBytes, inst.MemoryLimitBytes, inst.HasLiveMetrics); ok && (!hotMemOK || ratio > hotMemRatio) {
			hotMemName = inst.Name
			hotMemRatio = ratio
			hotMemStyle = topMemorySeverity(inst)
			hotMemOK = true
		}
		if ratio, ok := topUsageRatio(inst.DiskAllocatedBytes, inst.DiskLimitBytes, true); ok && (!hotDiskOK || ratio > hotDiskRatio) {
			hotDiskName = inst.Name
			hotDiskRatio = ratio
			hotDiskStyle = topDiskSeverity(inst)
			hotDiskOK = true
		}

		prevInst, ok := prevByName[inst.Name]
		if !ok {
			continue
		}
		if cpuPct, ok := topCPUPercent(prevInst, inst, prevElapsed); ok && (!hotCPUOK || cpuPct > hotCPUPct) {
			hotCPUName = inst.Name
			hotCPUPct = cpuPct
			hotCPUStyle = topCPUSeverity(inst, cpuPct)
			hotCPUOK = true
		}
		if rxRate, ok := topByteRate(prevInst.NetRXBytes, inst.NetRXBytes, prevElapsed, inst.HasNetStats && prevInst.HasNetStats); ok {
			totalRXRate += rxRate
			hasRXRate = true
		}
		if txRate, ok := topByteRate(prevInst.NetTXBytes, inst.NetTXBytes, prevElapsed, inst.HasNetStats && prevInst.HasNetStats); ok {
			totalTXRate += txRate
			hasTXRate = true
		}
	}

	countsLine := fmt.Sprintf(
		"%s %d  %s %d  %s %d  %s %d",
		topMuted("running"),
		running,
		topMuted("stopped"),
		stopped,
		topMuted("transition"),
		transition,
		topMuted("failed"),
		failed,
	)

	details := []string{
		fmt.Sprintf("%s %s %s", topMuted("hot cpu"), hotCPUName, topSummaryMetricValue(hotCPUOK, formatTopCPU(hotCPUPct), hotCPUStyle)),
		fmt.Sprintf("%s %s %s", topMuted("hot mem"), hotMemName, topSummaryMetricValue(hotMemOK, formatTopPercent(hotMemRatio), hotMemStyle)),
		fmt.Sprintf("%s %s %s", topMuted("hot disk"), hotDiskName, topSummaryMetricValue(hotDiskOK, formatTopPercent(hotDiskRatio), hotDiskStyle)),
	}
	if hasRXRate {
		details = append(details, fmt.Sprintf("%s %s", topMuted("rx"), formatTopRate(totalRXRate)))
	}
	if hasTXRate {
		details = append(details, fmt.Sprintf("%s %s", topMuted("tx"), formatTopRate(totalTXRate)))
	}

	return countsLine + "\n" + strings.Join(details, "  ")
}

func topBoxLine(left, middle, right string, widths []int) string {
	parts := make([]string, 0, len(widths))
	for _, width := range widths {
		parts = append(parts, strings.Repeat("─", width+2))
	}
	return left + strings.Join(parts, middle) + right + "\n"
}

func topBoxRow(values []string, styles []topCellStyle, widths []int) string {
	var b strings.Builder
	b.WriteString("│")
	for i, value := range values {
		clamped := topClampCell(value, widths[i])
		padding := strings.Repeat(" ", widths[i]-len([]rune(clamped)))
		b.WriteByte(' ')
		b.WriteString(topColorize(clamped, topRowStyle(styles, i)))
		b.WriteString(padding)
		b.WriteByte(' ')
		b.WriteString("│")
	}
	b.WriteByte('\n')
	return b.String()
}

func topClampCell(value string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 3 {
		return string(runes[:width])
	}
	return string(runes[:width-3]) + "..."
}

func topRowStyle(styles []topCellStyle, i int) topCellStyle {
	if i < 0 || i >= len(styles) {
		return topCellStyleNone
	}
	return styles[i]
}

func topColorize(value string, style topCellStyle) string {
	if value == "" {
		return value
	}
	code := ""
	switch style {
	case topCellStyleOK:
		code = "\x1b[38;2;0;121;76m"
	case topCellStyleWarning:
		code = "\x1b[38;2;255;183;0m"
	case topCellStyleCritical:
		code = "\x1b[38;2;255;65;54m"
	case topCellStyleMuted:
		code = "\x1b[38;2;128;128;128m"
	case topCellStyleHeader:
		code = "\x1b[1m"
	default:
		return value
	}
	return code + value + "\x1b[0m"
}

func topSummaryMetricValue(ok bool, value string, style topCellStyle) string {
	if !ok {
		return topColorize("-", topCellStyleMuted)
	}
	return topColorize(value, style)
}

func topMuted(value string) string {
	return topWrapStyle(value, "\x1b[38;2;128;128;128m")
}

func topEmphasize(value string) string {
	return topWrapStyle(value, "\x1b[1m")
}

func topWrapStyle(value, code string) string {
	if value == "" {
		return value
	}
	return code + value + "\x1b[0m"
}

func formatTopCPU(pct float64) string {
	return strconv.FormatFloat(pct, 'f', 1, 64) + "%"
}

func formatTopMemory(inst topInstanceSnapshot) string {
	if !inst.HasLiveMetrics || inst.MemoryLimitBytes <= 0 {
		return "-"
	}
	return formatTopUsage(inst.MemoryCurrentBytes, inst.MemoryLimitBytes)
}

func formatTopDisk(inst topInstanceSnapshot) string {
	if inst.DiskLimitBytes <= 0 {
		return "-"
	}
	return formatTopUsage(inst.DiskAllocatedBytes, inst.DiskLimitBytes)
}

func formatTopUsage(current, limit int64) string {
	ratio, ok := topUsageRatio(current, limit, true)
	if !ok {
		return format.BinarySize(current) + "/" + format.BinarySize(limit)
	}
	return fmt.Sprintf("%s/%s %s", format.BinarySize(current), format.BinarySize(limit), formatTopPercentPadded(ratio))
}

func formatTopRate(rate float64) string {
	if rate < 0 {
		return "-"
	}
	return format.BinarySize(int64(math.Round(rate))) + "/s"
}

func formatTopUptime(inst topInstanceSnapshot) string {
	if !inst.HasUptime {
		return "-"
	}
	return formatTopDuration(inst.Uptime)
}

func formatTopStatus(inst topInstanceSnapshot) (string, topCellStyle) {
	switch inst.State {
	case model.StateReady:
		return "running", topCellStyleOK
	case model.StateProvisioning, model.StateAwaitingTailnet:
		return inst.State, topCellStyleWarning
	case model.StateFailed, model.StateDeleting:
		return inst.State, topCellStyleCritical
	case model.StateStopped:
		return inst.State, topCellStyleWarning
	default:
		return inst.State, topCellStyleNone
	}
}

func topCPUSeverity(inst topInstanceSnapshot, pct float64) topCellStyle {
	if inst.VCPUCount <= 0 {
		return topCellStyleNone
	}
	return topRatioSeverity(pct/float64(inst.VCPUCount*100), 0.90, 0.98)
}

func topMemorySeverity(inst topInstanceSnapshot) topCellStyle {
	if !inst.HasLiveMetrics || inst.MemoryLimitBytes <= 0 {
		return topCellStyleMuted
	}
	return topRatioSeverity(float64(inst.MemoryCurrentBytes)/float64(inst.MemoryLimitBytes), 0.75, 0.90)
}

func topDiskSeverity(inst topInstanceSnapshot) topCellStyle {
	if inst.DiskLimitBytes <= 0 {
		return topCellStyleMuted
	}
	return topRatioSeverity(float64(inst.DiskAllocatedBytes)/float64(inst.DiskLimitBytes), 0.80, 0.95)
}

func topUptimeStyle(inst topInstanceSnapshot) topCellStyle {
	if !inst.HasUptime {
		return topCellStyleMuted
	}
	return topCellStyleNone
}

func topRatioSeverity(ratio, warning, critical float64) topCellStyle {
	switch {
	case ratio >= critical:
		return topCellStyleCritical
	case ratio >= warning:
		return topCellStyleWarning
	case ratio >= 0:
		return topCellStyleOK
	default:
		return topCellStyleNone
	}
}

func topUsageRatio(current, limit int64, ok bool) (float64, bool) {
	if !ok || limit <= 0 || current < 0 {
		return 0, false
	}
	return float64(current) / float64(limit), true
}

func formatTopPercent(ratio float64) string {
	if ratio < 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", ratio*100)
}

func formatTopPercentPadded(ratio float64) string {
	if ratio < 0 {
		return "-"
	}
	return fmt.Sprintf("%3.0f%%", ratio*100)
}

func formatTopDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return d.String()
	}
	hours := d / time.Hour
	minutes := (d % time.Hour) / time.Minute
	seconds := (d % time.Minute) / time.Second
	if hours > 0 {
		return fmt.Sprintf("%dh%02dm", hours, minutes)
	}
	return fmt.Sprintf("%dm%02ds", minutes, seconds)
}

func topCPUPercent(prev, curr topInstanceSnapshot, elapsedSeconds float64) (float64, bool) {
	if !prev.HasLiveMetrics || !curr.HasLiveMetrics || elapsedSeconds <= 0 || curr.CPUUsageUsec < prev.CPUUsageUsec {
		return 0, false
	}
	return float64(curr.CPUUsageUsec-prev.CPUUsageUsec) / elapsedSeconds / 10000.0, true
}

func topByteRate(prev, curr uint64, elapsedSeconds float64, ok bool) (float64, bool) {
	if !ok || elapsedSeconds <= 0 || curr < prev {
		return 0, false
	}
	return float64(curr-prev) / elapsedSeconds, true
}

func parseTopArgs(args []string) (topRequest, error) {
	req := topRequest{interval: defaultTopInterval}
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--interval":
			i++
			if i >= len(args) {
				return topRequest{}, errors.New(topUsage())
			}
			interval, err := parseTopInterval(args[i])
			if err != nil {
				return topRequest{}, fmt.Errorf("invalid interval %q\n%s", args[i], topUsage())
			}
			req.interval = interval
		default:
			return topRequest{}, fmt.Errorf("unexpected argument %q\n%s", args[i], topUsage())
		}
	}
	if req.interval < 200*time.Millisecond {
		return topRequest{}, fmt.Errorf("interval must be at least 200ms\n%s", topUsage())
	}
	return req, nil
}

func parseTopInterval(raw string) (time.Duration, error) {
	if strings.IndexFunc(raw, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		seconds, err := strconv.Atoi(raw)
		if err != nil {
			return 0, err
		}
		return time.Duration(seconds) * time.Second, nil
	}
	return time.ParseDuration(raw)
}

func topUsage() string {
	return "usage: top [--interval DURATION]"
}

func topInstanceUptime(inst model.Instance, now time.Time) (time.Duration, bool) {
	if inst.FirecrackerPID <= 0 {
		return 0, false
	}
	info, err := os.Stat(filepath.Join("/proc", strconv.Itoa(inst.FirecrackerPID)))
	if err != nil {
		return 0, false
	}
	startedAt := info.ModTime()
	if startedAt.After(now) {
		return 0, true
	}
	return now.Sub(startedAt), true
}

func topAllocatedFileBytes(path string) int64 {
	if path == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.Size()
	}
	return st.Blocks * 512
}

func readInterfaceByteCounters(name string) (uint64, uint64, bool) {
	if strings.TrimSpace(name) == "" {
		return 0, 0, false
	}
	rx, err := readUint64File(filepath.Join("/sys/class/net", name, "statistics", "rx_bytes"))
	if err != nil {
		return 0, 0, false
	}
	tx, err := readUint64File(filepath.Join("/sys/class/net", name, "statistics", "tx_bytes"))
	if err != nil {
		return 0, 0, false
	}
	return rx, tx, true
}

func readUint64File(path string) (uint64, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(payload)), 10, 64)
}

func (a *App) handleExportSession(sess gssh.Session, actor model.Actor, args []string) (int, error) {
	name, err := parseExportArgs(args)
	if err != nil {
		_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
		return 2, err
	}
	if err := rejectPTY(sess, "export"); err != nil {
		_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
		return 1, err
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
	if err := rejectPTY(sess, "import"); err != nil {
		_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
		return 1, err
	}

	artifactInfo, stream, err := provision.PeekPortableArtifactInfo(sess)
	if err != nil {
		wrapped := fmt.Errorf("import: %w", err)
		_, _ = io.WriteString(sess.Stderr(), wrapped.Error()+"\n")
		return 1, wrapped
	}

	unlock := a.lockInstance(artifactInfo.Name)
	defer func() {
		unlock()
		a.syncZenGatewayBestEffort()
	}()

	reporter := newImportProgressReporter(sess.Stderr(), 350*time.Millisecond)
	inst, info, err := a.provisioner.ImportInstance(sess.Context(), actor, stream, reporter.Update)
	reporter.Close(err == nil)
	if err != nil {
		wrapped := fmt.Errorf("import: %w", err)
		_, _ = io.WriteString(sess.Stderr(), wrapped.Error()+"\n")
		return 1, wrapped
	}

	var b strings.Builder
	fmt.Fprintf(&b, "imported: %s\n", inst.Name)
	if info.Name != "" && info.Name != inst.Name {
		fmt.Fprintf(&b, "source-name: %s\n", info.Name)
	}
	fmt.Fprintf(&b, "state: %s\n", inst.State)
	fmt.Fprintf(&b, "rootfs-size: %s\n", format.BinarySize(effectiveInstanceRootFSSizeBytes(inst)))
	fmt.Fprintf(&b, "exported-at: %s\n", info.ExportedAt.Format(time.RFC3339))
	if inst.TailscaleName != "" {
		fmt.Fprintf(&b, "tailscale-name: %s\n", inst.TailscaleName)
	}
	if inst.TailscaleIP != "" {
		fmt.Fprintf(&b, "tailscale-ip: %s\n", inst.TailscaleIP)
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

	fmt.Fprintln(&b, "NAME")
	fmt.Fprintln(&b, "    srv - manage Firecracker microVM instances over SSH")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "SYNOPSIS")
	fmt.Fprintln(&b, "    ssh srv [--json] <command> [args...]")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "DESCRIPTION")
	fmt.Fprintln(&b, "    Create, inspect, and control isolated microVM instances")
	fmt.Fprintln(&b, "    with automatic Tailscale networking.")

	type helpEntry struct {
		synopsis    string
		description string
	}

	groups := []struct {
		header  string
		entries []helpEntry
	}{
		{
			header: "INSTANCE COMMANDS",
			entries: []helpEntry{
				{"new <name> [--cpus N] [--ram SIZE] [--rootfs-size SIZE]", "Create a new microvm instance."},
				{"list", "List all instances."},
				{"inspect <name>", "Show instance details and recent events."},
				{"start <name>", "Start a stopped instance."},
				{"stop <name>", "Stop a running instance."},
				{"restart <name>", "Restart an instance."},
				{"resize <name> [--cpus N] [--ram SIZE] [--rootfs-size SIZE]", "Change instance resources (must be stopped)."},
				{"delete <name>", "Delete an instance."},
			},
		},
		{
			header: "BACKUP COMMANDS",
			entries: []helpEntry{
				{"backup create <name>", "Create a backup of an instance."},
				{"backup list <name>", "List backups for an instance."},
				{"restore <name> <backup-id>", "Restore an instance from a backup."},
				{"export <name>", "Export instance as a portable archive to stdout."},
				{"import", "Import instance from stdin."},
			},
		},
		{
			header: "DIAGNOSTICS",
			entries: []helpEntry{
				{"logs <name> [serial|firecracker]", "View instance logs."},
				{"logs -f <name> <target>", "Follow logs in real time."},
				{"top [--interval DURATION]", "Watch live CPU, memory, disk, and network usage."},
			},
		},
		{
			header: "ADMIN COMMANDS",
			entries: []helpEntry{
				{"status", "Show host capacity and allocation summary."},
				{"snapshot create", "Create a read-only btrfs data snapshot."},
			},
		},
	}

	for _, group := range groups {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, group.header)
		fmt.Fprintln(&b)
		for _, e := range group.entries {
			fmt.Fprintf(&b, "    %s\n", e.synopsis)
			fmt.Fprintf(&b, "        %s\n", e.description)
		}
	}

	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "GLOBAL OPTIONS")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "    --json")
	fmt.Fprintln(&b, "        Return machine-readable JSON for supported non-streaming")
	fmt.Fprintln(&b, "        commands.")

	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "NEW AND RESIZE OPTIONS")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "    --cpus N")
	fmt.Fprintln(&b, "        Number of vCPUs.")
	fmt.Fprintln(&b, "    --ram SIZE")
	fmt.Fprintln(&b, "        Memory (e.g. 512m, 2g).")
	fmt.Fprintln(&b, "    --rootfs-size SIZE")
	fmt.Fprintln(&b, "        Root filesystem size (e.g. 4g, 10g).")

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

func (a *App) cmdRestore(ctx context.Context, actor model.Actor, args []string, outFormat outputFormat) (commandResult, error) {
	name, backupID, err := parseRestoreArgs(args)
	if err != nil {
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	unlock := a.lockInstance(name)
	defer func() {
		unlock()
		a.syncZenGatewayBestEffort()
	}()

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
	if outFormat == outputFormatJSON {
		return jsonResult(restoreResponseJSON{Action: "restored", Instance: instanceSummaryPayload(a.cfg, inst, false), Backup: backupPayload(info)})
	}
	return commandResult{stdout: stdout, exitCode: 0}, nil
}

func (a *App) cmdStart(ctx context.Context, actor model.Actor, args []string, outFormat outputFormat) (commandResult, error) {
	if len(args) != 2 {
		err := errors.New("usage: start <name>")
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	unlock := a.lockInstance(args[1])
	defer func() {
		unlock()
		a.syncZenGatewayBestEffort()
	}()

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
	if outFormat == outputFormatJSON {
		return jsonResult(commandActionJSON{Action: "started", Instance: instanceSummaryPayload(a.cfg, inst, true)})
	}
	return lifecycleReadyResult("started", inst), nil
}

func (a *App) cmdStop(ctx context.Context, actor model.Actor, args []string, outFormat outputFormat) (commandResult, error) {
	if len(args) != 2 {
		err := errors.New("usage: stop <name>")
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	unlock := a.lockInstance(args[1])
	defer func() {
		unlock()
		a.syncZenGatewayBestEffort()
	}()

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
	if outFormat == outputFormatJSON {
		return jsonResult(commandActionJSON{Action: "stopped", Instance: instanceSummaryPayload(a.cfg, inst, false)})
	}
	return commandResult{stdout: fmt.Sprintf("stopped: %s\nstate: %s\n", inst.Name, inst.State), exitCode: 0}, nil
}

func (a *App) cmdRestart(ctx context.Context, actor model.Actor, args []string, outFormat outputFormat) (commandResult, error) {
	if len(args) != 2 {
		err := errors.New("usage: restart <name>")
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	unlock := a.lockInstance(args[1])
	defer func() {
		unlock()
		a.syncZenGatewayBestEffort()
	}()

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
	if outFormat == outputFormatJSON {
		return jsonResult(commandActionJSON{Action: "restarted", Instance: instanceSummaryPayload(a.cfg, inst, true)})
	}
	return lifecycleReadyResult("restarted", inst), nil
}

func (a *App) cmdDelete(ctx context.Context, actor model.Actor, args []string, outFormat outputFormat) (commandResult, error) {
	if len(args) != 2 {
		err := errors.New("usage: delete <name>")
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	unlock := a.lockInstance(args[1])
	defer func() {
		unlock()
		a.syncZenGatewayBestEffort()
	}()

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
	if outFormat == outputFormatJSON {
		return jsonResult(commandActionJSON{Action: "deleted", Instance: instanceSummaryPayload(a.cfg, inst, false)})
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
	if command == "snapshot" || command == "status" {
		if !a.isAdmin(actor) {
			return false, fmt.Sprintf("%s is not in SRV_ADMIN_USERS", actor.UserLogin)
		}
		if len(a.cfg.AllowedUsers) == 0 {
			return true, fmt.Sprintf("%s allowed to run %s as admin", actor.UserLogin, command)
		}
		for _, user := range a.cfg.AllowedUsers {
			if strings.EqualFold(user, actor.UserLogin) {
				return true, fmt.Sprintf("%s allowed to run %s as admin", actor.UserLogin, command)
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

func formatStatusInstanceSummary(instances statusInstancesJSON) string {
	parts := []string{
		fmt.Sprintf("%d total", instances.Total),
		fmt.Sprintf("%d running", instances.Running),
		fmt.Sprintf("%d stopped", instances.Stopped),
		fmt.Sprintf("%d failed", instances.Failed),
	}
	for _, state := range []string{model.StateDeleting} {
		if count := instances.ByState[state]; count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, state))
		}
	}
	return strings.Join(parts, ", ")
}

func formatStatusUsageLine(unit string, allocated, budget int64, pct int) string {
	switch unit {
	case "vcpu":
		return fmt.Sprintf("%d/%d vCPU [%d%%]", allocated, budget, pct)
	case "bytes":
		return formatStatusByteUsageLine(allocated, budget, pct)
	default:
		return fmt.Sprintf("%d/%d [%d%%]", allocated, budget, pct)
	}
}

func formatStatusResourceLine(resource statusResourceJSON, pct int) string {
	valueLine := formatStatusUsageLine(resource.Unit, resource.Allocated, resource.Budget, pct)
	if note := formatStatusInlineNote(resource); note != "" {
		return valueLine + " - " + note
	}
	return valueLine
}

func formatStatusInlineNote(resource statusResourceJSON) string {
	if resource.Advisory {
		return "overcommit allowed"
	}
	if resource.Reserve > 0 && resource.Unit == "bytes" {
		return format.BinarySize(resource.Reserve) + " host reserve"
	}
	return ""
}

func formatStatusByteUsageLine(allocated, budget int64, pct int) string {
	unit, divisor := statusByteUnit(maxInt64(absInt64(allocated), absInt64(budget)))
	return fmt.Sprintf("%s/%s %s [%d%%]",
		formatStatusUsageNumber(float64(allocated)/divisor),
		formatStatusUsageNumber(float64(budget)/divisor),
		unit,
		pct,
	)
}

func statusByteUnit(sizeBytes int64) (string, float64) {
	for _, unit := range []struct {
		name    string
		divisor float64
	}{
		{"TiB", 1024 * 1024 * 1024 * 1024},
		{"GiB", 1024 * 1024 * 1024},
		{"MiB", 1024 * 1024},
		{"KiB", 1024},
	} {
		if sizeBytes >= int64(unit.divisor) {
			return unit.name, unit.divisor
		}
	}
	return "B", 1
}

func formatStatusUsageNumber(value float64) string {
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(value, 'f', 2, 64), "0"), ".")
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func formatStatusLoadBar(width, logicalCores int, load float64) string {
	total := int64(logicalCores) * 100
	if total <= 0 {
		total = 100
	}
	return formatStatusBar(width, int64(math.Round(load*100)), total)
}

func formatStatusBar(width int, used, total int64) string {
	bar := format.BarWide(width, used, total)
	bar = strings.TrimPrefix(bar, "[")
	bar = strings.TrimSuffix(bar, "]")
	return bar
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
