package provision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"srv/internal/config"
	"srv/internal/model"
	"srv/internal/nethelper"
	"srv/internal/vmrunner"
)

// bootReportRunner stands in for the hypervisor and the guest bootstrap: a
// started guest reports the nonce it received unless report is false.
type bootReportRunner struct {
	orbLifecycleRunner
	serial string
	report bool
	fail   string
}

func (r *bootReportRunner) StartInstanceVM(ctx context.Context, req vmrunner.StartRequest) (vmrunner.StartResponse, error) {
	resp, err := r.orbLifecycleRunner.StartInstanceVM(ctx, req)
	if r.report {
		f, openErr := os.OpenFile(r.serial, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if openErr != nil {
			return resp, openErr
		}
		defer f.Close()
		if r.fail != "" {
			fmt.Fprintf(f, "srv-boot: {\"nonce\":%q,\"error\":%q}\n", req.Bootstrap.BootNonce, r.fail)
		} else {
			fmt.Fprintf(f, "boot noise\nsrv-boot: {\"nonce\":%q,\"ssh_host_key\":\"SHA256:fixture\"}\n", req.Bootstrap.BootNonce)
		}
	}
	return resp, err
}

type recordingNetworkHelper struct {
	noopNetworkHelper
	setups []nethelper.SetupRequest
}

func (h *recordingNetworkHelper) SetupInstanceNetwork(_ context.Context, req nethelper.SetupRequest) error {
	h.setups = append(h.setups, req)
	return nil
}

// hostGuestTestProvisioner has no tailnet API client: a guest without a
// tailnet must not need one.
func hostGuestTestProvisioner(t *testing.T, name string) (*Provisioner, *bootReportRunner, *recordingNetworkHelper, config.Config) {
	t.Helper()
	templateTestTools(t)
	dir := t.TempDir()
	cfg := loadProvisionTestConfig(t, map[string]string{"SRV_DATA_DIR": dir})
	cfg.BaseRootFSPath, cfg.BaseKernelPath = filepath.Join(dir, "base.img"), filepath.Join(dir, "kernel")
	cfg.OutboundInterface, cfg.GuestReadyTimeout = "fixture0", time.Second
	cfg.GuestAuthTags = nil
	templateTestImage(t, cfg.BaseRootFSPath, nil)
	if err := os.WriteFile(cfg.BaseKernelPath, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.InstancesDir(), 0700); err != nil {
		t.Fatal(err)
	}
	runner := &bootReportRunner{serial: filepath.Join(cfg.InstancesDir(), name, "serial.log"), report: true}
	network := &recordingNetworkHelper{}
	p := &Provisioner{cfg: cfg, store: newProvisionTestStore(t, cfg), log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		vmRunner: runner, networkHelper: network}
	return p, runner, network, cfg
}

func TestHostGuestLifecycleWithoutGuestTailscale(t *testing.T) {
	ctx := context.Background()
	p, runner, network, cfg := hostGuestTestProvisioner(t, "guest")
	p.cfg.ZenAPIKey, p.cfg.ZenGatewayPort = "configured", 4000
	actor := model.Actor{UserLogin: "owner"}
	opts := CreateOptions{Kind: model.KindVM, GuestNetwork: model.GuestNetworkHost, MemoryMiB: 128, RootFSSizeBytes: 32 << 20}

	orb := opts
	orb.Kind = model.KindOrb
	if _, err := p.Create(ctx, "guest-orb", actor, orb); err == nil {
		t.Fatal("host guest network accepted for an orb")
	}
	inst, err := p.Create(ctx, "guest", actor, opts)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != model.StateReady || inst.GuestNetwork != model.GuestNetworkHost || inst.TailnetNodeID != "" || inst.TailscaleIP != "" {
		t.Fatalf("unexpected instance: %+v", inst)
	}
	stored, err := p.store.GetInstance(ctx, "guest")
	if err != nil || stored.GuestNetwork != model.GuestNetworkHost || stored.State != model.StateReady {
		t.Fatalf("persisted instance: %+v, %v", stored, err)
	}
	if _, err := p.store.EnrollmentKey(ctx, inst.ID); err == nil {
		t.Fatal("host guest create recorded an enrollment key")
	}
	// Operators read the reported host key from inspect, which shows event messages.
	events, err := p.store.ListEvents(ctx, inst.ID, 50)
	if err != nil || !slices.ContainsFunc(events, func(e model.InstanceEvent) bool { return strings.Contains(e.Message, "SHA256:fixture") }) {
		t.Fatalf("ready event lacks the reported host key: %+v, %v", events, err)
	}
	if len(network.setups) != 1 {
		t.Fatalf("network setups = %d", len(network.setups))
	}
	boot := runner.starts[0].Bootstrap
	if boot.TailscaleAuthKey != "" || boot.TailscaleControlURL != "" || len(boot.TailscaleTags) != 0 || boot.EnrollmentNonce != "" ||
		boot.ZenGatewayPort != 4000 || boot.GuestNetwork != model.GuestNetworkHost || boot.BootNonce == "" || boot.BootNonce != stored.StartRequestID {
		t.Fatalf("bootstrap: %+v", boot)
	}

	// RAM resume skips the guest bootstrap, so it could never report a fresh
	// nonce: saving RAM is refused, and saved RAM is never resumed.
	if _, err := p.StopWithOptions(ctx, "guest", StopOptions{SaveState: true}); err == nil || !strings.Contains(err.Error(), "--save-state") {
		t.Fatalf("save-state stop of host guest: %v", err)
	}
	if still, err := p.store.GetInstance(ctx, "guest"); err != nil || still.State != model.StateReady || !runner.running {
		t.Fatalf("refused save-state changed the guest: %+v, %v", still, err)
	}
	if _, err := p.Stop(ctx, "guest"); err != nil {
		t.Fatal(err)
	}
	ramState := filepath.Join(cfg.InstancesDir(), "guest", "ram-state")
	if err := os.Mkdir(ramState, 0o700); err != nil {
		t.Fatal(err)
	}
	launches := len(runner.starts)
	if started, err := p.Start(ctx, "guest"); err == nil || !strings.Contains(err.Error(), "--discard-state") || started.State == model.StateReady || len(runner.starts) != launches {
		t.Fatalf("saved RAM was resumed or ignored: %+v, %v", started, err)
	}
	if started, err := p.StartWithOptions(ctx, "guest", StartOptions{DiscardState: true}); err != nil || started.State != model.StateReady {
		t.Fatalf("discard-state cold boot: %+v, %v", started, err)
	}
	if !runner.starts[len(runner.starts)-1].DiscardState {
		t.Fatal("cold boot did not discard saved RAM")
	}
	if err := os.Remove(ramState); err != nil {
		t.Fatal(err)
	}

	// A warm start is ready only on this attempt's report: the earlier report
	// remains in the serial log and must not count.
	if _, err := p.Stop(ctx, "guest"); err != nil {
		t.Fatal(err)
	}
	runner.report = false
	if started, err := p.Start(ctx, "guest"); err == nil || started.State == model.StateReady {
		t.Fatalf("stale boot report made a new start ready: %+v, %v", started, err)
	}
	pending, err := p.store.GetInstance(ctx, "guest")
	if err != nil || pending.State != model.StateAwaitingGuest {
		t.Fatalf("unconfirmed guest state: %+v, %v", pending, err)
	}
	// A later report for the same attempt completes readiness on retry.
	f, err := os.OpenFile(pending.SerialLogPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "srv-boot: {\"nonce\":%q}\n", pending.StartRequestID)
	f.Close()
	if ready, err := p.Start(ctx, "guest"); err != nil || ready.State != model.StateReady {
		t.Fatalf("late readiness: %+v, %v", ready, err)
	}

	// The retry verified the running guest instead of launching another.
	if len(runner.starts) != 3 {
		t.Fatalf("starts = %d, want create, discard-state boot and one warm start", len(runner.starts))
	}
	for _, start := range runner.starts {
		if start.Bootstrap.TailscaleControlURL != "" || len(start.Bootstrap.TailscaleTags) != 0 || start.Bootstrap.GuestNetwork != model.GuestNetworkHost {
			t.Fatalf("launch carried tailnet settings: %+v", start.Bootstrap)
		}
	}
	// A vanished runtime leaves a non-tailnet guest stopped, not failed.
	runner.running = false
	if reconciled, err := p.ReconcileInstanceRuntime(ctx, "guest"); err != nil || reconciled.State != model.StateStopped {
		t.Fatalf("absent runtime: %+v, %v", reconciled, err)
	}

	var artifact bytes.Buffer
	if _, err := p.ExportInstance(ctx, "guest", &artifact); err != nil {
		t.Fatal(err)
	}
	tr, zr, err := newPortableArtifactTarReader(bytes.NewReader(artifact.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := readPortableManifest(tr)
	zr.Close()
	if err != nil || manifest.Version != 5 || manifest.Instance.GuestNetwork != model.GuestNetworkHost {
		t.Fatalf("export must require a guest-network-aware reader: %+v, %v", manifest, err)
	}
	destCfg := loadProvisionTestConfig(t, map[string]string{"SRV_DATA_DIR": t.TempDir(), "SRV_BASE_KERNEL": cfg.BaseKernelPath})
	dest := &Provisioner{cfg: destCfg, store: newProvisionTestStore(t, destCfg), log: p.log}
	if err := os.MkdirAll(destCfg.InstancesDir(), 0700); err != nil {
		t.Fatal(err)
	}
	imported, _, err := dest.ImportInstance(ctx, actor, bytes.NewReader(artifact.Bytes()), nil)
	if err != nil || imported.GuestNetwork != model.GuestNetworkHost {
		t.Fatalf("imported guest network: %+v, %v", imported, err)
	}
}

func TestBootReportRequiresThisAttemptsNonce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serial.log")
	if err := os.WriteFile(path, []byte("srv-boot: {\"nonce\":\"old\"}\nsrv-enrollment: {\"nonce\":\"new\",\"node_id\":\"n\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bootReport(path, "new"); err == nil {
		t.Fatal("accepted another attempt's report or an enrollment line")
	}
	if _, err := bootReport(path, ""); err == nil {
		t.Fatal("accepted an empty nonce")
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprintln(f, `srv-boot: {"nonce":"new","ssh_host_key":"SHA256:abc"}`)
	f.Close()
	if report, err := bootReport(path, "new"); err != nil || report.SSHHostKey != "SHA256:abc" {
		t.Fatalf("report: %+v, %v", report, err)
	}
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprintln(f, `srv-boot: {"nonce":"failed","error":"no sshd"}`)
	f.Close()
	if _, err := bootReport(path, "failed"); !errors.Is(err, errBootFailed) {
		t.Fatalf("failure report: %v", err)
	}
}

func TestHostGuestBootFailureFailsFast(t *testing.T) {
	ctx := context.Background()
	p, runner, _, _ := hostGuestTestProvisioner(t, "guest")
	p.cfg.GuestReadyTimeout = time.Minute
	runner.fail = "requires an OpenSSH server in the image"
	began := time.Now()
	inst, err := p.Create(ctx, "guest", model.Actor{UserLogin: "owner"}, CreateOptions{Kind: model.KindVM, GuestNetwork: model.GuestNetworkHost, MemoryMiB: 128, RootFSSizeBytes: 32 << 20})
	if !errors.Is(err, errBootFailed) || !strings.Contains(err.Error(), "OpenSSH") || inst.State == model.StateReady {
		t.Fatalf("boot failure: %+v, %v", inst, err)
	}
	if elapsed := time.Since(began); elapsed > 10*time.Second {
		t.Fatalf("boot failure waited for the readiness timeout: %s", elapsed)
	}
	stored, err := p.store.GetInstance(ctx, "guest")
	if err != nil || !strings.Contains(stored.LastError, "OpenSSH") || stored.State == model.StateReady {
		t.Fatalf("stored boot failure: %+v, %v", stored, err)
	}
	events, err := p.store.ListEvents(ctx, stored.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	recorded := false
	for _, event := range events {
		recorded = recorded || (event.Type == "boot" && strings.Contains(event.Payload, "OpenSSH"))
	}
	if !recorded {
		t.Fatalf("boot failure event missing: %+v", events)
	}
}
