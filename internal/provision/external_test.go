package provision

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"srv/internal/model"
	"srv/internal/nethelper"
)

func (h *recordingNetworkHelper) ReapplyExternalNetwork(_ context.Context, req nethelper.SetupRequest) error {
	h.reapplys = append(h.reapplys, req)
	return nil
}

// External-only guests share the host guest lifecycle and add confinement.
func TestExternalOnlyGuestIsConfined(t *testing.T) {
	ctx := context.Background()
	p, runner, network, cfg := hostGuestTestProvisioner(t, "ext")
	p.cfg.ZenAPIKey, p.cfg.ZenGatewayPort = "configured", 4000
	p.cfg.ExternalDenyCIDRs = []string{"203.0.113.0/24"}
	actor := model.Actor{UserLogin: "owner"}
	opts := CreateOptions{Kind: model.KindVM, GuestNetwork: model.GuestNetworkExternalOnly, MemoryMiB: 128, RootFSSizeBytes: 32 << 20}

	if _, err := p.Create(ctx, "ext", actor, opts); err == nil || !strings.Contains(err.Error(), "SRV_EXTERNAL_UPLINKS") {
		t.Fatalf("external-only without an approved uplink: %v", err)
	}
	if len(runner.starts) != 0 || len(network.setups) != 0 {
		t.Fatal("rejected create reached the runner or network helper")
	}

	p.cfg.ExternalUplinks = []string{"wan0", "wan1"}
	inst, err := p.Create(ctx, "ext", actor, opts)
	if err != nil || inst.State != model.StateReady || inst.GuestNetwork != model.GuestNetworkExternalOnly {
		t.Fatalf("create: %+v, %v", inst, err)
	}
	setup := network.setups[0]
	if !slices.Equal(setup.ExternalUplinks, []string{"wan0", "wan1"}) || setup.GuestAddr != inst.GuestAddr ||
		!slices.Contains(setup.DenyCIDRs, cfg.VMNetworkCIDR) || !slices.Contains(setup.DenyCIDRs, "203.0.113.0/24") {
		t.Fatalf("setup did not request confinement: %+v", setup)
	}
	if boot := runner.starts[0].Bootstrap; boot.ZenGatewayPort != 0 || boot.GuestNetwork != model.GuestNetworkExternalOnly || boot.TailscaleControlURL != "" {
		t.Fatalf("bootstrap: %+v", boot)
	}
	// A service restart reapplies the current policy to the running guest.
	p.cfg.ExternalDenyCIDRs = []string{"192.0.2.77/32"}
	if err := p.RestoreInstance(ctx, "ext"); err != nil {
		t.Fatal(err)
	}
	if len(network.reapplys) != 1 || !slices.Contains(network.reapplys[0].DenyCIDRs, "192.0.2.77/32") || len(runner.starts) != 1 {
		t.Fatalf("restore did not reapply the current policy: %+v, starts=%d", network.reapplys, len(runner.starts))
	}

	if _, err := p.Stop(ctx, "ext"); err != nil {
		t.Fatal(err)
	}
	var artifact bytes.Buffer
	if _, err := p.ExportInstance(ctx, "ext", &artifact); err != nil {
		t.Fatal(err)
	}
	destCfg := loadProvisionTestConfig(t, map[string]string{"SRV_DATA_DIR": t.TempDir(), "SRV_BASE_KERNEL": cfg.BaseKernelPath})
	dest := &Provisioner{cfg: destCfg, store: newProvisionTestStore(t, destCfg), log: p.log}
	if err := os.MkdirAll(destCfg.InstancesDir(), 0700); err != nil {
		t.Fatal(err)
	}
	imported, _, err := dest.ImportInstance(ctx, actor, bytes.NewReader(artifact.Bytes()), nil)
	if err != nil || imported.GuestNetwork != model.GuestNetworkExternalOnly {
		t.Fatalf("imported guest network: %+v, %v", imported, err)
	}
	// The destination has no approved uplink, so the import cannot be started.
	if _, err := dest.Start(ctx, "ext"); err == nil || !strings.Contains(err.Error(), "SRV_EXTERNAL_UPLINKS") {
		t.Fatalf("start without approved uplink: %v", err)
	}
}
