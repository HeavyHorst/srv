package provision

import (
	"archive/tar"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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

	"tailscale.com/client/tailscale/v2"

	"srv/internal/config"
	"srv/internal/format"
	"srv/internal/host"
	"srv/internal/model"
	"srv/internal/nethelper"
	"srv/internal/storage"
	"srv/internal/store"
	"srv/internal/vmrunner"
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
	p := &Provisioner{
		cfg:                 cfg,
		log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:               st,
		readFilesystemBytes: host.DefaultReadFilesystemBytes,
	}

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

func TestDeleteDoesNotMarkInstanceDeletedWhenDiskCleanupFails(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{
		cfg:      cfg,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:    st,
		vmRunner: noopVMRunner{},
	}

	inst := provisionTestInstance(cfg, "demo", model.StateStopped, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(instance dir): %v", err)
	}
	if err := os.WriteFile(inst.RootFSPath, []byte("vm"), 0o644); err != nil {
		t.Fatalf("WriteFile(rootfs): %v", err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance(): %v", err)
	}

	oldRemovePathAll := removePathAll
	removePathAll = func(string) error {
		return errors.New("disk busy")
	}
	t.Cleanup(func() {
		removePathAll = oldRemovePathAll
	})

	deletedInst, err := p.Delete(ctx, inst.Name)
	if err == nil || !strings.Contains(err.Error(), `remove instance directory for "demo"`) {
		t.Fatalf("Delete() error = %v", err)
	}
	if deletedInst.State != model.StateDeleting {
		t.Fatalf("Delete() state = %q, want %q", deletedInst.State, model.StateDeleting)
	}
	if deletedInst.DeletedAt != nil {
		t.Fatalf("Delete() DeletedAt = %v, want nil", deletedInst.DeletedAt)
	}
	if !strings.Contains(deletedInst.LastError, "delete failed:") {
		t.Fatalf("Delete() LastError = %q", deletedInst.LastError)
	}

	stored, getErr := st.GetInstance(ctx, inst.Name)
	if getErr != nil {
		t.Fatalf("GetInstance(): %v", getErr)
	}
	if stored.State != model.StateDeleting {
		t.Fatalf("stored state = %q, want %q", stored.State, model.StateDeleting)
	}
	if stored.DeletedAt != nil {
		t.Fatalf("stored DeletedAt = %v, want nil", stored.DeletedAt)
	}
	if !strings.Contains(stored.LastError, "delete failed:") {
		t.Fatalf("stored LastError = %q", stored.LastError)
	}
	if _, statErr := os.Stat(inst.RootFSPath); statErr != nil {
		t.Fatalf("rootfs should remain after failed delete, stat err = %v", statErr)
	}
}

func TestAllocateNetworkSkipsDeletedSubnetsAndDetectsExhaustion(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, map[string]string{
		"SRV_VM_NETWORK_CIDR": "10.0.0.0/29",
	})
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{
		cfg:                 cfg,
		log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:               st,
		readFilesystemBytes: host.DefaultReadFilesystemBytes,
	}

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
		ZenGatewayPort:      11434,
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
	if meta.SRV.ZenGatewayPort != 11434 {
		t.Fatalf("ZenGatewayPort = %d, want 11434", meta.SRV.ZenGatewayPort)
	}
	if meta.SRV.Hostname != "demo" || !reflect.DeepEqual(meta.SRV.TailscaleTags, []string{"tag:microvm"}) {
		t.Fatalf("unexpected metadata payload: %#v", meta)
	}
}

func TestCreateListAndRestoreBackup(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{
		cfg:                 cfg,
		log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:               st,
		readFilesystemBytes: host.DefaultReadFilesystemBytes,
	}

	oldReflinkCloneFile := reflinkCloneFile
	t.Cleanup(func() {
		reflinkCloneFile = oldReflinkCloneFile
	})
	reflinkCloneFile = func(_ context.Context, src, dest string) error {
		payload, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, payload, 0o644)
	}

	inst := provisionTestInstance(cfg, "demo", model.StateStopped, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	inst.TailscaleName = "demo.tailnet"
	inst.TailscaleIP = "100.64.0.10"
	inst.RootFSSizeBytes = 12 << 20
	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(instance dir): %v", err)
	}
	if err := os.WriteFile(inst.RootFSPath, []byte("rootfs-v1"), 0o644); err != nil {
		t.Fatalf("WriteFile(rootfs): %v", err)
	}
	if err := os.WriteFile(inst.SerialLogPath, []byte("serial-v1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(serial): %v", err)
	}
	if err := os.WriteFile(inst.LogPath, []byte("firecracker-v1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(firecracker): %v", err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance(): %v", err)
	}

	backup, err := p.CreateBackup(ctx, inst.Name)
	if err != nil {
		t.Fatalf("CreateBackup(): %v", err)
	}
	if backup.Name != inst.Name {
		t.Fatalf("CreateBackup() name = %q, want %q", backup.Name, inst.Name)
	}
	if _, err := os.Stat(filepath.Join(backup.Path, backupManifestName)); err != nil {
		t.Fatalf("Stat(manifest): %v", err)
	}

	backups, err := p.ListBackups(ctx, inst.Name)
	if err != nil {
		t.Fatalf("ListBackups(): %v", err)
	}
	if len(backups) != 1 || backups[0].ID != backup.ID {
		t.Fatalf("ListBackups() = %#v", backups)
	}

	mutated := inst
	mutated.VCPUCount = 4
	mutated.MemoryMiB = 4096
	mutated.RootFSSizeBytes = 20 << 20
	mutated.UpdatedAt = inst.UpdatedAt.Add(time.Minute)
	if err := st.UpdateInstance(ctx, mutated); err != nil {
		t.Fatalf("UpdateInstance(): %v", err)
	}
	if err := os.WriteFile(inst.RootFSPath, []byte("rootfs-v2"), 0o644); err != nil {
		t.Fatalf("WriteFile(rootfs v2): %v", err)
	}
	if err := os.WriteFile(inst.SerialLogPath, []byte("serial-v2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(serial v2): %v", err)
	}
	if err := os.WriteFile(inst.LogPath, []byte("firecracker-v2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(firecracker v2): %v", err)
	}

	restored, restoredBackup, err := p.RestoreBackup(ctx, inst.Name, backup.ID)
	if err != nil {
		t.Fatalf("RestoreBackup(): %v", err)
	}
	if restoredBackup.ID != backup.ID {
		t.Fatalf("RestoreBackup() backup ID = %q, want %q", restoredBackup.ID, backup.ID)
	}
	if restored.State != model.StateStopped {
		t.Fatalf("RestoreBackup() state = %q, want %q", restored.State, model.StateStopped)
	}
	if restored.VCPUCount != inst.VCPUCount || restored.MemoryMiB != inst.MemoryMiB || restored.RootFSSizeBytes != inst.RootFSSizeBytes {
		t.Fatalf("RestoreBackup() restored instance = %#v, want original sizing %#v", restored, inst)
	}

	payload, err := os.ReadFile(inst.RootFSPath)
	if err != nil {
		t.Fatalf("ReadFile(rootfs): %v", err)
	}
	if string(payload) != "rootfs-v1" {
		t.Fatalf("rootfs contents = %q, want %q", string(payload), "rootfs-v1")
	}
	serialPayload, err := os.ReadFile(inst.SerialLogPath)
	if err != nil {
		t.Fatalf("ReadFile(serial): %v", err)
	}
	if string(serialPayload) != "serial-v1\n" {
		t.Fatalf("serial log contents = %q, want %q", string(serialPayload), "serial-v1\n")
	}
	fcPayload, err := os.ReadFile(inst.LogPath)
	if err != nil {
		t.Fatalf("ReadFile(firecracker): %v", err)
	}
	if string(fcPayload) != "firecracker-v1\n" {
		t.Fatalf("firecracker log contents = %q, want %q", string(fcPayload), "firecracker-v1\n")
	}

	stored, err := st.GetInstance(ctx, inst.Name)
	if err != nil {
		t.Fatalf("GetInstance(): %v", err)
	}
	if stored.VCPUCount != inst.VCPUCount || stored.MemoryMiB != inst.MemoryMiB || stored.RootFSSizeBytes != inst.RootFSSizeBytes {
		t.Fatalf("stored instance after restore = %#v", stored)
	}
	if stored.FirecrackerPID != 0 {
		t.Fatalf("stored firecracker pid = %d, want 0", stored.FirecrackerPID)
	}
	info, err := os.Stat(inst.RootFSPath)
	if err != nil {
		t.Fatalf("Stat(rootfs): %v", err)
	}
	if got := info.Mode().Perm(); got != 0o660 {
		t.Fatalf("rootfs mode after restore = %o, want 660", got)
	}
	for _, path := range []string{inst.SerialLogPath, inst.LogPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s): %v", path, err)
		}
		if got := info.Mode().Perm(); got != 0o660 {
			t.Fatalf("mode for %s after restore = %o, want 660", path, got)
		}
	}

	events, err := st.ListEvents(ctx, inst.ID, 10)
	if err != nil {
		t.Fatalf("ListEvents(): %v", err)
	}
	var sawBackupCreate bool
	var sawBackupRestore bool
	for _, evt := range events {
		if evt.Type == "backup" && evt.Message == "instance backup created" {
			sawBackupCreate = true
		}
		if evt.Type == "backup" && evt.Message == "instance restored from backup" {
			sawBackupRestore = true
		}
	}
	if !sawBackupCreate || !sawBackupRestore {
		t.Fatalf("expected backup create and restore events, got %#v", events)
	}
}

func TestExportAndImportPortableArtifact(t *testing.T) {
	ctx := context.Background()
	sourceKernel := filepath.Join(t.TempDir(), "source-vmlinux")
	sourceInitrd := filepath.Join(t.TempDir(), "source-initrd.img")
	destKernel := filepath.Join(t.TempDir(), "dest-vmlinux")
	destInitrd := filepath.Join(t.TempDir(), "dest-initrd.img")
	for _, path := range []string{sourceKernel, sourceInitrd, destKernel, destInitrd} {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}

	sourceCfg := loadProvisionTestConfig(t, map[string]string{
		"SRV_BASE_KERNEL": sourceKernel,
		"SRV_BASE_INITRD": sourceInitrd,
	})
	destCfg := loadProvisionTestConfig(t, map[string]string{
		"SRV_BASE_KERNEL":     destKernel,
		"SRV_BASE_INITRD":     destInitrd,
		"SRV_VM_NETWORK_CIDR": "10.1.0.0/29",
	})
	sourceStore := newProvisionTestStore(t, sourceCfg)
	destStore := newProvisionTestStore(t, destCfg)
	source := &Provisioner{
		cfg:                 sourceCfg,
		log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:               sourceStore,
		readFilesystemBytes: host.DefaultReadFilesystemBytes,
	}
	dest := &Provisioner{
		cfg:                 destCfg,
		log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:               destStore,
		readFilesystemBytes: host.DefaultReadFilesystemBytes,
	}
	for _, dir := range []string{sourceCfg.InstancesDir(), destCfg.InstancesDir()} {
		if err := os.MkdirAll(dir, 0o770); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}

	rootfsPayload := bytes.Repeat([]byte("rootfs-data\n"), 32)
	serialPayload := []byte("serial-data\n")
	fcPayload := []byte("firecracker-data\n")
	sourceInst := provisionTestInstance(sourceCfg, "demo", model.StateStopped, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	sourceInst.VCPUCount = 4
	sourceInst.MemoryMiB = 4096
	sourceInst.RootFSSizeBytes = int64(len(rootfsPayload))
	sourceInst.TailscaleName = "demo.tailnet"
	sourceInst.TailscaleIP = "100.64.0.10"
	if err := os.MkdirAll(filepath.Dir(sourceInst.RootFSPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(source instance dir): %v", err)
	}
	if err := os.WriteFile(sourceInst.RootFSPath, rootfsPayload, 0o644); err != nil {
		t.Fatalf("WriteFile(rootfs): %v", err)
	}
	if err := os.WriteFile(sourceInst.SerialLogPath, serialPayload, 0o644); err != nil {
		t.Fatalf("WriteFile(serial): %v", err)
	}
	if err := os.WriteFile(sourceInst.LogPath, fcPayload, 0o644); err != nil {
		t.Fatalf("WriteFile(firecracker): %v", err)
	}
	if err := sourceStore.CreateInstance(ctx, sourceInst); err != nil {
		t.Fatalf("CreateInstance(source): %v", err)
	}

	var stream bytes.Buffer
	exportedInfo, err := source.ExportInstance(ctx, sourceInst.Name, &stream)
	if err != nil {
		t.Fatalf("ExportInstance(): %v", err)
	}
	if exportedInfo.Name != sourceInst.Name {
		t.Fatalf("ExportInstance() name = %q, want %q", exportedInfo.Name, sourceInst.Name)
	}

	progressByFile := make(map[string]ImportProgress)
	imported, importedInfo, err := dest.ImportInstance(
		ctx,
		model.Actor{UserLogin: "alice@example.com", NodeName: "workstation"},
		bytes.NewReader(stream.Bytes()),
		func(progress ImportProgress) {
			progressByFile[progress.Name] = progress
		},
	)
	if err != nil {
		t.Fatalf("ImportInstance(): %v", err)
	}
	if importedInfo.Name != sourceInst.Name {
		t.Fatalf("ImportInstance() source name = %q, want %q", importedInfo.Name, sourceInst.Name)
	}
	if imported.Name != sourceInst.Name {
		t.Fatalf("ImportInstance() target name = %q, want %q", imported.Name, sourceInst.Name)
	}
	if imported.ID != sourceInst.ID {
		t.Fatalf("ImportInstance() id = %q, want %q", imported.ID, sourceInst.ID)
	}
	if imported.State != model.StateStopped {
		t.Fatalf("ImportInstance() state = %q, want %q", imported.State, model.StateStopped)
	}
	if !imported.CreatedAt.Equal(sourceInst.CreatedAt) {
		t.Fatalf("ImportInstance() created_at = %s, want %s", imported.CreatedAt, sourceInst.CreatedAt)
	}
	if imported.CreatedByUser != sourceInst.CreatedByUser || imported.CreatedByNode != sourceInst.CreatedByNode {
		t.Fatalf("ImportInstance() creator = %q/%q, want %q/%q", imported.CreatedByUser, imported.CreatedByNode, sourceInst.CreatedByUser, sourceInst.CreatedByNode)
	}
	if imported.VCPUCount != sourceInst.VCPUCount || imported.MemoryMiB != sourceInst.MemoryMiB || imported.RootFSSizeBytes != sourceInst.RootFSSizeBytes {
		t.Fatalf("ImportInstance() sizing = %#v, want %#v", imported, sourceInst)
	}
	if imported.TailscaleName != sourceInst.TailscaleName || imported.TailscaleIP != sourceInst.TailscaleIP {
		t.Fatalf("ImportInstance() tailscale fields = %#v, want %#v", imported, sourceInst)
	}
	if imported.RootFSPath == sourceInst.RootFSPath || !strings.HasPrefix(imported.RootFSPath, destCfg.InstancesDir()+string(os.PathSeparator)) {
		t.Fatalf("ImportInstance() rootfs path = %q, want path under %q", imported.RootFSPath, destCfg.InstancesDir())
	}
	if imported.KernelPath != destKernel || imported.InitrdPath != destInitrd {
		t.Fatalf("ImportInstance() runtime paths = %q/%q, want %q/%q", imported.KernelPath, imported.InitrdPath, destKernel, destInitrd)
	}
	if imported.NetworkCIDR != "10.1.0.0/30" || imported.HostAddr != "10.1.0.1/30" || imported.GuestAddr != "10.1.0.2/30" || imported.GatewayAddr != "10.1.0.1" {
		t.Fatalf("ImportInstance() network = %q %q %q %q", imported.NetworkCIDR, imported.HostAddr, imported.GuestAddr, imported.GatewayAddr)
	}

	gotRootFS, err := os.ReadFile(imported.RootFSPath)
	if err != nil {
		t.Fatalf("ReadFile(imported rootfs): %v", err)
	}
	if !bytes.Equal(gotRootFS, rootfsPayload) {
		t.Fatalf("imported rootfs payload mismatch")
	}
	gotSerial, err := os.ReadFile(imported.SerialLogPath)
	if err != nil {
		t.Fatalf("ReadFile(imported serial): %v", err)
	}
	if !bytes.Equal(gotSerial, serialPayload) {
		t.Fatalf("imported serial payload mismatch")
	}
	gotFirecracker, err := os.ReadFile(imported.LogPath)
	if err != nil {
		t.Fatalf("ReadFile(imported firecracker): %v", err)
	}
	if !bytes.Equal(gotFirecracker, fcPayload) {
		t.Fatalf("imported firecracker payload mismatch")
	}
	if progressByFile[backupRootFSName].CompletedBytes != int64(len(rootfsPayload)) || progressByFile[backupRootFSName].TotalBytes != int64(len(rootfsPayload)) {
		t.Fatalf("rootfs progress = %#v, want %d/%d", progressByFile[backupRootFSName], len(rootfsPayload), len(rootfsPayload))
	}
	if progressByFile[backupSerialLogName].CompletedBytes != int64(len(serialPayload)) || progressByFile[backupSerialLogName].TotalBytes != int64(len(serialPayload)) {
		t.Fatalf("serial progress = %#v, want %d/%d", progressByFile[backupSerialLogName], len(serialPayload), len(serialPayload))
	}
	if progressByFile[backupFirecrackerName].CompletedBytes != int64(len(fcPayload)) || progressByFile[backupFirecrackerName].TotalBytes != int64(len(fcPayload)) {
		t.Fatalf("firecracker progress = %#v, want %d/%d", progressByFile[backupFirecrackerName], len(fcPayload), len(fcPayload))
	}

	stored, err := destStore.GetInstance(ctx, imported.Name)
	if err != nil {
		t.Fatalf("GetInstance(imported): %v", err)
	}
	if stored.ID != imported.ID || stored.RootFSPath != imported.RootFSPath || stored.NetworkCIDR != imported.NetworkCIDR {
		t.Fatalf("stored imported instance = %#v, want %#v", stored, imported)
	}

	sourceEvents, err := sourceStore.ListEvents(ctx, sourceInst.ID, 10)
	if err != nil {
		t.Fatalf("ListEvents(source): %v", err)
	}
	var sawExport bool
	for _, evt := range sourceEvents {
		if evt.Type == "export" && evt.Message == "instance exported as portable artifact" {
			sawExport = true
		}
	}
	if !sawExport {
		t.Fatalf("expected export event, got %#v", sourceEvents)
	}

	importedEvents, err := destStore.ListEvents(ctx, imported.ID, 10)
	if err != nil {
		t.Fatalf("ListEvents(imported): %v", err)
	}
	var sawImport bool
	for _, evt := range importedEvents {
		if evt.Type == "import" && evt.Message == "instance imported from portable artifact" {
			sawImport = true
		}
	}
	if !sawImport {
		t.Fatalf("expected import event, got %#v", importedEvents)
	}
}

func TestPeekPortableArtifactInfoReplaysStream(t *testing.T) {
	exportedAt := time.Date(2026, time.April, 5, 10, 30, 0, 0, time.UTC)
	manifest := portableManifest{
		Version:    portableManifestVersion,
		ExportedAt: exportedAt,
		Instance: portableInstance{
			Name:      "demo",
			CreatedAt: exportedAt.Add(-time.Hour),
		},
		Files: backupFiles{RootFS: backupRootFSName},
	}
	stream := portableArtifactTestStream(t, manifest, map[string][]byte{
		backupRootFSName: []byte("rootfs"),
	})

	info, replay, err := PeekPortableArtifactInfo(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("PeekPortableArtifactInfo(): %v", err)
	}
	if info.Name != manifest.Instance.Name {
		t.Fatalf("PeekPortableArtifactInfo() name = %q, want %q", info.Name, manifest.Instance.Name)
	}
	replayed, err := io.ReadAll(replay)
	if err != nil {
		t.Fatalf("ReadAll(replay): %v", err)
	}
	if !bytes.Equal(replayed, stream) {
		t.Fatalf("PeekPortableArtifactInfo() replay did not preserve stream bytes")
	}
}

func TestPeekPortableArtifactInfoDoesNotDrainCompressedStream(t *testing.T) {
	exportedAt := time.Date(2026, time.April, 5, 10, 45, 0, 0, time.UTC)
	manifest := portableManifest{
		Version:    portableManifestVersion,
		ExportedAt: exportedAt,
		Instance: portableInstance{
			Name:            "demo",
			CreatedAt:       exportedAt.Add(-time.Hour),
			RootFSSizeBytes: 1 << 20,
		},
		Files: backupFiles{RootFS: backupRootFSName},
	}
	rootfsPayload := make([]byte, 1<<20)
	if _, err := cryptorand.Read(rootfsPayload); err != nil {
		t.Fatalf("crypto/rand.Read(rootfs): %v", err)
	}
	stream := portableArtifactFlushedManifestTestStream(t, manifest, map[string][]byte{
		backupRootFSName: rootfsPayload,
	})
	manifestOnlyStream := portableArtifactFlushedManifestTestStream(t, manifest, nil)
	release := make(chan struct{})
	reader := &gatedReadCloser{
		data:    stream,
		limit:   len(manifestOnlyStream),
		release: release,
	}

	type peekResult struct {
		info   PortableArtifactInfo
		replay io.Reader
		err    error
	}
	resultCh := make(chan peekResult, 1)
	go func() {
		info, replay, err := PeekPortableArtifactInfo(reader)
		resultCh <- peekResult{info: info, replay: replay, err: err}
	}()

	var result peekResult
	select {
	case result = <-resultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("PeekPortableArtifactInfo() blocked waiting for data past the manifest")
	}
	if result.err != nil {
		t.Fatalf("PeekPortableArtifactInfo(): %v", result.err)
	}
	if result.info.Name != manifest.Instance.Name {
		t.Fatalf("PeekPortableArtifactInfo() name = %q, want %q", result.info.Name, manifest.Instance.Name)
	}

	close(release)
	replayed, err := io.ReadAll(result.replay)
	if err != nil {
		t.Fatalf("ReadAll(replay): %v", err)
	}
	if !bytes.Equal(replayed, stream) {
		t.Fatalf("PeekPortableArtifactInfo() replay did not preserve gated stream bytes")
	}
}

func TestImportInstanceRejectsAliasedPortableArtifactEntries(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{
		cfg:                 cfg,
		log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:               st,
		readFilesystemBytes: host.DefaultReadFilesystemBytes,
	}
	if err := os.MkdirAll(cfg.InstancesDir(), 0o770); err != nil {
		t.Fatalf("MkdirAll(instances dir): %v", err)
	}

	manifest := portableManifest{
		Version:    portableManifestVersion,
		ExportedAt: time.Date(2026, time.April, 5, 11, 0, 0, 0, time.UTC),
		Instance: portableInstance{
			Name:            "demo",
			CreatedAt:       time.Date(2026, time.April, 4, 11, 0, 0, 0, time.UTC),
			RootFSSizeBytes: int64(len("rootfs")),
		},
		Files: backupFiles{
			RootFS:    backupRootFSName,
			SerialLog: backupRootFSName,
		},
	}
	stream := portableArtifactTestStream(t, manifest, map[string][]byte{
		backupRootFSName: []byte("rootfs"),
	})

	_, _, err := p.ImportInstance(ctx, model.Actor{UserLogin: "alice@example.com", NodeName: "workstation"}, bytes.NewReader(stream))
	if err == nil || !strings.Contains(err.Error(), `serial log entry must be "serial.log"`) {
		t.Fatalf("ImportInstance() error = %v", err)
	}
}

func TestImportInstanceRejectsOversizedPortableLogs(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{
		cfg:                 cfg,
		log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:               st,
		readFilesystemBytes: host.DefaultReadFilesystemBytes,
	}
	if err := os.MkdirAll(cfg.InstancesDir(), 0o770); err != nil {
		t.Fatalf("MkdirAll(instances dir): %v", err)
	}

	manifest := portableManifest{
		Version:    portableManifestVersion,
		ExportedAt: time.Date(2026, time.April, 5, 12, 0, 0, 0, time.UTC),
		Instance: portableInstance{
			Name:            "demo",
			CreatedAt:       time.Date(2026, time.April, 4, 12, 0, 0, 0, time.UTC),
			RootFSSizeBytes: int64(len("rootfs")),
		},
		Files: backupFiles{
			RootFS:    backupRootFSName,
			SerialLog: backupSerialLogName,
		},
	}

	pr, pw := io.Pipe()
	go func() {
		payload, err := json.Marshal(manifest)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		tw, zw, err := newPortableArtifactTarWriter(pw)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if err := writeTarBytes(tw, backupManifestName, payload, 0o640, manifest.ExportedAt); err != nil {
			_ = closePortableArtifactTarWriter(tw, zw)
			_ = pw.CloseWithError(err)
			return
		}
		if err := writeTarBytes(tw, backupRootFSName, []byte("rootfs"), 0o660, manifest.ExportedAt); err != nil {
			_ = closePortableArtifactTarWriter(tw, zw)
			_ = pw.CloseWithError(err)
			return
		}
		if err := tw.WriteHeader(&tar.Header{Name: backupSerialLogName, Mode: 0o660, Size: portableLogMaxSize + 1, ModTime: manifest.ExportedAt}); err != nil {
			_ = closePortableArtifactTarWriter(tw, zw)
			_ = pw.CloseWithError(err)
			return
		}
		if err := zw.Flush(); err != nil {
			_ = closePortableArtifactTarWriter(tw, zw)
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()

	_, _, err := p.ImportInstance(ctx, model.Actor{UserLogin: "alice@example.com", NodeName: "workstation"}, pr)
	if err == nil || !strings.Contains(err.Error(), "portable artifact serial log is too large") {
		t.Fatalf("ImportInstance() error = %v", err)
	}
}

func TestWriteTarFileTailExportsNewestBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serial.log")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatalf("WriteFile(serial): %v", err)
	}

	var stream bytes.Buffer
	tw := tar.NewWriter(&stream)
	truncated, err := writeTarFileTail(tw, backupSerialLogName, path, 0o660, 4)
	if err != nil {
		t.Fatalf("writeTarFileTail(): %v", err)
	}
	if !truncated {
		t.Fatal("writeTarFileTail() did not report truncation")
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close(tar writer): %v", err)
	}

	tr := tar.NewReader(bytes.NewReader(stream.Bytes()))
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("Next(): %v", err)
	}
	if hdr.Name != backupSerialLogName {
		t.Fatalf("header name = %q, want %q", hdr.Name, backupSerialLogName)
	}
	if hdr.Size != 4 {
		t.Fatalf("header size = %d, want 4", hdr.Size)
	}
	payload, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("ReadAll(tar entry): %v", err)
	}
	if string(payload) != "6789" {
		t.Fatalf("payload = %q, want %q", payload, "6789")
	}
}

func TestImportInstanceReleasesAdmissionLockDuringStreaming(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{
		cfg:                 cfg,
		log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:               st,
		readFilesystemBytes: host.DefaultReadFilesystemBytes,
	}
	if err := os.MkdirAll(cfg.InstancesDir(), 0o770); err != nil {
		t.Fatalf("MkdirAll(instances dir): %v", err)
	}

	manifest := portableManifest{
		Version:    portableManifestVersion,
		ExportedAt: time.Date(2026, time.April, 5, 13, 0, 0, 0, time.UTC),
		Instance: portableInstance{
			Name:            "demo",
			CreatedAt:       time.Date(2026, time.April, 4, 13, 0, 0, 0, time.UTC),
			RootFSSizeBytes: 1,
		},
		Files: backupFiles{RootFS: backupRootFSName},
	}

	pr, pw := io.Pipe()
	headerWritten := make(chan struct{})
	allowPayload := make(chan struct{})
	importDone := make(chan error, 1)
	go func() {
		_, _, err := p.ImportInstance(ctx, model.Actor{UserLogin: "alice@example.com", NodeName: "workstation"}, pr)
		importDone <- err
	}()
	go func() {
		payload, err := json.Marshal(manifest)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		tw, zw, err := newPortableArtifactTarWriter(pw)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if err := writeTarBytes(tw, backupManifestName, payload, 0o640, manifest.ExportedAt); err != nil {
			_ = closePortableArtifactTarWriter(tw, zw)
			_ = pw.CloseWithError(err)
			return
		}
		if err := tw.WriteHeader(&tar.Header{Name: backupRootFSName, Mode: 0o660, Size: 1, ModTime: manifest.ExportedAt}); err != nil {
			_ = closePortableArtifactTarWriter(tw, zw)
			_ = pw.CloseWithError(err)
			return
		}
		if err := zw.Flush(); err != nil {
			_ = closePortableArtifactTarWriter(tw, zw)
			_ = pw.CloseWithError(err)
			return
		}
		close(headerWritten)
		<-allowPayload
		if _, err := tw.Write([]byte{'x'}); err != nil {
			_ = closePortableArtifactTarWriter(tw, zw)
			_ = pw.CloseWithError(err)
			return
		}
		if err := closePortableArtifactTarWriter(tw, zw); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()

	select {
	case <-headerWritten:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for import stream header")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		inst, found, err := st.FindInstance(ctx, manifest.Instance.Name)
		if err != nil {
			t.Fatalf("FindInstance(): %v", err)
		}
		if found {
			if inst.State != model.StateProvisioning {
				t.Fatalf("provisional import state = %q, want %q", inst.State, model.StateProvisioning)
			}
			break
		}
		select {
		case err := <-importDone:
			t.Fatalf("ImportInstance() finished early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for provisional imported instance")
		}
		time.Sleep(10 * time.Millisecond)
	}

	admissionAvailable := false
	admissionDeadline := time.Now().Add(500 * time.Millisecond)
	for !admissionAvailable {
		if p.admissionMu.TryLock() {
			admissionAvailable = true
			p.admissionMu.Unlock()
			break
		}
		select {
		case err := <-importDone:
			t.Fatalf("ImportInstance() finished before admission lock check: %v", err)
		default:
		}
		if time.Now().After(admissionDeadline) {
			t.Fatal("admissionMu remained locked during streaming import")
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(allowPayload)
	select {
	case err := <-importDone:
		if err != nil {
			t.Fatalf("ImportInstance(): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for import completion")
	}
}

func portableArtifactTestStream(t *testing.T, manifest portableManifest, files map[string][]byte) []byte {
	t.Helper()
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest): %v", err)
	}
	var stream bytes.Buffer
	tw, zw, err := newPortableArtifactTarWriter(&stream)
	if err != nil {
		t.Fatalf("newPortableArtifactTarWriter(): %v", err)
	}
	if err := writeTarBytes(tw, backupManifestName, payload, 0o640, manifest.ExportedAt); err != nil {
		t.Fatalf("writeTarBytes(manifest): %v", err)
	}
	for _, name := range []string{backupRootFSName, backupSerialLogName, backupFirecrackerName} {
		payload, ok := files[name]
		if !ok {
			continue
		}
		if err := writeTarBytes(tw, name, payload, 0o660, manifest.ExportedAt); err != nil {
			t.Fatalf("writeTarBytes(%s): %v", name, err)
		}
	}
	if err := closePortableArtifactTarWriter(tw, zw); err != nil {
		t.Fatalf("closePortableArtifactTarWriter(): %v", err)
	}
	return stream.Bytes()
}

func portableArtifactFlushedManifestTestStream(t *testing.T, manifest portableManifest, files map[string][]byte) []byte {
	t.Helper()
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest): %v", err)
	}
	var stream bytes.Buffer
	tw, zw, err := newPortableArtifactTarWriter(&stream)
	if err != nil {
		t.Fatalf("newPortableArtifactTarWriter(): %v", err)
	}
	if err := writeTarBytes(tw, backupManifestName, payload, 0o640, manifest.ExportedAt); err != nil {
		t.Fatalf("writeTarBytes(manifest): %v", err)
	}
	if err := zw.Flush(); err != nil {
		t.Fatalf("zw.Flush(): %v", err)
	}
	for _, name := range []string{backupRootFSName, backupSerialLogName, backupFirecrackerName} {
		payload, ok := files[name]
		if !ok {
			continue
		}
		if err := writeTarBytes(tw, name, payload, 0o660, manifest.ExportedAt); err != nil {
			t.Fatalf("writeTarBytes(%s): %v", name, err)
		}
	}
	if err := closePortableArtifactTarWriter(tw, zw); err != nil {
		t.Fatalf("closePortableArtifactTarWriter(): %v", err)
	}
	return stream.Bytes()
}

type gatedReadCloser struct {
	data    []byte
	limit   int
	pos     int
	release <-chan struct{}
}

func (r *gatedReadCloser) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if r.pos >= r.limit {
		<-r.release
	}
	maxRead := len(p)
	if r.pos < r.limit {
		if remaining := r.limit - r.pos; remaining < maxRead {
			maxRead = remaining
		}
	}
	if remaining := len(r.data) - r.pos; remaining < maxRead {
		maxRead = remaining
	}
	n := copy(p, r.data[r.pos:r.pos+maxRead])
	r.pos += n
	return n, nil
}

func TestBackupRequiresStoppedInstance(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{
		cfg:                 cfg,
		log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:               st,
		readFilesystemBytes: host.DefaultReadFilesystemBytes,
	}

	inst := provisionTestInstance(cfg, "demo", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(instance dir): %v", err)
	}
	if err := os.WriteFile(inst.RootFSPath, []byte("rootfs"), 0o644); err != nil {
		t.Fatalf("WriteFile(rootfs): %v", err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance(): %v", err)
	}

	_, err := p.CreateBackup(ctx, inst.Name)
	if err == nil || !strings.Contains(err.Error(), "must be stopped before backup or restore") {
		t.Fatalf("CreateBackup() error = %v", err)
	}
}

func TestRestoreBackupRejectsRecreatedInstanceWithSameName(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st}

	oldReflinkCloneFile := reflinkCloneFile
	t.Cleanup(func() {
		reflinkCloneFile = oldReflinkCloneFile
	})
	reflinkCloneFile = func(_ context.Context, src, dest string) error {
		payload, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, payload, 0o644)
	}

	original := provisionTestInstance(cfg, "demo", model.StateStopped, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	if err := os.MkdirAll(filepath.Dir(original.RootFSPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(instance dir): %v", err)
	}
	if err := os.WriteFile(original.RootFSPath, []byte("old-rootfs"), 0o644); err != nil {
		t.Fatalf("WriteFile(old rootfs): %v", err)
	}
	if err := st.CreateInstance(ctx, original); err != nil {
		t.Fatalf("CreateInstance(original): %v", err)
	}

	backup, err := p.CreateBackup(ctx, original.Name)
	if err != nil {
		t.Fatalf("CreateBackup(): %v", err)
	}
	if err := st.DeleteInstance(ctx, original.Name); err != nil {
		t.Fatalf("DeleteInstance(): %v", err)
	}

	recreated := provisionTestInstance(cfg, "demo", model.StateStopped, original.CreatedAt.Add(time.Hour))
	recreated.ID = "demo-recreated-id"
	recreated.NetworkCIDR = "10.0.0.4/30"
	recreated.HostAddr = "10.0.0.5/30"
	recreated.GuestAddr = "10.0.0.6/30"
	recreated.GatewayAddr = "10.0.0.5"
	if err := os.WriteFile(recreated.RootFSPath, []byte("new-rootfs"), 0o644); err != nil {
		t.Fatalf("WriteFile(new rootfs): %v", err)
	}
	if err := st.CreateInstance(ctx, recreated); err != nil {
		t.Fatalf("CreateInstance(recreated): %v", err)
	}

	_, _, err = p.RestoreBackup(ctx, recreated.Name, backup.ID)
	if err == nil || !strings.Contains(err.Error(), "restoring onto a recreated VM is not supported") {
		t.Fatalf("RestoreBackup() error = %v", err)
	}

	stored, err := st.GetInstance(ctx, recreated.Name)
	if err != nil {
		t.Fatalf("GetInstance(): %v", err)
	}
	if stored.ID != recreated.ID || stored.NetworkCIDR != recreated.NetworkCIDR || stored.GuestAddr != recreated.GuestAddr {
		t.Fatalf("stored recreated instance changed after rejected restore: %#v", stored)
	}
	payload, err := os.ReadFile(recreated.RootFSPath)
	if err != nil {
		t.Fatalf("ReadFile(recreated rootfs): %v", err)
	}
	if string(payload) != "new-rootfs" {
		t.Fatalf("recreated rootfs contents = %q, want %q", string(payload), "new-rootfs")
	}
}

func TestRestoreBackupStagesFilesBeforeReplacingActiveInstance(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st}

	oldReflinkCloneFile := reflinkCloneFile
	t.Cleanup(func() {
		reflinkCloneFile = oldReflinkCloneFile
	})
	reflinkCloneFile = func(_ context.Context, src, dest string) error {
		payload, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, payload, 0o644)
	}

	inst := provisionTestInstance(cfg, "demo", model.StateStopped, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(instance dir): %v", err)
	}
	if err := os.WriteFile(inst.RootFSPath, []byte("backup-rootfs"), 0o644); err != nil {
		t.Fatalf("WriteFile(rootfs): %v", err)
	}
	if err := os.WriteFile(inst.SerialLogPath, []byte("backup-serial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(serial): %v", err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance(): %v", err)
	}

	backup, err := p.CreateBackup(ctx, inst.Name)
	if err != nil {
		t.Fatalf("CreateBackup(): %v", err)
	}
	if err := os.WriteFile(inst.RootFSPath, []byte("live-rootfs"), 0o644); err != nil {
		t.Fatalf("WriteFile(live rootfs): %v", err)
	}
	if err := os.WriteFile(inst.SerialLogPath, []byte("live-serial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(live serial): %v", err)
	}

	reflinkCloneFile = func(_ context.Context, src, dest string) error {
		if filepath.Base(src) == backupSerialLogName {
			return errors.New("simulated restore failure")
		}
		payload, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, payload, 0o644)
	}

	_, _, err = p.RestoreBackup(ctx, inst.Name, backup.ID)
	if err == nil || !strings.Contains(err.Error(), "simulated restore failure") {
		t.Fatalf("RestoreBackup() error = %v", err)
	}

	rootfsPayload, err := os.ReadFile(inst.RootFSPath)
	if err != nil {
		t.Fatalf("ReadFile(rootfs): %v", err)
	}
	if string(rootfsPayload) != "live-rootfs" {
		t.Fatalf("rootfs contents after failed staged restore = %q, want %q", string(rootfsPayload), "live-rootfs")
	}
	serialPayload, err := os.ReadFile(inst.SerialLogPath)
	if err != nil {
		t.Fatalf("ReadFile(serial): %v", err)
	}
	if string(serialPayload) != "live-serial\n" {
		t.Fatalf("serial contents after failed staged restore = %q, want %q", string(serialPayload), "live-serial\n")
	}
	stored, err := st.GetInstance(ctx, inst.Name)
	if err != nil {
		t.Fatalf("GetInstance(): %v", err)
	}
	if stored.RootFSSizeBytes != inst.RootFSSizeBytes || stored.VCPUCount != inst.VCPUCount || stored.MemoryMiB != inst.MemoryMiB {
		t.Fatalf("stored instance changed after failed staged restore: %#v", stored)
	}
}

func TestCommitStagedRestoreFilesRollsBackEarlierReplacements(t *testing.T) {
	dir := t.TempDir()
	serialDest := filepath.Join(dir, "serial.log")
	blockedDest := filepath.Join(dir, "rootfs.img")

	if err := os.WriteFile(serialDest, []byte("live-serial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(serial dest): %v", err)
	}
	if err := os.Mkdir(blockedDest, 0o755); err != nil {
		t.Fatalf("Mkdir(blocked dest): %v", err)
	}

	writeStage := func(pattern, payload string) string {
		t.Helper()
		tmp, err := os.CreateTemp(dir, pattern)
		if err != nil {
			t.Fatalf("CreateTemp(%s): %v", pattern, err)
		}
		path := tmp.Name()
		if _, err := tmp.WriteString(payload); err != nil {
			_ = tmp.Close()
			t.Fatalf("WriteString(%s): %v", path, err)
		}
		if err := tmp.Close(); err != nil {
			t.Fatalf("Close(%s): %v", path, err)
		}
		return path
	}

	_, err := commitStagedRestoreFiles([]stagedRestoreFile{
		{dest: serialDest, tmp: writeStage(".srv-stage-serial-*", "backup-serial\n")},
		{dest: blockedDest, tmp: writeStage(".srv-stage-rootfs-*", "backup-rootfs")},
	})
	if err == nil {
		t.Fatal("commitStagedRestoreFiles() unexpectedly succeeded")
	}

	serialPayload, err := os.ReadFile(serialDest)
	if err != nil {
		t.Fatalf("ReadFile(serial dest): %v", err)
	}
	if string(serialPayload) != "live-serial\n" {
		t.Fatalf("serial contents after rollback = %q, want %q", string(serialPayload), "live-serial\n")
	}
	info, err := os.Stat(blockedDest)
	if err != nil {
		t.Fatalf("Stat(blocked dest): %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("blocked dest should remain a directory after rollback")
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".srv-restore-old-*"))
	if err != nil {
		t.Fatalf("Glob(restore old files): %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("rollback copies should be cleaned up, found %v", leftovers)
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
	if err := os.MkdirAll(cfg.InstancesDir(), 0o770); err != nil {
		t.Fatalf("MkdirAll(instances dir): %v", err)
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
		if !strings.HasPrefix(dest, cfg.InstancesDir()+string(os.PathSeparator)) {
			t.Fatalf("reflinkCloneFile dest = %q, want path under %q", dest, cfg.InstancesDir())
		}
	})

	t.Run("accepts installer-style data dir permissions", func(t *testing.T) {
		dataDirInfo, err := os.Stat(cfg.DataDirAbs())
		if err != nil {
			t.Fatalf("Stat(data dir): %v", err)
		}
		dataDirMode := dataDirInfo.Mode().Perm()
		t.Cleanup(func() {
			if err := os.Chmod(cfg.DataDirAbs(), dataDirMode); err != nil {
				t.Fatalf("restore data dir mode: %v", err)
			}
		})

		if err := os.MkdirAll(cfg.InstancesDir(), 0o770); err != nil {
			t.Fatalf("MkdirAll(instances dir): %v", err)
		}
		if err := os.Chmod(cfg.InstancesDir(), 0o770); err != nil {
			t.Fatalf("Chmod(instances dir): %v", err)
		}
		if err := os.Chmod(cfg.DataDirAbs(), 0o555); err != nil {
			t.Fatalf("Chmod(data dir): %v", err)
		}

		var dest string
		reflinkCloneFile = func(_ context.Context, _, gotDest string) error {
			dest = gotDest
			return nil
		}

		p := &Provisioner{cfg: cfg, tsClient: &tailscale.Client{}}
		if err := p.ensureCreatePrereqs(context.Background(), false); err != nil {
			t.Fatalf("ensureCreatePrereqs() with installer-style permissions error = %v", err)
		}
		if !strings.HasPrefix(dest, cfg.InstancesDir()+string(os.PathSeparator)) {
			t.Fatalf("reflinkCloneFile dest = %q, want path under %q", dest, cfg.InstancesDir())
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

func TestCreateRejectsWhenHostDiskSpaceIsLow(t *testing.T) {
	oldLoopDevicesForPath := loopDevicesForPath
	oldReflinkCloneFile := reflinkCloneFile
	t.Cleanup(func() {
		loopDevicesForPath = oldLoopDevicesForPath
		reflinkCloneFile = oldReflinkCloneFile
	})

	baseDir := t.TempDir()
	baseKernel := filepath.Join(baseDir, "images", "vmlinux")
	baseRootFS := filepath.Join(baseDir, "images", "rootfs.img")
	cfg := loadProvisionTestConfig(t, map[string]string{
		"SRV_BASE_KERNEL":     baseKernel,
		"SRV_BASE_ROOTFS":     baseRootFS,
		"TS_TAILNET":          "tailnet.example.com",
		"TS_CLIENT_SECRET":    "secret",
		"SRV_GUEST_AUTH_TAGS": "tag:microvm",
	})
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{
		cfg:      cfg,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:    st,
		tsClient: &tailscale.Client{},
		readHostMemoryBytes: func() (int64, error) {
			return 8 * 1024 * format.MiB, nil
		},
		readFilesystemBytes: func(string) (int64, error) {
			return 512 * format.MiB, nil
		},
	}
	if err := os.MkdirAll(cfg.InstancesDir(), 0o770); err != nil {
		t.Fatalf("MkdirAll(instances dir): %v", err)
	}

	for _, path := range []string{baseKernel, baseRootFS} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}
	if err := os.Truncate(baseRootFS, 4*format.MiB); err != nil {
		t.Fatalf("Truncate(baseRootFS): %v", err)
	}

	loopDevicesForPath = func(string) (string, error) { return "", nil }
	reflinkCloneFile = func(context.Context, string, string) error { return nil }

	if _, err := p.Create(context.Background(), "demo", model.Actor{UserLogin: "alice@example.com"}, CreateOptions{}); err == nil || !strings.Contains(err.Error(), "insufficient host disk") {
		t.Fatalf("Create() disk capacity error = %v", err)
	}
	if _, found, err := st.FindInstance(context.Background(), "demo"); err != nil {
		t.Fatalf("FindInstance(): %v", err)
	} else if found {
		t.Fatal("Create() should fail before persisting an instance row")
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

func TestEnsureStartPrereqsAllowsStoppedInstanceWithoutStoredTailnetIdentity(t *testing.T) {
	firecrackerBin := filepath.Join(t.TempDir(), "bin", "firecracker")
	cfg := loadProvisionTestConfig(t, map[string]string{
		"SRV_FIRECRACKER_BIN": firecrackerBin,
	})
	p := &Provisioner{cfg: cfg, tsClient: &tailscale.Client{}}
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

	if err := p.ensureStartPrereqs(inst); err != nil {
		t.Fatalf("ensureStartPrereqs() without prior tailnet identity: %v", err)
	}

	inst.TailscaleName = "demo.tailnet"
	if err := p.ensureStartPrereqs(inst); err != nil {
		t.Fatalf("ensureStartPrereqs() with prior tailnet identity: %v", err)
	}
}

func TestEnsureStartPrereqsRejectsDeletingInstance(t *testing.T) {
	cfg := loadProvisionTestConfig(t, nil)
	p := &Provisioner{cfg: cfg, tsClient: &tailscale.Client{}}
	inst := provisionTestInstance(cfg, "demo", model.StateDeleting, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))

	if err := p.ensureStartPrereqs(inst); err == nil || !strings.Contains(err.Error(), `instance "demo" is being deleted`) {
		t.Fatalf("ensureStartPrereqs() error = %v", err)
	}
}

func TestStartRejectsWhenHostMemoryIsLow(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{
		cfg:           cfg,
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:         st,
		tsClient:      &tailscale.Client{},
		networkHelper: panicNetworkHelper{t: t},
		vmRunner:      panicVMRunner{t: t},
		readHostMemoryBytes: func() (int64, error) {
			return 1024 * format.MiB, nil
		},
	}
	inst := provisionTestInstance(cfg, "demo", model.StateStopped, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(rootfs dir): %v", err)
	}
	for _, path := range []string{inst.RootFSPath, inst.KernelPath, inst.InitrdPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance(): %v", err)
	}

	if _, err := p.Start(ctx, inst.Name); err == nil || !strings.Contains(err.Error(), "insufficient host memory") {
		t.Fatalf("Start() memory capacity error = %v", err)
	}
}

func TestStartReconcilesLateTailnetJoinForRunningInstance(t *testing.T) {
	oldListTailnetDevices := listTailnetDevices
	t.Cleanup(func() { listTailnetDevices = oldListTailnetDevices })
	listTailnetDevices = func(context.Context, *tailscale.Client) ([]tailscale.Device, error) {
		return []tailscale.Device{{
			Hostname:  "demo",
			Name:      "demo.tailnet.ts.net",
			Addresses: []string{"100.64.0.10"},
		}}, nil
	}

	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st, tsClient: &tailscale.Client{}}
	inst := provisionTestInstance(cfg, "demo", model.StateAwaitingTailnet, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	inst.FirecrackerPID = os.Getpid()
	inst.LastError = errGuestNotReady.Error()
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	got, err := p.Start(ctx, "demo")
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if got.State != model.StateReady {
		t.Fatalf("Start() state = %q, want %q", got.State, model.StateReady)
	}
	if got.TailscaleName != "demo" {
		t.Fatalf("Start() tailscale name = %q, want demo", got.TailscaleName)
	}
	if got.TailscaleIP != "100.64.0.10" {
		t.Fatalf("Start() tailscale ip = %q, want 100.64.0.10", got.TailscaleIP)
	}
	if got.LastError != "" {
		t.Fatalf("Start() last error = %q, want empty", got.LastError)
	}

	stored, err := st.GetInstance(ctx, "demo")
	if err != nil {
		t.Fatalf("GetInstance(): %v", err)
	}
	if stored.State != model.StateReady || stored.TailscaleName != "demo" || stored.TailscaleIP != "100.64.0.10" {
		t.Fatalf("stored instance = %#v", stored)
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

func TestResizeRejectsGrowingRootFSWhenHostDiskSpaceIsLow(t *testing.T) {
	const testMiB = int64(1024 * 1024)

	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{
		cfg:   cfg,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
		readFilesystemBytes: func(string) (int64, error) {
			return 256 * format.MiB, nil
		},
	}

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
		t.Fatalf("CreateInstance(): %v", err)
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

	if _, err := p.Resize(ctx, inst.Name, CreateOptions{RootFSSizeBytes: 12 * testMiB}); err == nil || !strings.Contains(err.Error(), "insufficient host disk") {
		t.Fatalf("Resize() disk capacity error = %v", err)
	}
}

func TestCapacitySummaryIncludesDiskStorageDetails(t *testing.T) {
	oldReadProcMountInfo := storage.ReadProcMountInfo
	oldReadDirNames := storage.ReadDirNames
	oldReadTrimmedFile := storage.ReadTrimmedFile
	oldPathExists := storage.PathExists
	t.Cleanup(func() {
		storage.ReadProcMountInfo = oldReadProcMountInfo
		storage.ReadDirNames = oldReadDirNames
		storage.ReadTrimmedFile = oldReadTrimmedFile
		storage.PathExists = oldPathExists
	})

	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{
		cfg:   cfg,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
		readHostMemoryBytes: func() (int64, error) {
			return 8 * 1024 * format.MiB, nil
		},
		readFilesystemBytes: func(string) (int64, error) {
			return 128 * 1024 * format.MiB, nil
		},
	}
	if err := os.MkdirAll(cfg.InstancesDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll(instances dir): %v", err)
	}

	storage.ReadProcMountInfo = func() ([]byte, error) {
		line := fmt.Sprintf("36 25 0:32 / %s rw,relatime - btrfs /dev/md0 rw\n", cfg.InstancesDir())
		return []byte(line), nil
	}
	storage.ReadDirNames = func(path string) ([]string, error) {
		switch path {
		case "/sys/fs/btrfs":
			return []string{"fsid-1"}, nil
		case "/sys/fs/btrfs/fsid-1/devices":
			return []string{"md0"}, nil
		case "/sys/fs/btrfs/fsid-1/devinfo":
			return []string{"1"}, nil
		default:
			return nil, os.ErrNotExist
		}
	}
	storage.ReadTrimmedFile = func(path string) (string, error) {
		switch path {
		case "/sys/class/block/md0/dev":
			return "9:0", nil
		case "/sys/fs/btrfs/fsid-1/devinfo/1/missing":
			return "0", nil
		case "/sys/fs/btrfs/fsid-1/devinfo/1/error_stats":
			return "write_errs 0\nread_errs 0\nflush_errs 0\ncorruption_errs 0\ngeneration_errs 0", nil
		case "/sys/class/block/md0/md/array_state":
			return "clean", nil
		case "/sys/class/block/md0/md/degraded":
			return "0", nil
		case "/sys/class/block/md0/md/sync_action":
			return "idle", nil
		default:
			return "", os.ErrNotExist
		}
	}
	storage.PathExists = func(string) bool { return false }

	summary, err := p.CapacitySummary(ctx)
	if err != nil {
		t.Fatalf("CapacitySummary(): %v", err)
	}

	resources := make(map[string]host.CapacityResource, len(summary.Capacity))
	for _, resource := range summary.Capacity {
		resources[resource.Resource] = resource
	}
	disk := resources["disk"]
	want := []host.CapacityDetail{
		{Label: "BTRFS", Value: "DEVICE STATS CLEAN"},
		{Label: "MDADM", Value: "HEALTH O.K."},
	}
	if !reflect.DeepEqual(disk.Details, want) {
		t.Fatalf("disk details = %#v, want %#v", disk.Details, want)
	}
}

func TestReservedInstanceRootFSBytesUsesAllocatedBytesForDeletedAndDeletingInstances(t *testing.T) {
	rootfsPath := filepath.Join(t.TempDir(), "rootfs.img")
	if err := os.WriteFile(rootfsPath, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile(rootfs): %v", err)
	}
	if err := os.Truncate(rootfsPath, 64*format.MiB); err != nil {
		t.Fatalf("Truncate(rootfs): %v", err)
	}

	info, err := os.Stat(rootfsPath)
	if err != nil {
		t.Fatalf("Stat(rootfs): %v", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Stat(rootfs) sys = %T, want *syscall.Stat_t", info.Sys())
	}

	for _, state := range []string{model.StateDeleted, model.StateDeleting} {
		inst := model.Instance{
			State:           state,
			RootFSPath:      rootfsPath,
			RootFSSizeBytes: 64 * format.MiB,
		}
		if got, want := reservedInstanceRootFSBytes(inst), st.Blocks*512; got != want {
			t.Fatalf("reservedInstanceRootFSBytes(%q) = %d, want %d", state, got, want)
		}
	}
}

func TestEnsureHostDiskCapacityCountsDeletedInstancesStillPresentInStore(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	rootfsPath := filepath.Join(cfg.InstancesDir(), "deleted", "rootfs.img")
	if err := os.MkdirAll(filepath.Dir(rootfsPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(rootfs dir): %v", err)
	}
	if err := os.WriteFile(rootfsPath, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile(rootfs): %v", err)
	}
	if err := os.Truncate(rootfsPath, 64*format.MiB); err != nil {
		t.Fatalf("Truncate(rootfs): %v", err)
	}

	info, err := os.Stat(rootfsPath)
	if err != nil {
		t.Fatalf("Stat(rootfs): %v", err)
	}
	stInfo, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Stat(rootfs) sys = %T, want *syscall.Stat_t", info.Sys())
	}
	allocatedBytes := stInfo.Blocks * 512

	deleted := provisionTestInstance(cfg, "deleted", model.StateDeleted, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	deleted.RootFSPath = rootfsPath
	deleted.RootFSSizeBytes = 64 * format.MiB
	deletedAt := deleted.CreatedAt.Add(time.Minute)
	deleted.DeletedAt = &deletedAt
	if err := st.CreateInstance(ctx, deleted); err != nil {
		t.Fatalf("CreateInstance(deleted): %v", err)
	}

	requestedBytes := int64(8 * format.MiB)
	p := &Provisioner{
		cfg:   cfg,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
		readFilesystemBytes: func(string) (int64, error) {
			return hostDiskReserveBytes + allocatedBytes + requestedBytes - 1, nil
		},
	}

	if err := p.ensureHostDiskCapacity(ctx, "", requestedBytes); err == nil || !strings.Contains(err.Error(), "insufficient host disk") {
		t.Fatalf("ensureHostDiskCapacity() error = %v", err)
	}
}

func TestDeviceUpdatedSince(t *testing.T) {
	tsTime := func(value string) *tailscale.Time {
		t.Helper()
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			t.Fatalf("time.Parse(%q): %v", value, err)
		}
		return &tailscale.Time{Time: parsed}
	}

	previous := tailnetDeviceSnapshot{DeviceID: "device-1", LastSeen: "2026-03-29T12:00:00Z"}
	if deviceUpdatedSince(tailscale.Device{NodeID: "device-1", LastSeen: tsTime(previous.LastSeen)}, previous, true) {
		t.Fatalf("deviceUpdatedSince() reported unchanged device as updated")
	}
	if !deviceUpdatedSince(tailscale.Device{NodeID: "device-1", LastSeen: tsTime("2026-03-29T12:01:00Z")}, previous, true) {
		t.Fatalf("deviceUpdatedSince() should treat newer last-seen as updated")
	}
	if !deviceUpdatedSince(tailscale.Device{NodeID: "device-2", LastSeen: tsTime(previous.LastSeen)}, previous, true) {
		t.Fatalf("deviceUpdatedSince() should treat a new device ID as updated")
	}
	if !deviceUpdatedSince(tailscale.Device{NodeID: "device-1"}, tailnetDeviceSnapshot{}, false) {
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
		{state: model.StateDeleting, want: false},
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

type panicNetworkHelper struct {
	t *testing.T
}

func (h panicNetworkHelper) SetupInstanceNetwork(context.Context, nethelper.SetupRequest) error {
	h.t.Fatalf("SetupInstanceNetwork() should not be called")
	return nil
}

func (h panicNetworkHelper) CleanupInstanceNetwork(context.Context, nethelper.CleanupRequest) error {
	h.t.Fatalf("CleanupInstanceNetwork() should not be called")
	return nil
}

type panicVMRunner struct {
	t *testing.T
}

func (r panicVMRunner) StartInstanceVM(context.Context, vmrunner.StartRequest) (vmrunner.StartResponse, error) {
	r.t.Fatalf("StartInstanceVM() should not be called")
	return vmrunner.StartResponse{}, nil
}

func (r panicVMRunner) StopInstanceVM(context.Context, vmrunner.StopRequest) error {
	r.t.Fatalf("StopInstanceVM() should not be called")
	return nil
}

func (r panicVMRunner) ReadInstanceMetrics(context.Context, vmrunner.MetricsRequest) (vmrunner.MetricsResponse, error) {
	r.t.Fatalf("ReadInstanceMetrics() should not be called")
	return vmrunner.MetricsResponse{}, nil
}

type noopVMRunner struct{}

func (noopVMRunner) StartInstanceVM(context.Context, vmrunner.StartRequest) (vmrunner.StartResponse, error) {
	return vmrunner.StartResponse{}, nil
}

func (noopVMRunner) StopInstanceVM(context.Context, vmrunner.StopRequest) error {
	return nil
}

func (noopVMRunner) ReadInstanceMetrics(context.Context, vmrunner.MetricsRequest) (vmrunner.MetricsResponse, error) {
	return vmrunner.MetricsResponse{}, nil
}
