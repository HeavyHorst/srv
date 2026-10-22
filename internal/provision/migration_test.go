package provision

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"srv/internal/host"
	"srv/internal/model"
	"tailscale.com/client/tailscale/v2"
)

func TestCompletedMigrationDeletePreservesSharedTailnetIdentity(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	// An unconfigured API client fails if deletion touches the tailnet API.
	p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), vmRunner: noopVMRunner{}, tsClient: &tailscale.Client{}}
	inst := provisionTestInstance(cfg, "moved", model.StateStopped, time.Now().UTC())
	inst.TailnetNodeID = "shared-node"
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PrepareMigration(ctx, inst.Name, "destination", "transfer"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Delete(ctx, inst.Name); err == nil {
		t.Fatal("active source deleted")
	}
	if _, err := p.FinishMigration(ctx, inst.Name, "transfer", false); err != nil {
		t.Fatal(err)
	}
	deleted, err := p.Delete(ctx, inst.Name)
	if err != nil || deleted.State != model.StateDeleted {
		t.Fatalf("delete moved source: %+v, %v", deleted, err)
	}
}

func TestRecoveredMigrationRequiresFreshToken(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, store: st}
	inst := provisionTestInstance(cfg, "retry", model.StateStopped, time.Now().UTC())
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PrepareMigration(ctx, inst.Name, "destination", "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.FinishMigration(ctx, inst.Name, "old", true); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PrepareMigration(ctx, inst.Name, "destination", "old"); err == nil {
		t.Fatal("old transfer resumed after recovery")
	}
	if _, err := p.PrepareMigration(ctx, inst.Name, "destination", "fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.FinishMigration(ctx, inst.Name, "old", true); err == nil {
		t.Fatal("stale token removed fresh fence")
	}
}

func TestMigrationDestinationPolicyPersists(t *testing.T) {
	for _, opts := range []ImportOptions{{MemoryPoolName: "destination-pool"}, {FixedMemory: true}} {
		t.Run(opts.MemoryPoolName, func(t *testing.T) {
			ctx := context.Background()
			cfg := loadProvisionTestConfig(t, nil)
			st := newProvisionTestStore(t, cfg)
			p := &Provisioner{cfg: cfg, store: st}
			inst := provisionTestInstance(cfg, "pooled", model.StateStopped, time.Now().UTC())
			inst.MemoryMode, inst.MemoryPoolID = "pool", "source-local-id"
			if err := st.CreateInstance(ctx, inst); err != nil {
				t.Fatal(err)
			}
			if _, err := p.PrepareMigration(ctx, inst.Name, "target", "token"); err == nil {
				t.Fatal("implicit pool conversion accepted")
			}
			if _, err := p.PrepareMigration(ctx, inst.Name, "target", "token", opts); err != nil {
				t.Fatal(err)
			}
			m, err := p.MigrationStatus(ctx, inst.Name)
			if err != nil || m.MemoryPoolName != opts.MemoryPoolName || m.FixedMemory != opts.FixedMemory {
				t.Fatalf("policy lost: %+v %v", m, err)
			}
			if _, err := p.PrepareMigration(ctx, inst.Name, "target", "token", opts); err != nil {
				t.Fatal(err)
			}
			if _, err := p.PrepareMigration(ctx, inst.Name, "target", "token", ImportOptions{MemoryPoolName: "changed"}); err == nil {
				t.Fatal("retry changed policy")
			}
		})
	}
}

func TestPooledPortablePolicy(t *testing.T) {
	inst := model.Instance{MemoryMode: "pool", MemoryPoolID: "never-portable", Image: "arch", CreatedByUser: "owner"}
	m := portableManifestFromInstance(time.Now(), inst)
	if m.Version != 3 || m.Instance.MemoryMode != "pool" {
		t.Fatalf("unsafe pooled artifact: %+v", m)
	}
	if portableArtifactInfoFromManifest(m).CreatedByUser != "owner" {
		t.Fatal("missing owner")
	}
	if err := (ImportOptions{}).Validate(true); err == nil {
		t.Fatal("implicit conversion accepted")
	}
	if err := (ImportOptions{FixedMemory: true}).Validate(true); err != nil {
		t.Fatal(err)
	}
	if err := (ImportOptions{MemoryPoolName: "local"}).Validate(true); err != nil {
		t.Fatal(err)
	}
	if err := (ImportOptions{MemoryPoolName: "local", FixedMemory: true}).Validate(false); err == nil {
		t.Fatal("conflicting options accepted")
	}
	m.Version = 2
	if err := m.validate(); err == nil {
		t.Fatal("pooled artifact accepted as old version")
	}
}

func TestImportDestinationMemoryPolicy(t *testing.T) {
	for _, sourceMode := range []string{"fixed", "pool"} {
		for _, policy := range []string{"implicit", "fixed", "pool", "missing"} {
			t.Run(sourceMode+"/"+policy, func(t *testing.T) {
				ctx := context.Background()
				cfg := loadProvisionTestConfig(t, nil)
				st := newProvisionTestStore(t, cfg)
				p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), readFilesystemBytes: host.DefaultReadFilesystemBytes}
				now := time.Now().UTC()
				pool := model.MemoryPool{ID: "destination-local-id", Name: "destination", ReservedBytes: 1024 * 1024 * 1024, CreatedAt: now, UpdatedAt: now}
				if err := st.CreateMemoryPool(ctx, pool); err != nil {
					t.Fatal(err)
				}
				inst := provisionTestInstance(cfg, "imported", model.StateStopped, now)
				if err := os.MkdirAll(filepath.Dir(filepath.Dir(inst.RootFSPath)), 0o770); err != nil {
					t.Fatal(err)
				}
				inst.MemoryMode, inst.MemoryPoolID, inst.MemoryMiB, inst.VCPUCount, inst.RootFSSizeBytes = sourceMode, "source-local-id", 512, 1, 6
				m := portableManifestFromInstance(now, inst)
				stream := portableArtifactTestStream(t, m, map[string][]byte{backupRootFSName: []byte("rootfs")})
				opts := ImportOptions{}
				switch policy {
				case "fixed":
					opts.FixedMemory = true
				case "pool":
					opts.MemoryPoolName = "destination"
				case "missing":
					opts.MemoryPoolName = "missing"
				}
				got, _, err := p.ImportInstanceWithOptions(ctx, model.Actor{}, bytes.NewReader(stream), opts)
				if policy == "missing" || sourceMode == "pool" && policy == "implicit" {
					if err == nil {
						t.Fatal("invalid import accepted")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got.State != model.StateStopped {
					t.Fatalf("import not stopped: %+v", got)
				}
				if policy == "pool" {
					if got.MemoryMode != "pool" || got.MemoryPoolID != pool.ID {
						t.Fatalf("wrong destination pool: %+v", got)
					}
				} else if got.MemoryMode != "fixed" || got.MemoryPoolID != "" {
					t.Fatalf("source pool leaked: %+v", got)
				}
			})
		}
	}
}

func TestFailedMigrationStopRetainsFence(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	inst := provisionTestInstance(cfg, "uncertain", model.StateReady, time.Now().UTC())
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	// No runtime helper: stop cannot be confirmed, so export/complete and all
	// starts must remain prohibited rather than rolling back the durable fence.
	if _, err := p.PrepareMigration(ctx, inst.Name, "destination", "transfer"); err == nil {
		t.Fatal("unconfirmed stop prepared migration")
	}
	m, err := p.MigrationStatus(ctx, inst.Name)
	if err != nil || m.Phase != "preparing" || !m.WasRunning {
		t.Fatalf("lost stop fence: %+v, %v", m, err)
	}
	if _, err := p.Start(ctx, inst.Name); err == nil {
		t.Fatal("source started after unconfirmed stop")
	}
	if _, err := p.FinishMigration(ctx, inst.Name, "transfer", false); err == nil {
		t.Fatal("unprepared migration completed")
	}
	if _, err := p.ExportInstance(ctx, inst.Name, io.Discard); err == nil {
		t.Fatal("unconfirmed source exported")
	}
}

func TestImportInstanceIDCollisionPreservesExistingJournal(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), readFilesystemBytes: host.DefaultReadFilesystemBytes}
	inst := provisionTestInstance(cfg, "existing", model.StateStopped, time.Now().UTC())
	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.RootFSPath, []byte("original source disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PrepareMigration(ctx, inst.Name, "destination", "original-token"); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetInstance(ctx, inst.Name)
	if err != nil {
		t.Fatal(err)
	}
	migrationBefore, err := p.MigrationStatus(ctx, inst.Name)
	if err != nil {
		t.Fatal(err)
	}
	manifest := portableManifest{Version: portableManifestVersion, MigrationToken: "collision-token", ExportedAt: time.Now().UTC(), Instance: portableInstance{ID: inst.ID, Name: "different-name", CreatedAt: inst.CreatedAt, RootFSSizeBytes: 6}, Files: backupFiles{RootFS: backupRootFSName}}
	stream := portableArtifactTestStream(t, manifest, map[string][]byte{backupRootFSName: []byte("rootfs")})
	for _, phase := range []string{"importing", "imported"} {
		t.Run(phase, func(t *testing.T) {
			receipt := model.ImportReceipt{InstanceID: inst.ID, Token: "original-token", SourceInstanceID: "original-source", ExportedAt: "original-export", Phase: phase}
			if err := st.PutImportReceipt(ctx, receipt); err != nil {
				t.Fatal(err)
			}
			if _, _, err := p.ImportInstance(ctx, model.Actor{}, bytes.NewReader(stream)); err == nil || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("colliding import: %v", err)
			}
			got, err := st.GetImportReceipt(ctx, inst.ID)
			if err != nil || got != receipt {
				t.Fatalf("existing receipt changed: %+v, %v", got, err)
			}
			after, err := st.GetInstance(ctx, inst.Name)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("existing instance changed: %+v, %v", after, err)
			}
			migrationAfter, err := p.MigrationStatus(ctx, inst.Name)
			if err != nil || migrationAfter != migrationBefore {
				t.Fatalf("existing source fence changed: %+v, %v", migrationAfter, err)
			}
			payload, err := os.ReadFile(inst.RootFSPath)
			if err != nil || string(payload) != "original source disk" {
				t.Fatalf("existing disk changed: %q, %v", payload, err)
			}
			if _, found, err := st.FindInstance(ctx, "different-name"); err != nil || found {
				t.Fatalf("collision left reservation: %v, %v", found, err)
			}
		})
	}
}
