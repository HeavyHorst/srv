package provision

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"tailscale.com/client/tailscale"

	"srv/internal/config"
	"srv/internal/model"
	"srv/internal/store"
)

func TestPrepareInstanceDirAllowsReuseOfFailedAndDeletedInstances(t *testing.T) {
	tests := []struct {
		name  string
		state string
	}{
		{name: "failed", state: model.StateFailed},
		{name: "deleted", state: model.StateDeleted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			cfg := loadProvisionTestConfig(t, map[string]string{
				"SRV_VM_NETWORK_CIDR": "10.0.0.0/29",
			})
			st := newProvisionTestStore(t, cfg)
			p := &Provisioner{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st}

			inst := provisionTestInstance(cfg, "demo", tt.state, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
			if tt.state == model.StateDeleted {
				deletedAt := inst.CreatedAt.Add(10 * time.Minute)
				inst.DeletedAt = &deletedAt
			}
			if err := st.CreateInstance(ctx, inst); err != nil {
				t.Fatalf("CreateInstance: %v", err)
			}

			stalePath := filepath.Join(cfg.InstancesDir(), inst.Name, "stale.txt")
			if err := os.MkdirAll(filepath.Dir(stalePath), 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := os.WriteFile(stalePath, []byte("old"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			instanceDir, err := p.prepareInstanceDir(ctx, inst.Name)
			if err != nil {
				t.Fatalf("prepareInstanceDir: %v", err)
			}
			if instanceDir != filepath.Join(cfg.InstancesDir(), inst.Name) {
				t.Fatalf("prepareInstanceDir returned %q, want %q", instanceDir, filepath.Join(cfg.InstancesDir(), inst.Name))
			}
			if _, err := os.Stat(instanceDir); err != nil {
				t.Fatalf("instance dir missing after prepare: %v", err)
			}
			if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
				t.Fatalf("stale file should be removed, stat err = %v", err)
			}

			_, found, err := st.FindInstance(ctx, inst.Name)
			if err != nil {
				t.Fatalf("FindInstance: %v", err)
			}
			if found {
				t.Fatalf("old metadata row still present after prepare")
			}
		})
	}
}

func TestPrepareInstanceDirRejectsActiveOrOrphanedNames(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st}

	active := provisionTestInstance(cfg, "busy", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	if err := st.CreateInstance(ctx, active); err != nil {
		t.Fatalf("CreateInstance(active): %v", err)
	}

	if _, err := p.prepareInstanceDir(ctx, active.Name); err == nil || !strings.Contains(err.Error(), `instance "busy" already exists with state ready`) {
		t.Fatalf("prepareInstanceDir(active) error = %v", err)
	}

	orphanDir := filepath.Join(cfg.InstancesDir(), "orphan")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(orphan): %v", err)
	}
	if _, err := p.prepareInstanceDir(ctx, "orphan"); err == nil || !strings.Contains(err.Error(), `instance "orphan" already exists on disk`) {
		t.Fatalf("prepareInstanceDir(orphan) error = %v", err)
	}
}

func TestInstanceDirRejectsUnsafeNames(t *testing.T) {
	cfg := loadProvisionTestConfig(t, nil)
	p := &Provisioner{cfg: cfg}

	if got, err := p.instanceDir("demo"); err != nil || got != filepath.Join(cfg.InstancesDir(), "demo") {
		t.Fatalf("instanceDir(demo) = (%q, %v)", got, err)
	}

	for _, name := range []string{"", ".", "..", "nested/demo", "../escape"} {
		if _, err := p.instanceDir(name); err == nil {
			t.Fatalf("instanceDir(%q) unexpectedly succeeded", name)
		}
	}
}

func TestRemoveInstanceDirDeletesOnlyCanonicalPath(t *testing.T) {
	cfg := loadProvisionTestConfig(t, nil)
	p := &Provisioner{cfg: cfg}

	instanceDir := filepath.Join(cfg.InstancesDir(), "demo")
	outsideDir := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(instanceDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(instanceDir): %v", err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(outsideDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(instanceDir, "rootfs.img"), []byte("vm"), 0o644); err != nil {
		t.Fatalf("WriteFile(instance rootfs): %v", err)
	}
	outsideFile := filepath.Join(outsideDir, "keep.txt")
	if err := os.WriteFile(outsideFile, []byte("keep"), 0o644); err != nil {
		t.Fatalf("WriteFile(outside): %v", err)
	}

	if err := p.removeInstanceDir("demo"); err != nil {
		t.Fatalf("removeInstanceDir(): %v", err)
	}
	if _, err := os.Stat(instanceDir); !os.IsNotExist(err) {
		t.Fatalf("instance dir should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(outsideFile); err != nil {
		t.Fatalf("outside file should remain, stat err = %v", err)
	}
	if err := p.removeInstanceDir("../escape"); err == nil {
		t.Fatalf("removeInstanceDir() accepted traversal name")
	}
	if _, err := os.Stat(outsideFile); err != nil {
		t.Fatalf("outside file should still remain after rejected traversal, stat err = %v", err)
	}
}

func TestAllocateNetworkSkipsDeletedSubnetsAndDetectsExhaustion(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, map[string]string{
		"SRV_VM_NETWORK_CIDR": "10.0.0.0/29",
	})
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st}

	used := provisionTestInstance(cfg, "used", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	used.NetworkCIDR = "10.0.0.0/30"
	used.HostAddr = "10.0.0.1/30"
	used.GuestAddr = "10.0.0.2/30"
	used.GatewayAddr = "10.0.0.1"

	deleted := provisionTestInstance(cfg, "deleted", model.StateDeleted, used.CreatedAt.Add(time.Minute))
	deleted.NetworkCIDR = "10.0.0.4/30"
	deleted.HostAddr = "10.0.0.5/30"
	deleted.GuestAddr = "10.0.0.6/30"
	deleted.GatewayAddr = "10.0.0.5"
	deletedAt := deleted.CreatedAt.Add(time.Minute)
	deleted.DeletedAt = &deletedAt

	if err := st.CreateInstance(ctx, used); err != nil {
		t.Fatalf("CreateInstance(used): %v", err)
	}
	if err := st.CreateInstance(ctx, deleted); err != nil {
		t.Fatalf("CreateInstance(deleted): %v", err)
	}

	networkCIDR, hostAddr, guestAddr, gateway, err := p.allocateNetwork(ctx)
	if err != nil {
		t.Fatalf("allocateNetwork: %v", err)
	}
	if networkCIDR != "10.0.0.4/30" || hostAddr != "10.0.0.5/30" || guestAddr != "10.0.0.6/30" || gateway != "10.0.0.5" {
		t.Fatalf("allocateNetwork returned (%q, %q, %q, %q)", networkCIDR, hostAddr, guestAddr, gateway)
	}

	reusedDeleted := deleted
	reusedDeleted.State = model.StateReady
	reusedDeleted.UpdatedAt = reusedDeleted.UpdatedAt.Add(time.Minute)
	reusedDeleted.DeletedAt = nil
	if err := st.UpdateInstance(ctx, reusedDeleted); err != nil {
		t.Fatalf("UpdateInstance(reusedDeleted): %v", err)
	}

	if _, _, _, _, err := p.allocateNetwork(ctx); err == nil || !strings.Contains(err.Error(), "no free /30 network blocks remain") {
		t.Fatalf("allocateNetwork() exhaustion error = %v", err)
	}
}

func TestWriteMetadataFileRedactsAuthKey(t *testing.T) {
	cfg := loadProvisionTestConfig(t, nil)
	p := &Provisioner{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: newProvisionTestStore(t, cfg)}
	inst := provisionTestInstance(cfg, "demo", model.StateProvisioning, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))

	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	bootstrap := guestBootstrap{
		Version:             1,
		Hostname:            "demo",
		TailscaleAuthKey:    "tskey-auth-secret",
		TailscaleControlURL: "https://control.example.com",
		TailscaleTags:       []string{"tag:microvm"},
	}

	if err := p.writeMetadataFile(inst, bootstrap); err != nil {
		t.Fatalf("writeMetadataFile: %v", err)
	}

	payload, err := os.ReadFile(filepath.Join(filepath.Dir(inst.RootFSPath), "meta.json"))
	if err != nil {
		t.Fatalf("ReadFile(meta.json): %v", err)
	}
	if strings.Contains(string(payload), bootstrap.TailscaleAuthKey) {
		t.Fatalf("metadata file leaked auth key: %s", payload)
	}

	var meta guestMetadata
	if err := json.Unmarshal(payload, &meta); err != nil {
		t.Fatalf("Unmarshal(meta.json): %v", err)
	}
	if meta.SRV.TailscaleAuthKey != "[redacted]" {
		t.Fatalf("TailscaleAuthKey = %q, want [redacted]", meta.SRV.TailscaleAuthKey)
	}
	if meta.SRV.Hostname != "demo" || !reflect.DeepEqual(meta.SRV.TailscaleTags, []string{"tag:microvm"}) {
		t.Fatalf("unexpected metadata payload: %#v", meta)
	}
}

func TestWaitForTailnetJoinFailsFastWhenGuestProcessExits(t *testing.T) {
	cfg := loadProvisionTestConfig(t, map[string]string{
		"SRV_GUEST_READY_TIMEOUT": "30s",
	})
	p := &Provisioner{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	_, _, err := p.waitForTailnetJoin(context.Background(), "demo", math.MaxInt32)
	if !errors.Is(err, errGuestExited) {
		t.Fatalf("waitForTailnetJoin() error = %v, want %v", err, errGuestExited)
	}
}

func TestBaseRootFSInUse(t *testing.T) {
	cfg := loadProvisionTestConfig(t, nil)
	p := &Provisioner{cfg: cfg}

	oldLoopDevicesForPath := loopDevicesForPath
	t.Cleanup(func() { loopDevicesForPath = oldLoopDevicesForPath })

	loopDevicesForPath = func(path string) (string, error) {
		if path != cfg.BaseRootFSPath {
			t.Fatalf("loopDevicesForPath called with %q, want %q", path, cfg.BaseRootFSPath)
		}
		return "/dev/loop7", nil
	}
	inUse, err := p.baseRootFSInUse()
	if err != nil {
		t.Fatalf("baseRootFSInUse() error = %v", err)
	}
	if !inUse {
		t.Fatalf("baseRootFSInUse() = false, want true")
	}

	loopDevicesForPath = func(string) (string, error) {
		return "", nil
	}
	inUse, err = p.baseRootFSInUse()
	if err != nil {
		t.Fatalf("baseRootFSInUse() error = %v", err)
	}
	if inUse {
		t.Fatalf("baseRootFSInUse() = true, want false")
	}
}

func TestEnsureCreatePrereqsChecksReflinkCloneability(t *testing.T) {
	cfg := loadProvisionTestConfig(t, map[string]string{
		"SRV_GUEST_AUTH_TAGS": "tag:microvm",
	})
	if err := os.MkdirAll(cfg.ImagesDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll(images): %v", err)
	}
	kernelPath := filepath.Join(cfg.ImagesDir(), "vmlinux")
	rootfsPath := filepath.Join(cfg.ImagesDir(), "rootfs.img")
	if err := os.WriteFile(kernelPath, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("WriteFile(kernel): %v", err)
	}
	if err := os.WriteFile(rootfsPath, []byte("rootfs"), 0o644); err != nil {
		t.Fatalf("WriteFile(rootfs): %v", err)
	}
	cfg.BaseKernelPath = kernelPath
	cfg.BaseRootFSPath = rootfsPath
	oldLoopDevicesForPath := loopDevicesForPath
	oldReflinkCloneFile := reflinkCloneFile
	t.Cleanup(func() {
		loopDevicesForPath = oldLoopDevicesForPath
		reflinkCloneFile = oldReflinkCloneFile
	})
	loopDevicesForPath = func(string) (string, error) {
		return "", nil
	}

	t.Run("accepts reflink-capable storage", func(t *testing.T) {
		var src, dest string
		reflinkCloneFile = func(_ context.Context, gotSrc, gotDest string) error {
			src = gotSrc
			dest = gotDest
			return nil
		}
		p := &Provisioner{cfg: cfg, tsClient: &tailscale.Client{}}
		if err := p.ensureCreatePrereqs(context.Background(), false); err != nil {
			t.Fatalf("ensureCreatePrereqs() error = %v", err)
		}
		if src != cfg.BaseRootFSPath {
			t.Fatalf("reflinkCloneFile src = %q, want %q", src, cfg.BaseRootFSPath)
		}
		if !strings.HasPrefix(dest, cfg.DataDirAbs()+string(os.PathSeparator)) {
			t.Fatalf("reflinkCloneFile dest = %q, want path under %q", dest, cfg.DataDirAbs())
		}
	})

	t.Run("rejects storage without reflink support", func(t *testing.T) {
		reflinkCloneFile = func(context.Context, string, string) error {
			return errors.New("operation not supported")
		}
		p := &Provisioner{cfg: cfg, tsClient: &tailscale.Client{}}
		err := p.ensureCreatePrereqs(context.Background(), false)
		if err == nil || !strings.Contains(err.Error(), "must be reflink-cloneable into data dir") {
			t.Fatalf("ensureCreatePrereqs() error = %v", err)
		}
	})
}

func TestResolveCreateOptions(t *testing.T) {
	cfg := loadProvisionTestConfig(t, nil)
	p := &Provisioner{cfg: cfg}

	resolved, needsResize, err := p.resolveCreateOptions(CreateOptions{VCPUCount: 4, MemoryMiB: 2048, RootFSSizeBytes: 8 << 30}, 4<<30)
	if err != nil {
		t.Fatalf("resolveCreateOptions() error = %v", err)
	}
	if resolved.VCPUCount != 4 || resolved.MemoryMiB != 2048 || resolved.RootFSSizeBytes != 8<<30 {
		t.Fatalf("resolveCreateOptions() = %#v", resolved)
	}
	if !needsResize {
		t.Fatalf("resolveCreateOptions() needsResize = false, want true")
	}

	resolved, needsResize, err = p.resolveCreateOptions(CreateOptions{}, 4<<30)
	if err != nil {
		t.Fatalf("resolveCreateOptions(defaults) error = %v", err)
	}
	if resolved.VCPUCount != cfg.VCPUCount || resolved.MemoryMiB != cfg.MemoryMiB || resolved.RootFSSizeBytes != 4<<30 {
		t.Fatalf("resolveCreateOptions(defaults) = %#v", resolved)
	}
	if needsResize {
		t.Fatalf("resolveCreateOptions(defaults) needsResize = true, want false")
	}

	if _, _, err := p.resolveCreateOptions(CreateOptions{RootFSSizeBytes: (4 << 30) - 1}, 4<<30); err == nil || !strings.Contains(err.Error(), "smaller than the base image size") {
		t.Fatalf("resolveCreateOptions(smaller rootfs) error = %v", err)
	}
	if _, _, err := p.resolveCreateOptions(CreateOptions{VCPUCount: 3}, 4<<30); err == nil || !strings.Contains(err.Error(), "vm vcpu count must be 1 or an even number") {
		t.Fatalf("resolveCreateOptions(odd vcpus) error = %v", err)
	}
	if _, _, err := p.resolveCreateOptions(CreateOptions{VCPUCount: config.MaxVCPUCount + 1}, 4<<30); err == nil || !strings.Contains(err.Error(), "vm vcpu count must be <= 32") {
		t.Fatalf("resolveCreateOptions(too many vcpus) error = %v", err)
	}
}

func TestEffectiveMachineConfigUsesInstanceOverrides(t *testing.T) {
	cfg := loadProvisionTestConfig(t, nil)
	p := &Provisioner{cfg: cfg}
	inst := provisionTestInstance(cfg, "demo", model.StateStopped, time.Now().UTC())
	inst.VCPUCount = 4
	inst.MemoryMiB = 4096

	if got := p.effectiveVCPUCount(inst); got != 4 {
		t.Fatalf("effectiveVCPUCount() = %d, want 4", got)
	}
	if got := p.effectiveMemoryMiB(inst); got != 4096 {
		t.Fatalf("effectiveMemoryMiB() = %d, want 4096", got)
	}

	inst.VCPUCount = 0
	inst.MemoryMiB = 0
	if got := p.effectiveVCPUCount(inst); got != cfg.VCPUCount {
		t.Fatalf("effectiveVCPUCount() fallback = %d, want %d", got, cfg.VCPUCount)
	}
	if got := p.effectiveMemoryMiB(inst); got != cfg.MemoryMiB {
		t.Fatalf("effectiveMemoryMiB() fallback = %d, want %d", got, cfg.MemoryMiB)
	}
}

func TestEnsureInstanceRuntimePermissions(t *testing.T) {
	cfg := loadProvisionTestConfig(t, nil)
	p := &Provisioner{cfg: cfg}
	inst := provisionTestInstance(cfg, "demo", model.StateStopped, time.Now().UTC())

	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(instance dir): %v", err)
	}
	if err := os.WriteFile(inst.RootFSPath, []byte("rootfs"), 0o644); err != nil {
		t.Fatalf("WriteFile(rootfs): %v", err)
	}
	if err := os.WriteFile(inst.LogPath, []byte("log"), 0o644); err != nil {
		t.Fatalf("WriteFile(log): %v", err)
	}
	if err := os.WriteFile(inst.SerialLogPath, []byte("serial"), 0o644); err != nil {
		t.Fatalf("WriteFile(serial): %v", err)
	}

	if err := p.ensureInstanceRuntimePermissions(inst); err != nil {
		t.Fatalf("ensureInstanceRuntimePermissions(): %v", err)
	}

	assertMode := func(path string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s): %v", path, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("mode for %s = %o, want %o", path, got, want)
		}
	}

	assertMode(filepath.Dir(inst.RootFSPath), 0o770)
	assertMode(inst.RootFSPath, 0o660)
	assertMode(inst.LogPath, 0o644)
	assertMode(inst.SerialLogPath, 0o644)
}

func TestEnsureStartPrereqsRequiresCompletedBootstrap(t *testing.T) {
	firecrackerBin := filepath.Join(t.TempDir(), "bin", "firecracker")
	cfg := loadProvisionTestConfig(t, map[string]string{
		"SRV_FIRECRACKER_BIN": firecrackerBin,
	})
	p := &Provisioner{cfg: cfg}
	inst := provisionTestInstance(cfg, "demo", model.StateStopped, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	p.applyConfiguredBootArtifacts(&inst)

	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(instance dir): %v", err)
	}
	for _, path := range []string{inst.RootFSPath, inst.KernelPath, inst.InitrdPath, cfg.FirecrackerBinary} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}

	err := p.ensureStartPrereqs(inst)
	if err == nil || !strings.Contains(err.Error(), "has not completed initial tailnet bootstrap") {
		t.Fatalf("ensureStartPrereqs() error = %v", err)
	}

	p.tsClient = &tailscale.Client{}
	inst.TailscaleName = "demo.tailnet"
	if err := p.ensureStartPrereqs(inst); err != nil {
		t.Fatalf("ensureStartPrereqs() with prior tailnet identity: %v", err)
	}
}

func TestEnsureStartPrereqsUsesCurrentBaseKernelPath(t *testing.T) {
	currentKernel := filepath.Join(t.TempDir(), "images", "current-vmlinux")
	cfg := loadProvisionTestConfig(t, map[string]string{
		"SRV_BASE_KERNEL": currentKernel,
	})
	p := &Provisioner{cfg: cfg, tsClient: &tailscale.Client{}}
	inst := provisionTestInstance(cfg, "demo", model.StateStopped, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	inst.KernelPath = filepath.Join(t.TempDir(), "images", "old-vmlinux")
	inst.TailscaleName = "demo.tailnet"
	p.applyConfiguredBootArtifacts(&inst)

	for _, path := range []string{inst.RootFSPath, inst.KernelPath, inst.InitrdPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}

	if err := p.ensureStartPrereqs(inst); err != nil {
		t.Fatalf("ensureStartPrereqs() with refreshed base kernel: %v", err)
	}

	if inst.KernelPath != currentKernel {
		t.Fatalf("applyConfiguredBootArtifacts() kernel path = %q, want %q", inst.KernelPath, currentKernel)
	}
}

func TestResizeStoppedInstanceUpdatesStoredConfigAndGrowsRootFS(t *testing.T) {
	const testMiB = int64(1024 * 1024)

	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st}

	inst := provisionTestInstance(cfg, "demo", model.StateStopped, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	inst.RootFSSizeBytes = 8 * testMiB
	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(rootfs dir): %v", err)
	}
	if err := os.WriteFile(inst.RootFSPath, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(rootfs): %v", err)
	}
	if err := os.Truncate(inst.RootFSPath, inst.RootFSSizeBytes); err != nil {
		t.Fatalf("Truncate(rootfs): %v", err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(bin): %v", err)
	}
	resize2fs := filepath.Join(binDir, "resize2fs")
	if err := os.WriteFile(resize2fs, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(resize2fs): %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	resized, err := p.Resize(ctx, inst.Name, CreateOptions{VCPUCount: 4, MemoryMiB: 4096, RootFSSizeBytes: 12 * testMiB})
	if err != nil {
		t.Fatalf("Resize(): %v", err)
	}
	if resized.VCPUCount != 4 || resized.MemoryMiB != 4096 || resized.RootFSSizeBytes != 12*testMiB {
		t.Fatalf("Resize() returned %#v", resized)
	}

	info, err := os.Stat(inst.RootFSPath)
	if err != nil {
		t.Fatalf("Stat(rootfs): %v", err)
	}
	if info.Size() != 12*testMiB {
		t.Fatalf("rootfs size after resize = %d, want %d", info.Size(), 12*testMiB)
	}

	stored, err := st.GetInstance(ctx, inst.Name)
	if err != nil {
		t.Fatalf("GetInstance(): %v", err)
	}
	if stored.VCPUCount != 4 || stored.MemoryMiB != 4096 || stored.RootFSSizeBytes != 12*testMiB {
		t.Fatalf("stored instance = %#v", stored)
	}

	events, err := st.ListEvents(ctx, inst.ID, 10)
	if err != nil {
		t.Fatalf("ListEvents(): %v", err)
	}
	var sawResize bool
	var sawStorage bool
	for _, evt := range events {
		if evt.Type == "resize" && evt.Message == "instance config updated" {
			sawResize = true
		}
		if evt.Type == "storage" && evt.Message == "rootfs expanded for instance" {
			sawStorage = true
		}
	}
	if !sawResize || !sawStorage {
		t.Fatalf("expected resize and storage events, got %#v", events)
	}
}

func TestResizeRejectsShrinkingRootFS(t *testing.T) {
	const testMiB = int64(1024 * 1024)

	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st}

	inst := provisionTestInstance(cfg, "demo", model.StateStopped, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	inst.RootFSSizeBytes = 8 * testMiB
	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(rootfs dir): %v", err)
	}
	if err := os.WriteFile(inst.RootFSPath, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(rootfs): %v", err)
	}
	if err := os.Truncate(inst.RootFSPath, inst.RootFSSizeBytes); err != nil {
		t.Fatalf("Truncate(rootfs): %v", err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	_, err := p.Resize(ctx, inst.Name, CreateOptions{RootFSSizeBytes: 4 * testMiB})
	if err == nil || !strings.Contains(err.Error(), "smaller than the current image size") {
		t.Fatalf("Resize() shrink error = %v", err)
	}

	info, err := os.Stat(inst.RootFSPath)
	if err != nil {
		t.Fatalf("Stat(rootfs): %v", err)
	}
	if info.Size() != 8*testMiB {
		t.Fatalf("rootfs size changed after failed shrink: %d", info.Size())
	}
}

func TestDeviceUpdatedSince(t *testing.T) {
	previous := tailnetDeviceSnapshot{DeviceID: "device-1", LastSeen: "2026-03-29T12:00:00Z"}
	if deviceUpdatedSince(tailscale.Device{DeviceID: "device-1", LastSeen: previous.LastSeen}, previous, true) {
		t.Fatalf("deviceUpdatedSince() reported unchanged device as updated")
	}
	if !deviceUpdatedSince(tailscale.Device{DeviceID: "device-1", LastSeen: "2026-03-29T12:01:00Z"}, previous, true) {
		t.Fatalf("deviceUpdatedSince() should treat newer last-seen as updated")
	}
	if !deviceUpdatedSince(tailscale.Device{DeviceID: "device-2", LastSeen: previous.LastSeen}, previous, true) {
		t.Fatalf("deviceUpdatedSince() should treat a new device ID as updated")
	}
	if !deviceUpdatedSince(tailscale.Device{DeviceID: "device-1"}, tailnetDeviceSnapshot{}, false) {
		t.Fatalf("deviceUpdatedSince() should accept the first matching device when no previous snapshot exists")
	}
}

func TestProcessExistsTreatsPermissionDeniedAsRunning(t *testing.T) {
	oldSignalProcess := signalProcess
	t.Cleanup(func() {
		signalProcess = oldSignalProcess
	})

	signalProcess = func(pid int, sig syscall.Signal) error {
		if pid != 4321 {
			t.Fatalf("signalProcess pid = %d, want 4321", pid)
		}
		if sig != 0 {
			t.Fatalf("signalProcess signal = %d, want 0", sig)
		}
		return syscall.EPERM
	}
	if !processExists(4321) {
		t.Fatal("processExists() = false, want true for EPERM")
	}

	signalProcess = func(pid int, sig syscall.Signal) error {
		return syscall.ESRCH
	}
	if processExists(4321) {
		t.Fatal("processExists() = true, want false for ESRCH")
	}
}

func TestShouldAutoStartAfterStartup(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{state: model.StateReady, want: true},
		{state: model.StateProvisioning, want: true},
		{state: model.StateAwaitingTailnet, want: true},
		{state: model.StateStopped, want: false},
		{state: model.StateFailed, want: false},
		{state: model.StateDeleted, want: false},
	}
	for _, tt := range tests {
		inst := model.Instance{State: tt.state}
		if got := shouldAutoStartAfterStartup(inst); got != tt.want {
			t.Fatalf("shouldAutoStartAfterStartup(%q) = %v, want %v", tt.state, got, tt.want)
		}
	}
}

func TestHelperFunctions(t *testing.T) {
	valid := []string{"demo", "demo-1", strings.Repeat("a", 63)}
	invalid := []string{"Demo", "-demo", "demo-", "demo_1", strings.Repeat("a", 64)}
	for _, name := range valid {
		if !validName.MatchString(name) {
			t.Fatalf("validName rejected %q", name)
		}
	}
	for _, name := range invalid {
		if validName.MatchString(name) {
			t.Fatalf("validName accepted %q", name)
		}
	}

	if got := tapName("demo"); got != tapName("demo") || len(got) != 14 || !strings.HasPrefix(got, "tap-") {
		t.Fatalf("tapName(demo) = %q", got)
	}
	if matched, _ := regexp.MatchString(`^02:fc:[0-9a-f]{2}:[0-9a-f]{2}:[0-9a-f]{2}:[0-9a-f]{2}$`, guestMAC("demo")); !matched {
		t.Fatalf("guestMAC(demo) did not match expected format")
	}
	if got := kernelArgs("quiet loglevel=3"); got != "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw quiet loglevel=3" {
		t.Fatalf("kernelArgs() = %q", got)
	}
	if got, err := directChildPath("/tmp/srv", "demo"); err != nil || got != "/tmp/srv/demo" {
		t.Fatalf("directChildPath() = (%q, %v)", got, err)
	}
	for _, name := range []string{"", ".", "..", "nested/demo"} {
		if _, err := directChildPath("/tmp/srv", name); err == nil {
			t.Fatalf("directChildPath(%q) unexpectedly succeeded", name)
		}
	}
	if got := firstNonEmpty("", "  ", "value", "other"); got != "value" {
		t.Fatalf("firstNonEmpty() = %q, want value", got)
	}
	if got := prefixBeforeDot("demo.tailnet.example"); got != "demo" {
		t.Fatalf("prefixBeforeDot() = %q, want demo", got)
	}
	if got := trimDot("demo."); got != "demo" {
		t.Fatalf("trimDot() = %q, want demo", got)
	}
	if got := uint32ToIP(0x0a000001).String(); got != "10.0.0.1" {
		t.Fatalf("uint32ToIP() = %q, want 10.0.0.1", got)
	}
}

func loadProvisionTestConfig(t *testing.T, env map[string]string) config.Config {
	t.Helper()
	oldArgs := os.Args
	oldCommandLine := flag.CommandLine

	fs := flag.NewFlagSet("srv.test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flag.CommandLine = fs
	os.Args = []string{"srv.test"}
	t.Cleanup(func() {
		flag.CommandLine = oldCommandLine
		os.Args = oldArgs
	})

	dataDir := t.TempDir()
	t.Setenv("SRV_DATA_DIR", dataDir)
	t.Setenv("SRV_VM_NETWORK_CIDR", "10.0.0.0/29")
	for key, value := range env {
		t.Setenv(key, value)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load(): %v", err)
	}
	return cfg
}

func newProvisionTestStore(t *testing.T, cfg config.Config) *store.Store {
	t.Helper()
	st, err := store.Open(cfg.DatabasePath())
	if err != nil {
		t.Fatalf("store.Open(%q): %v", cfg.DatabasePath(), err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
	})
	return st
}

func provisionTestInstance(cfg config.Config, name, state string, createdAt time.Time) model.Instance {
	instanceDir := filepath.Join(cfg.InstancesDir(), name)
	return model.Instance{
		ID:              name + "-id",
		Name:            name,
		State:           state,
		CreatedAt:       createdAt,
		UpdatedAt:       createdAt.Add(30 * time.Second),
		CreatedByUser:   "alice@example.com",
		CreatedByNode:   "laptop",
		VCPUCount:       cfg.VCPUCount,
		MemoryMiB:       cfg.MemoryMiB,
		RootFSSizeBytes: 4 << 30,
		RootFSPath:      filepath.Join(instanceDir, "rootfs.img"),
		KernelPath:      filepath.Join(cfg.ImagesDir(), "vmlinux"),
		InitrdPath:      filepath.Join(cfg.ImagesDir(), "initrd.img"),
		SocketPath:      filepath.Join(instanceDir, "firecracker.sock"),
		LogPath:         filepath.Join(instanceDir, "firecracker.log"),
		SerialLogPath:   filepath.Join(instanceDir, "serial.log"),
		TapDevice:       "tap-1234567890",
		GuestMAC:        "02:fc:aa:bb:cc:dd",
		NetworkCIDR:     "10.0.0.0/30",
		HostAddr:        "10.0.0.1/30",
		GuestAddr:       "10.0.0.2/30",
		GatewayAddr:     "10.0.0.1",
	}
}
