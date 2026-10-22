package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"srv/internal/format"
	"srv/internal/host"
	"srv/internal/model"
)

func TestPlacementUsesReservationAndPhysicalDiskBudgets(t *testing.T) {
	cfg := loadProvisionTestConfig(t, nil)
	cfg.BaseKernelPath = filepath.Join(t.TempDir(), "kernel")
	cfg.BaseInitrdPath = ""
	cfg.BaseRootFSPath = filepath.Join(t.TempDir(), "rootfs")
	for _, path := range []string{cfg.BaseKernelPath, cfg.BaseRootFSPath} {
		if err := os.WriteFile(path, []byte("image"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	st := newProvisionTestStore(t, cfg)
	memory, diskAvailable := int64(4096*format.MiB), int64(10*format.GiB)
	p := &Provisioner{
		cfg: cfg, store: st,
		readHostMemoryBytes: func() (int64, error) { return memory, nil },
		readFilesystemUsage: func(string) (host.FilesystemUsage, error) {
			return host.FilesystemUsage{Total: 20 * format.GiB, Available: diskAvailable}, nil
		},
	}
	ctx := context.Background()
	opts := CreateOptions{VCPUCount: 2, MemoryMiB: 1024, RootFSSizeBytes: 4 * format.GiB}
	got := p.Placement(ctx, "demo", opts)
	if !got.Eligible || got.DiskLeftBytes != 5*format.GiB || got.MemoryLeftMiB <= 0 || got.MemoryLeftMiB >= 2560 {
		t.Fatalf("placement must include VM overhead and disk reserve: %+v", got)
	}
	// The logical reservation budget fits, but actual filesystem space does not.
	diskAvailable = 5*format.GiB - 1
	got = p.Placement(ctx, "demo", opts)
	if got.Eligible || !strings.Contains(got.Reason, "available disk") {
		t.Fatalf("physical disk shortage not rejected: %+v", got)
	}
	diskAvailable = 5 * format.GiB
	if got := p.Placement(ctx, "demo", opts); !got.Eligible || got.DiskLeftBytes != 0 {
		t.Fatalf("exact physical disk boundary should fit: %+v", got)
	}
	memory = 1536 * format.MiB // RAM alone fits, but VM overhead does not.
	if got := p.Placement(ctx, "demo", opts); got.Eligible || !strings.Contains(got.Reason, "memory") {
		t.Fatalf("VM overhead must count against memory budget: %+v", got)
	}
	instances, err := st.ListInstances(ctx, true)
	if err != nil || len(instances) != 0 {
		t.Fatalf("preflight must not reserve instances: %+v %v", instances, err)
	}
}

func TestImportPlacementAllowsDeletedSourceWithoutRootFSTemplate(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	cfg.BaseKernelPath = filepath.Join(t.TempDir(), "kernel")
	cfg.BaseRootFSPath = filepath.Join(t.TempDir(), "missing-rootfs")
	cfg.BaseInitrdPath = ""
	if err := os.WriteFile(cfg.BaseKernelPath, []byte("kernel"), 0600); err != nil {
		t.Fatal(err)
	}
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, store: st,
		readHostMemoryBytes: func() (int64, error) { return 8 * format.GiB, nil },
		readFilesystemUsage: func(string) (host.FilesystemUsage, error) {
			return host.FilesystemUsage{Total: 20 * format.GiB, Available: 10 * format.GiB}, nil
		},
	}
	inst := provisionTestInstance(cfg, "returning", model.StateDeleted, time.Now().UTC())
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	m := model.Migration{InstanceID: inst.ID, Token: "old", Target: "other", Phase: "completed"}
	if err := st.PutMigration(ctx, m); err != nil {
		t.Fatal(err)
	}
	opts := CreateOptions{VCPUCount: 2, MemoryMiB: 1024, RootFSSizeBytes: 2 * format.GiB}
	if got := p.ImportPlacement(ctx, inst.Name, opts); !got.Eligible || got.DiskLeftBytes != 7*format.GiB {
		t.Fatalf("return migration must use incoming disk and allow deleted completed source: %+v", got)
	}
	if got := p.Placement(ctx, inst.Name, opts); got.Eligible {
		t.Fatal("new must still require rootfs template")
	}
	if err := os.WriteFile(cfg.BaseRootFSPath, []byte("template"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := p.Placement(ctx, inst.Name, opts); !got.Eligible {
		t.Fatalf("new must allow deleted name: %+v", got)
	}
	m.Phase = "prepared"
	if err := st.PutMigration(ctx, m); err != nil {
		t.Fatal(err)
	}
	if got := p.ImportPlacement(ctx, inst.Name, opts); got.Eligible || !strings.Contains(got.Reason, "fenced") {
		t.Fatalf("unfinished migration must remain fenced: %+v", got)
	}
}
