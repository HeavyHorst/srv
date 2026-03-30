package provision

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2/clientcredentials"
	"tailscale.com/client/tailscale"

	"srv/internal/config"
	"srv/internal/model"
	"srv/internal/nethelper"
	"srv/internal/store"
	"srv/internal/vmrunner"
)

var (
	errGuestNotReady   = errors.New("guest never joined the tailnet before timeout")
	errGuestExited     = errors.New("guest exited before joining the tailnet")
	validName          = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	signalProcess      = syscall.Kill
	loopDevicesForPath = func(path string) (string, error) {
		output, err := exec.Command("losetup", "-j", path, "--output", "NAME", "--noheadings").CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("list loop devices for %s: %w: %s", path, err, strings.TrimSpace(string(output)))
		}
		return strings.TrimSpace(string(output)), nil
	}
)

type Provisioner struct {
	cfg           config.Config
	log           *slog.Logger
	store         *store.Store
	tsClient      *tailscale.Client
	networkHelper networkHelper
	vmRunner      vmRunner
}

type networkHelper interface {
	SetupInstanceNetwork(ctx context.Context, req nethelper.SetupRequest) error
	CleanupInstanceNetwork(ctx context.Context, req nethelper.CleanupRequest) error
}

type vmRunner interface {
	StartInstanceVM(ctx context.Context, req vmrunner.StartRequest) (vmrunner.StartResponse, error)
	StopInstanceVM(ctx context.Context, req vmrunner.StopRequest) error
}

type guestBootstrap = vmrunner.Bootstrap
type guestMetadata = vmrunner.Metadata

type tailnetDeviceSnapshot struct {
	DeviceID string
	LastSeen string
}

type CreateOptions struct {
	VCPUCount       int64
	MemoryMiB       int64
	RootFSSizeBytes int64
}

func New(cfg config.Config, logger *slog.Logger, st *store.Store) (*Provisioner, error) {
	p := &Provisioner{
		cfg:           cfg,
		log:           logger,
		store:         st,
		networkHelper: nethelper.NewClient(cfg.NetHelperSocketPath),
		vmRunner:      vmrunner.NewClient(cfg.VMRunnerSocketPath),
	}
	if cfg.Tailnet != "" && cfg.TailscaleClientSecret != "" {
		credentials := clientcredentials.Config{
			ClientID:     firstNonEmpty(cfg.TailscaleClientID, "srv-control-plane"),
			ClientSecret: cfg.TailscaleClientSecret,
			TokenURL:     strings.TrimRight(cfg.TailscaleAPIBaseURL, "/") + "/api/v2/oauth/token",
		}
		client := tailscale.NewClient(cfg.Tailnet, nil)
		client.BaseURL = strings.TrimRight(cfg.TailscaleAPIBaseURL, "/")
		client.HTTPClient = credentials.Client(context.Background())
		client.UserAgent = "srv-control-plane"
		p.tsClient = client
	}
	return p, nil
}

func (p *Provisioner) Create(ctx context.Context, name string, actor model.Actor, opts CreateOptions) (model.Instance, error) {
	if !validName.MatchString(name) {
		return model.Instance{}, fmt.Errorf("invalid instance name %q", name)
	}
	baseRootFSSize, err := p.baseRootFSSize()
	if err != nil {
		return model.Instance{}, err
	}
	resolved, needsResize, err := p.resolveCreateOptions(opts, baseRootFSSize)
	if err != nil {
		return model.Instance{}, err
	}
	if err := p.ensureCreatePrereqs(needsResize); err != nil {
		return model.Instance{}, err
	}

	instanceDir, err := p.prepareInstanceDir(ctx, name)
	if err != nil {
		return model.Instance{}, err
	}

	networkCIDR, hostAddr, guestAddr, gateway, err := p.allocateNetwork(ctx)
	if err != nil {
		_ = os.RemoveAll(instanceDir)
		return model.Instance{}, err
	}

	now := time.Now().UTC()
	inst := model.Instance{
		ID:              uuid.NewString(),
		Name:            name,
		State:           model.StateProvisioning,
		CreatedAt:       now,
		UpdatedAt:       now,
		CreatedByUser:   actor.UserLogin,
		CreatedByNode:   actor.NodeName,
		VCPUCount:       resolved.VCPUCount,
		MemoryMiB:       resolved.MemoryMiB,
		RootFSSizeBytes: resolved.RootFSSizeBytes,
		RootFSPath:      filepath.Join(instanceDir, "rootfs.img"),
		KernelPath:      p.cfg.BaseKernelPath,
		InitrdPath:      p.cfg.BaseInitrdPath,
		SocketPath:      filepath.Join(instanceDir, "firecracker.sock"),
		LogPath:         filepath.Join(instanceDir, "firecracker.log"),
		SerialLogPath:   filepath.Join(instanceDir, "serial.log"),
		TapDevice:       tapName(name),
		GuestMAC:        guestMAC(name),
		NetworkCIDR:     networkCIDR,
		HostAddr:        hostAddr,
		GuestAddr:       guestAddr,
		GatewayAddr:     gateway,
	}

	if err := p.store.CreateInstance(ctx, inst); err != nil {
		_ = os.RemoveAll(instanceDir)
		return model.Instance{}, err
	}
	p.recordEvent(inst.ID, "create", "instance record created", nil)

	cleanup := true
	startedMachine := false
	var mintedKeyID string
	defer func() {
		if cleanup {
			if startedMachine {
				_ = p.stopFirecracker(inst.Name, inst.FirecrackerPID)
			}
			_ = p.cleanupNetworking(inst)
			_ = p.removeInstanceDir(inst.Name)
			if mintedKeyID != "" {
				_ = p.deleteAuthKey(context.Background(), mintedKeyID)
			}
			inst.State = model.StateFailed
			inst.LastError = firstNonEmpty(inst.LastError, "provisioning failed")
			inst.UpdatedAt = time.Now().UTC()
			_ = p.store.UpdateInstance(context.Background(), inst)
		}
	}()

	if err := p.cloneRootFS(ctx, inst.RootFSPath); err != nil {
		inst.LastError = err.Error()
		return inst, err
	}
	p.recordEvent(inst.ID, "storage", "rootfs cloned from base image", map[string]any{"dest": inst.RootFSPath})
	if needsResize {
		if err := p.growRootFS(inst.RootFSPath, inst.RootFSSizeBytes); err != nil {
			inst.LastError = err.Error()
			return inst, err
		}
		p.recordEvent(inst.ID, "storage", "rootfs expanded for instance", map[string]any{"size_bytes": inst.RootFSSizeBytes})
	}
	if err := p.ensureInstanceRuntimePermissions(inst); err != nil {
		inst.LastError = err.Error()
		return inst, err
	}

	if err := p.setupNetworking(ctx, inst); err != nil {
		inst.LastError = err.Error()
		return inst, err
	}
	p.recordEvent(inst.ID, "network", "tap device and NAT configured", map[string]any{"tap": inst.TapDevice, "network": inst.NetworkCIDR})

	authKey, keyID, err := p.mintGuestAuthKey(ctx)
	if err != nil {
		inst.LastError = err.Error()
		return inst, err
	}
	mintedKeyID = keyID

	bootstrap := guestBootstrap{
		Version:             1,
		Hostname:            inst.Name,
		TailscaleAuthKey:    authKey,
		TailscaleControlURL: p.cfg.GuestTailscaleControlURL,
		TailscaleTags:       p.cfg.GuestAuthTags,
	}
	if err := p.writeMetadataFile(inst, bootstrap); err != nil {
		inst.LastError = err.Error()
		return inst, err
	}
	p.recordEvent(inst.ID, "bootstrap", "guest bootstrap metadata written", nil)

	pid, err := p.startFirecracker(ctx, inst, bootstrap)
	if err != nil {
		inst.LastError = err.Error()
		return inst, err
	}
	startedMachine = true
	inst.FirecrackerPID = pid
	inst.UpdatedAt = time.Now().UTC()
	if err := p.store.UpdateInstance(ctx, inst); err != nil {
		inst.LastError = err.Error()
		return inst, err
	}
	p.recordEvent(inst.ID, "firecracker", "microVM started", map[string]any{"pid": pid})

	tailscaleName, tailscaleIP, err := p.waitForTailnetJoin(ctx, inst.Name, inst.FirecrackerPID)
	if err != nil {
		inst.LastError = err.Error()
		inst.UpdatedAt = time.Now().UTC()
		if errors.Is(err, errGuestNotReady) {
			cleanup = false
			inst.State = model.StateAwaitingTailnet
			_ = p.store.UpdateInstance(context.Background(), inst)
			p.recordEvent(inst.ID, "tailnet", "guest readiness timed out; instance left intact for debugging", nil)
			return inst, err
		}
		return inst, err
	}

	inst.State = model.StateReady
	inst.TailscaleName = tailscaleName
	inst.TailscaleIP = tailscaleIP
	inst.LastError = ""
	inst.UpdatedAt = time.Now().UTC()
	if err := p.store.UpdateInstance(ctx, inst); err != nil {
		inst.LastError = err.Error()
		return inst, err
	}
	p.recordEvent(inst.ID, "tailnet", "guest joined the tailnet", map[string]any{"tailscale_name": tailscaleName, "tailscale_ip": tailscaleIP})

	cleanup = false
	if mintedKeyID != "" {
		_ = p.deleteAuthKey(context.Background(), mintedKeyID)
	}
	return inst, nil
}

func (p *Provisioner) Delete(ctx context.Context, name string) (model.Instance, error) {
	inst, err := p.store.GetInstance(ctx, name)
	if err != nil {
		return model.Instance{}, err
	}
	if inst.State == model.StateDeleted {
		return inst, fmt.Errorf("instance %q is already deleted", name)
	}

	inst.State = model.StateDeleting
	inst.UpdatedAt = time.Now().UTC()
	if err := p.store.UpdateInstance(ctx, inst); err != nil {
		return inst, err
	}
	p.recordEvent(inst.ID, "delete", "delete requested", nil)

	if err := p.stopFirecracker(inst.Name, inst.FirecrackerPID); err != nil {
		p.log.Warn("stop firecracker", "name", inst.Name, "pid", inst.FirecrackerPID, "err", err)
	}
	if err := p.cleanupNetworking(inst); err != nil {
		p.log.Warn("cleanup networking", "name", inst.Name, "err", err)
	}
	if err := p.removeInstanceDir(inst.Name); err != nil {
		p.log.Warn("remove instance directory", "name", inst.Name, "err", err)
	}
	if p.tsClient != nil {
		if device, ok, err := p.findDevice(ctx, inst.Name); err == nil && ok {
			if err := p.tsClient.DeleteDevice(ctx, device.DeviceID); err != nil {
				p.log.Warn("delete tailscale device", "name", inst.Name, "err", err)
			}
		}
	}

	now := time.Now().UTC()
	inst.State = model.StateDeleted
	inst.FirecrackerPID = 0
	inst.TailscaleName = ""
	inst.TailscaleIP = ""
	inst.LastError = ""
	inst.UpdatedAt = now
	inst.DeletedAt = &now
	if err := p.store.UpdateInstance(ctx, inst); err != nil {
		return inst, err
	}
	p.recordEvent(inst.ID, "delete", "instance deleted", nil)
	return inst, nil
}

func (p *Provisioner) RestoreInstances(ctx context.Context) error {
	instances, err := p.store.ListInstances(ctx, false)
	if err != nil {
		return err
	}
	for _, inst := range instances {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := p.restoreInstance(ctx, inst); err != nil {
			p.log.Error("restore instance after startup", "name", inst.Name, "state", inst.State, "err", err)
		}
	}
	return nil
}

func (p *Provisioner) restoreInstance(ctx context.Context, inst model.Instance) error {
	if inst.FirecrackerPID > 0 && processExists(inst.FirecrackerPID) {
		return nil
	}
	if inst.FirecrackerPID != 0 {
		inst.FirecrackerPID = 0
		inst.UpdatedAt = time.Now().UTC()
		if err := p.stopFirecracker(inst.Name, 0); err != nil {
			p.log.Warn("cleanup stale firecracker state", "name", inst.Name, "err", err)
		}
		if err := p.store.UpdateInstance(ctx, inst); err != nil {
			return err
		}
	}
	if !shouldAutoStartAfterStartup(inst) {
		return nil
	}
	if !hasTailnetIdentity(inst) {
		p.recordEvent(inst.ID, "startup", "instance was left offline after control-plane startup because it never completed initial tailnet bootstrap", nil)
		return nil
	}
	p.recordEvent(inst.ID, "startup", "restarting instance automatically after control-plane startup", nil)
	if _, err := p.Start(ctx, inst.Name); err != nil {
		p.recordEvent(inst.ID, "startup", "automatic restart after control-plane startup failed", map[string]any{"error": err.Error()})
		return err
	}
	return nil
}

func (p *Provisioner) Stop(ctx context.Context, name string) (model.Instance, error) {
	inst, err := p.store.GetInstance(ctx, name)
	if err != nil {
		return model.Instance{}, err
	}
	if inst.State == model.StateDeleted {
		return inst, fmt.Errorf("instance %q is already deleted", name)
	}
	if inst.State == model.StateStopped && inst.FirecrackerPID == 0 {
		return inst, fmt.Errorf("instance %q is already stopped", name)
	}

	p.recordEvent(inst.ID, "stop", "stop requested", nil)
	if err := p.stopFirecracker(inst.Name, inst.FirecrackerPID); err != nil {
		return inst, err
	}
	if err := p.cleanupNetworking(inst); err != nil {
		return inst, err
	}

	inst.State = model.StateStopped
	inst.FirecrackerPID = 0
	inst.LastError = ""
	inst.UpdatedAt = time.Now().UTC()
	if err := p.store.UpdateInstance(ctx, inst); err != nil {
		return inst, err
	}
	p.recordEvent(inst.ID, "stop", "instance stopped", nil)
	return inst, nil
}

func (p *Provisioner) Resize(ctx context.Context, name string, opts CreateOptions) (model.Instance, error) {
	inst, err := p.store.GetInstance(ctx, name)
	if err != nil {
		return model.Instance{}, err
	}
	if inst.State == model.StateDeleted {
		return inst, fmt.Errorf("instance %q is deleted", name)
	}
	if inst.State != model.StateStopped {
		return inst, fmt.Errorf("instance %q must be stopped before resize (current state: %s)", name, inst.State)
	}
	if inst.FirecrackerPID > 0 && processExists(inst.FirecrackerPID) {
		return inst, fmt.Errorf("instance %q must be stopped before resize", name)
	}

	resized := inst
	if resized.FirecrackerPID > 0 {
		resized.FirecrackerPID = 0
	}
	if opts.VCPUCount > 0 {
		resized.VCPUCount = opts.VCPUCount
	}
	if opts.MemoryMiB > 0 {
		resized.MemoryMiB = opts.MemoryMiB
	}
	if err := validateMachineShape(p.effectiveVCPUCount(resized), p.effectiveMemoryMiB(resized)); err != nil {
		return inst, err
	}
	if opts.RootFSSizeBytes > 0 {
		currentSize, err := p.rootFSSize(resized.RootFSPath)
		if err != nil {
			return inst, err
		}
		if opts.RootFSSizeBytes < currentSize {
			return inst, fmt.Errorf("rootfs size %d bytes is smaller than the current image size %d bytes", opts.RootFSSizeBytes, currentSize)
		}
		if opts.RootFSSizeBytes > currentSize {
			if _, err := exec.LookPath("resize2fs"); err != nil {
				return inst, errors.New("resize with a larger rootfs requires resize2fs on the host")
			}
			if err := p.growRootFS(resized.RootFSPath, opts.RootFSSizeBytes); err != nil {
				return inst, err
			}
			p.recordEvent(inst.ID, "storage", "rootfs expanded for instance", map[string]any{"size_bytes": opts.RootFSSizeBytes})
		}
		resized.RootFSSizeBytes = opts.RootFSSizeBytes
	}

	resized.LastError = ""
	resized.UpdatedAt = time.Now().UTC()
	if err := p.store.UpdateInstance(ctx, resized); err != nil {
		return inst, err
	}
	p.recordEvent(resized.ID, "resize", "instance config updated", map[string]any{
		"vcpus":             p.effectiveVCPUCount(resized),
		"memory_mib":        p.effectiveMemoryMiB(resized),
		"rootfs_size_bytes": resized.RootFSSizeBytes,
	})
	return resized, nil
}

func (p *Provisioner) Start(ctx context.Context, name string) (model.Instance, error) {
	inst, err := p.store.GetInstance(ctx, name)
	if err != nil {
		return model.Instance{}, err
	}
	if err := p.ensureStartPrereqs(inst); err != nil {
		return inst, err
	}
	if inst.FirecrackerPID > 0 && processExists(inst.FirecrackerPID) {
		return inst, fmt.Errorf("instance %q is already running", name)
	}

	previousDevice, hadPreviousDevice, err := p.currentDeviceSnapshot(ctx, inst.Name)
	if err != nil {
		return inst, err
	}
	if err := p.cleanupNetworking(inst); err != nil {
		p.log.Warn("cleanup stale networking before start", "name", inst.Name, "err", err)
	}
	if err := p.ensureInstanceRuntimePermissions(inst); err != nil {
		return inst, err
	}
	p.recordEvent(inst.ID, "start", "start requested", nil)

	cleanup := true
	startedMachine := false
	defer func() {
		if cleanup {
			if startedMachine {
				_ = p.stopFirecracker(inst.Name, inst.FirecrackerPID)
			}
			_ = p.cleanupNetworking(inst)
			inst.State = model.StateStopped
			inst.FirecrackerPID = 0
			inst.LastError = firstNonEmpty(inst.LastError, "start failed")
			inst.UpdatedAt = time.Now().UTC()
			_ = p.store.UpdateInstance(context.Background(), inst)
		}
	}()

	if err := p.setupNetworking(ctx, inst); err != nil {
		inst.LastError = err.Error()
		return inst, err
	}
	p.recordEvent(inst.ID, "network", "tap device and NAT configured", map[string]any{"tap": inst.TapDevice, "network": inst.NetworkCIDR})

	bootstrap := guestBootstrap{
		Version:             1,
		Hostname:            inst.Name,
		TailscaleControlURL: p.cfg.GuestTailscaleControlURL,
		TailscaleTags:       p.cfg.GuestAuthTags,
	}
	pid, err := p.startFirecracker(ctx, inst, bootstrap)
	if err != nil {
		inst.LastError = err.Error()
		return inst, err
	}
	startedMachine = true
	inst.State = model.StateProvisioning
	inst.FirecrackerPID = pid
	inst.LastError = ""
	inst.UpdatedAt = time.Now().UTC()
	if err := p.store.UpdateInstance(ctx, inst); err != nil {
		inst.LastError = err.Error()
		return inst, err
	}
	p.recordEvent(inst.ID, "firecracker", "microVM started", map[string]any{"pid": pid})

	tailscaleName, tailscaleIP, err := p.waitForTailnetJoinAfter(ctx, inst.Name, inst.FirecrackerPID, previousDevice, hadPreviousDevice)
	if err != nil {
		inst.LastError = err.Error()
		inst.UpdatedAt = time.Now().UTC()
		if errors.Is(err, errGuestNotReady) {
			cleanup = false
			inst.State = model.StateAwaitingTailnet
			_ = p.store.UpdateInstance(context.Background(), inst)
			p.recordEvent(inst.ID, "tailnet", "guest readiness timed out after start; instance left intact for debugging", nil)
			return inst, err
		}
		return inst, err
	}

	inst.State = model.StateReady
	inst.TailscaleName = tailscaleName
	inst.TailscaleIP = tailscaleIP
	inst.LastError = ""
	inst.UpdatedAt = time.Now().UTC()
	if err := p.store.UpdateInstance(ctx, inst); err != nil {
		inst.LastError = err.Error()
		return inst, err
	}
	p.recordEvent(inst.ID, "tailnet", "guest rejoined the tailnet", map[string]any{"tailscale_name": tailscaleName, "tailscale_ip": tailscaleIP})

	cleanup = false
	return inst, nil
}

func (p *Provisioner) prepareInstanceDir(ctx context.Context, name string) (string, error) {
	instanceDir, err := p.instanceDir(name)
	if err != nil {
		return "", err
	}
	if existing, found, err := p.store.FindInstance(ctx, name); err != nil {
		return "", err
	} else if found {
		switch existing.State {
		case model.StateFailed, model.StateDeleted:
			if err := p.store.DeleteInstance(ctx, name); err != nil {
				return "", err
			}
			if err := p.removeInstanceDir(name); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("instance %q already exists with state %s", name, existing.State)
		}
	}
	if err := os.Mkdir(instanceDir, 0o770); err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("instance %q already exists on disk", name)
		}
		return "", fmt.Errorf("create instance directory: %w", err)
	}
	if err := os.Chmod(instanceDir, 0o770); err != nil {
		return "", fmt.Errorf("set instance directory permissions: %w", err)
	}
	return instanceDir, nil
}

func (p *Provisioner) ensureCreatePrereqs(needsResize bool) error {
	for _, path := range []string{p.cfg.BaseKernelPath, p.cfg.BaseRootFSPath} {
		if path == "" {
			return errors.New("create requires SRV_BASE_KERNEL and SRV_BASE_ROOTFS")
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("required file %s: %w", path, err)
		}
	}
	inUse, err := p.baseRootFSInUse()
	if err != nil {
		return err
	}
	if inUse {
		return fmt.Errorf("base rootfs image %s is still attached to a loop device; wait for the image build or mount to finish before creating instances", p.cfg.BaseRootFSPath)
	}
	if needsResize {
		if _, err := exec.LookPath("resize2fs"); err != nil {
			return errors.New("create with a custom rootfs size requires resize2fs on the host")
		}
	}
	if p.tsClient == nil {
		return errors.New("create requires TS_TAILNET and TS_CLIENT_SECRET so the control plane can mint one-off guest auth keys")
	}
	if len(p.cfg.GuestAuthTags) == 0 {
		return errors.New("create requires SRV_GUEST_AUTH_TAGS so minted guest auth keys carry an allowed tag")
	}
	fsType, err := p.fsType(p.cfg.DataDirAbs())
	if err != nil {
		return err
	}
	if fsType != "btrfs" {
		return fmt.Errorf("data dir %s must live on btrfs, found %s", p.cfg.DataDirAbs(), fsType)
	}
	return nil
}

func (p *Provisioner) ensureStartPrereqs(inst model.Instance) error {
	if inst.State == model.StateDeleted {
		return fmt.Errorf("instance %q is deleted", inst.Name)
	}
	if !hasTailnetIdentity(inst) {
		return fmt.Errorf("instance %q has not completed initial tailnet bootstrap yet; use inspect to debug or delete/new to reprovision", inst.Name)
	}
	if p.tsClient == nil {
		return errors.New("start requires TS_TAILNET and TS_CLIENT_SECRET so the control plane can observe guest tailnet readiness")
	}
	for _, path := range []string{inst.KernelPath, inst.RootFSPath} {
		if path == "" {
			return fmt.Errorf("instance %q is missing required runtime paths", inst.Name)
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("required file %s: %w", path, err)
		}
	}
	return nil
}

func (p *Provisioner) baseRootFSInUse() (bool, error) {
	loops, err := loopDevicesForPath(p.cfg.BaseRootFSPath)
	if err != nil {
		return false, err
	}
	return loops != "", nil
}

func (p *Provisioner) baseRootFSSize() (int64, error) {
	info, err := os.Stat(p.cfg.BaseRootFSPath)
	if err != nil {
		return 0, fmt.Errorf("stat base rootfs %s: %w", p.cfg.BaseRootFSPath, err)
	}
	return info.Size(), nil
}

func (p *Provisioner) rootFSSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat rootfs %s: %w", path, err)
	}
	return info.Size(), nil
}

func (p *Provisioner) resolveCreateOptions(opts CreateOptions, baseRootFSSize int64) (CreateOptions, bool, error) {
	resolved := CreateOptions{
		VCPUCount:       p.effectiveVCPUCount(model.Instance{VCPUCount: opts.VCPUCount}),
		MemoryMiB:       p.effectiveMemoryMiB(model.Instance{MemoryMiB: opts.MemoryMiB}),
		RootFSSizeBytes: opts.RootFSSizeBytes,
	}
	if resolved.RootFSSizeBytes == 0 {
		resolved.RootFSSizeBytes = baseRootFSSize
	}
	if err := validateMachineShape(resolved.VCPUCount, resolved.MemoryMiB); err != nil {
		return CreateOptions{}, false, err
	}
	if resolved.RootFSSizeBytes < baseRootFSSize {
		return CreateOptions{}, false, fmt.Errorf("rootfs size %d bytes is smaller than the base image size %d bytes", resolved.RootFSSizeBytes, baseRootFSSize)
	}
	return resolved, resolved.RootFSSizeBytes > baseRootFSSize, nil
}

func (p *Provisioner) cloneRootFS(ctx context.Context, dest string) error {
	cmd := exec.CommandContext(ctx, "cp", "--reflink=always", p.cfg.BaseRootFSPath, dest)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("reflink rootfs clone: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if err := os.Chmod(dest, 0o660); err != nil {
		return fmt.Errorf("set rootfs permissions: %w", err)
	}
	return nil
}

func (p *Provisioner) growRootFS(path string, sizeBytes int64) error {
	if err := os.Truncate(path, sizeBytes); err != nil {
		return fmt.Errorf("expand rootfs image to %d bytes: %w", sizeBytes, err)
	}
	cmd := exec.Command("resize2fs", path)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("resize rootfs filesystem: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (p *Provisioner) ensureInstanceRuntimePermissions(inst model.Instance) error {
	instanceDir := filepath.Dir(inst.RootFSPath)
	if err := os.Chmod(instanceDir, 0o770); err != nil {
		return fmt.Errorf("set instance directory permissions: %w", err)
	}
	if err := os.Chmod(inst.RootFSPath, 0o660); err != nil {
		return fmt.Errorf("set rootfs permissions: %w", err)
	}
	// The vm runner owns runtime log creation and may hand existing files to the
	// jailer identity on first boot, so leave log paths alone during restart.
	return nil
}

func (p *Provisioner) allocateNetwork(ctx context.Context) (networkCIDR, hostAddr, guestAddr, gateway string, err error) {
	instances, err := p.store.ListInstances(ctx, false)
	if err != nil {
		return "", "", "", "", err
	}
	used := make(map[string]struct{}, len(instances))
	for _, inst := range instances {
		if inst.NetworkCIDR != "" {
			used[inst.NetworkCIDR] = struct{}{}
		}
	}

	baseIP, baseNet, err := net.ParseCIDR(p.cfg.VMNetworkCIDR)
	if err != nil {
		return "", "", "", "", fmt.Errorf("parse vm network cidr: %w", err)
	}
	ones, bits := baseNet.Mask.Size()
	if bits != 32 || ones > 30 {
		return "", "", "", "", fmt.Errorf("vm network cidr %s must be an IPv4 prefix with room for /30 allocations", p.cfg.VMNetworkCIDR)
	}

	base := binary.BigEndian.Uint32(baseIP.To4())
	size := uint32(1) << (32 - ones)
	for offset := uint32(0); offset+4 <= size; offset += 4 {
		subnetIP := uint32ToIP(base + offset)
		subnet := (&net.IPNet{IP: subnetIP, Mask: net.CIDRMask(30, 32)}).String()
		if _, exists := used[subnet]; exists {
			continue
		}
		host := uint32ToIP(base + offset + 1)
		guest := uint32ToIP(base + offset + 2)
		return subnet, host.String() + "/30", guest.String() + "/30", host.String(), nil
	}
	return "", "", "", "", errors.New("no free /30 network blocks remain")
}

func (p *Provisioner) setupNetworking(ctx context.Context, inst model.Instance) error {
	if p.networkHelper == nil {
		return errors.New("network helper client is unavailable")
	}
	outbound, err := p.outboundInterface(ctx)
	if err != nil {
		return err
	}
	return p.networkHelper.SetupInstanceNetwork(ctx, nethelper.SetupRequest{
		TapDevice:         inst.TapDevice,
		HostAddr:          inst.HostAddr,
		NetworkCIDR:       inst.NetworkCIDR,
		OutboundInterface: outbound,
	})
}

func (p *Provisioner) cleanupNetworking(inst model.Instance) error {
	if p.networkHelper == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	outbound, _ := p.outboundInterface(ctx)
	return p.networkHelper.CleanupInstanceNetwork(ctx, nethelper.CleanupRequest{
		TapDevice:         inst.TapDevice,
		NetworkCIDR:       inst.NetworkCIDR,
		OutboundInterface: outbound,
	})
}

func (p *Provisioner) mintGuestAuthKey(ctx context.Context) (secret string, keyID string, err error) {
	caps := tailscale.KeyCapabilities{
		Devices: tailscale.KeyDeviceCapabilities{
			Create: tailscale.KeyDeviceCreateCapabilities{
				Reusable:      false,
				Ephemeral:     false,
				Preauthorized: true,
				Tags:          p.cfg.GuestAuthTags,
			},
		},
	}
	secret, meta, err := p.tsClient.CreateKeyWithExpiry(ctx, caps, p.cfg.GuestAuthExpiry)
	if err != nil {
		return "", "", fmt.Errorf("mint guest auth key: %w", err)
	}
	if meta != nil {
		keyID = meta.ID
	}
	return secret, keyID, nil
}

func (p *Provisioner) deleteAuthKey(ctx context.Context, id string) error {
	if id == "" || p.tsClient == nil {
		return nil
	}
	return p.tsClient.DeleteKey(ctx, id)
}

func (p *Provisioner) writeMetadataFile(inst model.Instance, bootstrap guestBootstrap) error {
	redacted := bootstrap
	redacted.TailscaleAuthKey = "[redacted]"
	payload, err := json.MarshalIndent(guestMetadata{SRV: redacted}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal bootstrap metadata: %w", err)
	}
	path := filepath.Join(filepath.Dir(inst.RootFSPath), "meta.json")
	if err := os.WriteFile(path, payload, 0o640); err != nil {
		return fmt.Errorf("write metadata file: %w", err)
	}
	return nil
}

func (p *Provisioner) startFirecracker(ctx context.Context, inst model.Instance, bootstrap guestBootstrap) (int, error) {
	if p.vmRunner == nil {
		return 0, errors.New("vm runner client is unavailable")
	}
	resp, err := p.vmRunner.StartInstanceVM(ctx, vmrunner.StartRequest{
		Name:        inst.Name,
		TapDevice:   inst.TapDevice,
		GuestMAC:    inst.GuestMAC,
		GuestAddr:   inst.GuestAddr,
		GatewayAddr: inst.GatewayAddr,
		Nameservers: p.cfg.VMDNSServers,
		VCPUCount:   p.effectiveVCPUCount(inst),
		MemoryMiB:   p.effectiveMemoryMiB(inst),
		KernelArgs:  kernelArgs(p.cfg.ExtraKernelArgs),
		Bootstrap:   bootstrap,
	})
	if err != nil {
		return 0, err
	}
	return resp.PID, nil
}

func (p *Provisioner) instanceDir(name string) (string, error) {
	instanceDir, err := directChildPath(p.cfg.InstancesDir(), name)
	if err != nil {
		return "", fmt.Errorf("resolve instance directory for %q: %w", name, err)
	}
	return instanceDir, nil
}

func (p *Provisioner) removeInstanceDir(name string) error {
	instanceDir, err := p.instanceDir(name)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(instanceDir); err != nil {
		return fmt.Errorf("remove instance directory %s: %w", instanceDir, err)
	}
	return nil
}

func (p *Provisioner) effectiveVCPUCount(inst model.Instance) int64 {
	if inst.VCPUCount > 0 {
		return inst.VCPUCount
	}
	return p.cfg.VCPUCount
}

func (p *Provisioner) effectiveMemoryMiB(inst model.Instance) int64 {
	if inst.MemoryMiB > 0 {
		return inst.MemoryMiB
	}
	return p.cfg.MemoryMiB
}

func validateMachineShape(vcpus, memoryMiB int64) error {
	return config.ValidateMachineShape(vcpus, memoryMiB)
}

func (p *Provisioner) waitForTailnetJoin(ctx context.Context, name string, firecrackerPID int) (string, string, error) {
	return p.waitForTailnetJoinAfter(ctx, name, firecrackerPID, tailnetDeviceSnapshot{}, false)
}

func (p *Provisioner) waitForTailnetJoinAfter(ctx context.Context, name string, firecrackerPID int, previous tailnetDeviceSnapshot, hadPrevious bool) (string, string, error) {
	deadlineCtx, cancel := context.WithTimeout(ctx, p.cfg.GuestReadyTimeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		if firecrackerPID > 0 && !processExists(firecrackerPID) {
			return "", "", errGuestExited
		}
		device, ok, err := p.findDevice(deadlineCtx, name)
		if err == nil && ok && deviceUpdatedSince(device, previous, hadPrevious) {
			ip := ""
			if len(device.Addresses) > 0 {
				ip = device.Addresses[0]
			}
			return firstNonEmpty(device.Hostname, device.Name, name), ip, nil
		}
		select {
		case <-deadlineCtx.Done():
			return "", "", errGuestNotReady
		case <-ticker.C:
		}
	}
}

func (p *Provisioner) findDevice(ctx context.Context, name string) (tailscale.Device, bool, error) {
	if p.tsClient == nil {
		return tailscale.Device{}, false, errors.New("tailscale device client is unavailable")
	}
	devices, err := p.tsClient.Devices(ctx, nil)
	if err != nil {
		return tailscale.Device{}, false, fmt.Errorf("list tailscale devices: %w", err)
	}
	for _, device := range devices {
		if strings.EqualFold(device.Hostname, name) || strings.EqualFold(trimDot(prefixBeforeDot(device.Name)), name) {
			return *device, true, nil
		}
	}
	return tailscale.Device{}, false, nil
}

func (p *Provisioner) currentDeviceSnapshot(ctx context.Context, name string) (tailnetDeviceSnapshot, bool, error) {
	device, ok, err := p.findDevice(ctx, name)
	if err != nil {
		return tailnetDeviceSnapshot{}, false, err
	}
	if !ok {
		return tailnetDeviceSnapshot{}, false, nil
	}
	return tailnetDeviceSnapshot{DeviceID: device.DeviceID, LastSeen: device.LastSeen}, true, nil
}

func (p *Provisioner) stopFirecracker(name string, pid int) error {
	if p.vmRunner == nil {
		return errors.New("vm runner client is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := p.vmRunner.StopInstanceVM(ctx, vmrunner.StopRequest{Name: name, PID: pid}); err != nil {
		return fmt.Errorf("stop firecracker for %q: %w", name, err)
	}
	return nil
}

func (p *Provisioner) fsType(path string) (string, error) {
	output, err := exec.Command("stat", "-f", "-c", "%T", path).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("stat fs type: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func (p *Provisioner) outboundInterface(ctx context.Context) (string, error) {
	if p.cfg.OutboundInterface != "" {
		return p.cfg.OutboundInterface, nil
	}
	output, err := exec.CommandContext(ctx, "ip", "route", "show", "default").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("detect outbound interface: %w: %s", err, strings.TrimSpace(string(output)))
	}
	fields := strings.Fields(string(output))
	for i := 0; i < len(fields)-1; i++ {
		if fields[i] == "dev" {
			return strings.TrimSpace(fields[i+1]), nil
		}
	}
	return "", errors.New("could not determine outbound interface from default route")
}

func (p *Provisioner) recordEvent(instanceID, eventType, message string, payload any) {
	text := ""
	if payload != nil {
		if encoded, err := json.Marshal(payload); err == nil {
			text = string(encoded)
		}
	}
	if err := p.store.RecordEvent(context.Background(), model.InstanceEvent{
		InstanceID: instanceID,
		CreatedAt:  time.Now().UTC(),
		Type:       eventType,
		Message:    message,
		Payload:    text,
	}); err != nil {
		p.log.Error("record instance event", "instance_id", instanceID, "type", eventType, "err", err)
	}
}

func hasTailnetIdentity(inst model.Instance) bool {
	return strings.TrimSpace(inst.TailscaleName) != "" || strings.TrimSpace(inst.TailscaleIP) != ""
}

func shouldAutoStartAfterStartup(inst model.Instance) bool {
	switch inst.State {
	case model.StateDeleted, model.StateFailed, model.StateStopped:
		return false
	default:
		return true
	}
}

func deviceUpdatedSince(device tailscale.Device, previous tailnetDeviceSnapshot, hadPrevious bool) bool {
	if !hadPrevious {
		return true
	}
	if previous.DeviceID != "" && device.DeviceID != "" && device.DeviceID != previous.DeviceID {
		return true
	}
	return strings.TrimSpace(device.LastSeen) != "" && device.LastSeen != previous.LastSeen
}

func readUnifiedCgroupPath(path string) (string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(payload), "\n") {
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		cgroupPath := strings.TrimSpace(strings.TrimPrefix(line, "0::"))
		if cgroupPath == "" {
			return "/", nil
		}
		if !filepath.IsAbs(cgroupPath) {
			return "", fmt.Errorf("unified cgroup path %q is not absolute", cgroupPath)
		}
		return cgroupPath, nil
	}
	return "", fmt.Errorf("could not find a unified cgroup entry in %s", path)
}

func directChildPath(base, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name is empty")
	}
	if name == "." || name == ".." || filepath.Base(name) != name {
		return "", fmt.Errorf("name %q must be a single path segment", name)
	}
	return filepath.Join(filepath.Clean(base), name), nil
}

func tapName(name string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return fmt.Sprintf("tap-%010x", h.Sum64())[:14]
}

func guestMAC(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	buf := make([]byte, 2)
	_, _ = rand.Read(buf)
	sum := h.Sum32()
	return fmt.Sprintf("02:fc:%02x:%02x:%02x:%02x", byte(sum>>24), byte(sum>>16), buf[0], buf[1])
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := signalProcess(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func uint32ToIP(v uint32) net.IP {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, v)
	return net.IP(buf)
}

func kernelArgs(extra string) string {
	base := []string{"console=ttyS0", "reboot=k", "panic=1", "pci=off", "root=/dev/vda", "rw"}
	if extra != "" {
		base = append(base, strings.Fields(extra)...)
	}
	return strings.Join(base, " ")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func prefixBeforeDot(v string) string {
	part, _, _ := strings.Cut(v, ".")
	return part
}

func trimDot(v string) string {
	return strings.TrimSuffix(v, ".")
}
