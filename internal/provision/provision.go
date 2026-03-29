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

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/google/uuid"
	"golang.org/x/oauth2/clientcredentials"
	"tailscale.com/client/tailscale"

	"srv/internal/config"
	"srv/internal/model"
	"srv/internal/store"
)

var (
	errGuestNotReady    = errors.New("guest never joined the tailnet before timeout")
	errGuestExited      = errors.New("guest exited before joining the tailnet")
	validName           = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	vmContextForRequest = func(context.Context) context.Context {
		return context.Background()
	}
	loopDevicesForPath = func(path string) (string, error) {
		output, err := exec.Command("losetup", "-j", path, "--output", "NAME", "--noheadings").CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("list loop devices for %s: %w: %s", path, err, strings.TrimSpace(string(output)))
		}
		return strings.TrimSpace(string(output)), nil
	}
)

type Provisioner struct {
	cfg      config.Config
	log      *slog.Logger
	store    *store.Store
	tsClient *tailscale.Client
}

type guestBootstrap struct {
	Version             int      `json:"version"`
	Hostname            string   `json:"hostname"`
	TailscaleAuthKey    string   `json:"tailscale_auth_key,omitempty"`
	TailscaleControlURL string   `json:"tailscale_control_url,omitempty"`
	TailscaleTags       []string `json:"tailscale_tags,omitempty"`
}

type guestMetadata struct {
	SRV guestBootstrap `json:"srv"`
}

type tailnetDeviceSnapshot struct {
	DeviceID string
	LastSeen string
}

func New(cfg config.Config, logger *slog.Logger, st *store.Store) (*Provisioner, error) {
	p := &Provisioner{cfg: cfg, log: logger, store: st}
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

func (p *Provisioner) Create(ctx context.Context, name string, actor model.Actor) (model.Instance, error) {
	if !validName.MatchString(name) {
		return model.Instance{}, fmt.Errorf("invalid instance name %q", name)
	}
	if err := p.ensureCreatePrereqs(); err != nil {
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
		ID:            uuid.NewString(),
		Name:          name,
		State:         model.StateProvisioning,
		CreatedAt:     now,
		UpdatedAt:     now,
		CreatedByUser: actor.UserLogin,
		CreatedByNode: actor.NodeName,
		RootFSPath:    filepath.Join(instanceDir, "rootfs.img"),
		KernelPath:    p.cfg.BaseKernelPath,
		InitrdPath:    p.cfg.BaseInitrdPath,
		SocketPath:    filepath.Join(instanceDir, "firecracker.sock"),
		LogPath:       filepath.Join(instanceDir, "firecracker.log"),
		SerialLogPath: filepath.Join(instanceDir, "serial.log"),
		TapDevice:     tapName(name),
		GuestMAC:      guestMAC(name),
		NetworkCIDR:   networkCIDR,
		HostAddr:      hostAddr,
		GuestAddr:     guestAddr,
		GatewayAddr:   gateway,
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
				_ = p.stopFirecracker(inst.FirecrackerPID)
			}
			_ = p.cleanupNetworking(inst)
			_ = os.RemoveAll(instanceDir)
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

	if err := p.stopFirecracker(inst.FirecrackerPID); err != nil {
		p.log.Warn("stop firecracker", "name", inst.Name, "pid", inst.FirecrackerPID, "err", err)
	}
	if err := p.cleanupNetworking(inst); err != nil {
		p.log.Warn("cleanup networking", "name", inst.Name, "err", err)
	}
	if err := os.RemoveAll(filepath.Dir(inst.RootFSPath)); err != nil {
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
	if err := p.stopFirecracker(inst.FirecrackerPID); err != nil {
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
	p.recordEvent(inst.ID, "start", "start requested", nil)

	cleanup := true
	startedMachine := false
	defer func() {
		if cleanup {
			if startedMachine {
				_ = p.stopFirecracker(inst.FirecrackerPID)
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
	instanceDir := filepath.Join(p.cfg.InstancesDir(), name)
	if existing, found, err := p.store.FindInstance(ctx, name); err != nil {
		return "", err
	} else if found {
		switch existing.State {
		case model.StateFailed, model.StateDeleted:
			if err := p.store.DeleteInstance(ctx, name); err != nil {
				return "", err
			}
			_ = os.RemoveAll(instanceDir)
		default:
			return "", fmt.Errorf("instance %q already exists with state %s", name, existing.State)
		}
	}
	if err := os.Mkdir(instanceDir, 0o755); err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("instance %q already exists on disk", name)
		}
		return "", fmt.Errorf("create instance directory: %w", err)
	}
	return instanceDir, nil
}

func (p *Provisioner) ensureCreatePrereqs() error {
	for _, path := range []string{p.cfg.BaseKernelPath, p.cfg.BaseRootFSPath, p.cfg.FirecrackerBinary} {
		if path == "" {
			return errors.New("create requires SRV_BASE_KERNEL, SRV_BASE_ROOTFS, and SRV_FIRECRACKER_BIN")
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
	for _, path := range []string{inst.KernelPath, inst.RootFSPath, p.cfg.FirecrackerBinary} {
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

func (p *Provisioner) cloneRootFS(ctx context.Context, dest string) error {
	cmd := exec.CommandContext(ctx, "cp", "--reflink=always", p.cfg.BaseRootFSPath, dest)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("reflink rootfs clone: %w: %s", err, strings.TrimSpace(string(output)))
	}
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
	outbound, err := p.outboundInterface(ctx)
	if err != nil {
		return err
	}
	if err := p.run(ctx, "ip", "tuntap", "add", "dev", inst.TapDevice, "mode", "tap"); err != nil {
		return err
	}
	if err := p.run(ctx, "ip", "addr", "add", inst.HostAddr, "dev", inst.TapDevice); err != nil {
		return err
	}
	if err := p.run(ctx, "ip", "link", "set", "dev", inst.TapDevice, "up"); err != nil {
		return err
	}
	if err := p.run(ctx, "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}
	if err := p.ensureIPTablesRule(ctx, "nat", "POSTROUTING", "-s", inst.NetworkCIDR, "-o", outbound, "-j", "MASQUERADE"); err != nil {
		return err
	}
	if err := p.ensureIPTablesRule(ctx, "filter", "FORWARD", "-i", inst.TapDevice, "-j", "ACCEPT"); err != nil {
		return err
	}
	if err := p.ensureIPTablesRule(ctx, "filter", "FORWARD", "-o", inst.TapDevice, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"); err != nil {
		return err
	}
	return nil
}

func (p *Provisioner) cleanupNetworking(inst model.Instance) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	outbound, _ := p.outboundInterface(ctx)
	var errs []string
	if outbound != "" {
		if err := p.deleteIPTablesRule(ctx, "nat", "POSTROUTING", "-s", inst.NetworkCIDR, "-o", outbound, "-j", "MASQUERADE"); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if err := p.deleteIPTablesRule(ctx, "filter", "FORWARD", "-i", inst.TapDevice, "-j", "ACCEPT"); err != nil {
		errs = append(errs, err.Error())
	}
	if err := p.deleteIPTablesRule(ctx, "filter", "FORWARD", "-o", inst.TapDevice, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"); err != nil {
		errs = append(errs, err.Error())
	}
	if inst.TapDevice != "" {
		if err := p.run(ctx, "ip", "link", "set", "dev", inst.TapDevice, "down"); err != nil && !isMissingNetworkDeviceError(err) {
			errs = append(errs, err.Error())
		}
		if err := p.run(ctx, "ip", "tuntap", "del", "dev", inst.TapDevice, "mode", "tap"); err != nil && !isMissingNetworkDeviceError(err) {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
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
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return fmt.Errorf("write metadata file: %w", err)
	}
	return nil
}

func (p *Provisioner) startFirecracker(ctx context.Context, inst model.Instance, bootstrap guestBootstrap) (int, error) {
	if err := os.Remove(inst.SocketPath); err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("remove stale firecracker socket: %w", err)
	}
	serialLog, err := os.OpenFile(inst.SerialLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open serial log: %w", err)
	}
	defer serialLog.Close()

	guestIP, guestNet, err := net.ParseCIDR(inst.GuestAddr)
	if err != nil {
		return 0, fmt.Errorf("parse guest addr: %w", err)
	}

	rootDriveID := "rootfs"
	isReadOnly := false
	isRootDevice := true
	vcpus := p.cfg.VCPUCount
	mem := p.cfg.MemoryMiB

	fcCfg := firecracker.Config{
		SocketPath:      inst.SocketPath,
		LogPath:         inst.LogPath,
		KernelImagePath: inst.KernelPath,
		InitrdPath:      inst.InitrdPath,
		KernelArgs:      kernelArgs(p.cfg.ExtraKernelArgs),
		Drives: []models.Drive{{
			DriveID:      &rootDriveID,
			PathOnHost:   &inst.RootFSPath,
			IsReadOnly:   &isReadOnly,
			IsRootDevice: &isRootDevice,
		}},
		NetworkInterfaces: firecracker.NetworkInterfaces{{
			StaticConfiguration: &firecracker.StaticNetworkConfiguration{
				MacAddress:  inst.GuestMAC,
				HostDevName: inst.TapDevice,
				IPConfiguration: &firecracker.IPConfiguration{
					IPAddr:      net.IPNet{IP: guestIP, Mask: guestNet.Mask},
					Gateway:     net.ParseIP(inst.GatewayAddr),
					Nameservers: p.cfg.VMDNSServers,
				},
			},
			AllowMMDS: true,
		}},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  &vcpus,
			MemSizeMib: &mem,
		},
		MmdsAddress: net.ParseIP("169.254.169.254"),
	}
	// The microVM should outlive the provisioning request; deletes are handled
	// later by PID instead of request-context cancellation.
	vmCtx := vmContextForRequest(ctx)

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(p.cfg.FirecrackerBinary).
		WithSocketPath(inst.SocketPath).
		WithStdout(serialLog).
		WithStderr(serialLog).
		Build(vmCtx)

	machine, err := firecracker.NewMachine(vmCtx, fcCfg, firecracker.WithProcessRunner(cmd))
	if err != nil {
		return 0, fmt.Errorf("create firecracker machine: %w", err)
	}
	machine.Handlers.FcInit = machine.Handlers.FcInit.Append(firecracker.NewSetMetadataHandler(guestMetadata{SRV: bootstrap}))

	if err := machine.Start(vmCtx); err != nil {
		return 0, fmt.Errorf("start firecracker machine: %w", err)
	}
	pid, err := machine.PID()
	if err != nil {
		return 0, fmt.Errorf("read firecracker pid: %w", err)
	}
	return pid, nil
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

func (p *Provisioner) stopFirecracker(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal firecracker pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill firecracker pid %d: %w", pid, err)
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

func (p *Provisioner) ensureIPTablesRule(ctx context.Context, table, chain string, rule ...string) error {
	checkArgs := append([]string{"-t", table, "-C", chain}, rule...)
	if err := exec.CommandContext(ctx, "iptables", checkArgs...).Run(); err == nil {
		return nil
	}
	addArgs := append([]string{"-t", table, "-A", chain}, rule...)
	return p.run(ctx, "iptables", addArgs...)
}

func (p *Provisioner) deleteIPTablesRule(ctx context.Context, table, chain string, rule ...string) error {
	deleteArgs := append([]string{"-t", table, "-D", chain}, rule...)
	cmd := exec.CommandContext(ctx, "iptables", deleteArgs...)
	if output, err := cmd.CombinedOutput(); err != nil {
		text := strings.TrimSpace(string(output))
		if strings.Contains(text, "No chain/target/match") || strings.Contains(text, "Bad rule") {
			return nil
		}
		return fmt.Errorf("iptables delete rule: %w: %s", err, text)
	}
	return nil
}

func (p *Provisioner) run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
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

func isMissingNetworkDeviceError(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "Cannot find device") || strings.Contains(text, "No such device") || strings.Contains(text, "does not exist")
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
	err := syscall.Kill(pid, 0)
	return err == nil
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
