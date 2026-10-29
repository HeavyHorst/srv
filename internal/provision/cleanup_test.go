package provision

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"srv/internal/model"
	"srv/internal/nethelper"
	"srv/internal/store"
	"srv/internal/vmrunner"
)

type cleanupNetwork struct {
	noopNetworkHelper
	verify func() error
}

func (n cleanupNetwork) VerifyInstanceNetworkAbsent(context.Context, nethelper.CleanupRequest) error {
	return n.verify()
}

type cleanupRunner struct {
	noopVMRunner
	verify func() error
}

func (r cleanupRunner) VerifyInstanceAbsent(context.Context, vmrunner.StartResultRequest) error {
	return r.verify()
}

func (r cleanupRunner) ColdDisk(_ context.Context, req vmrunner.ColdRequest) (vmrunner.ColdStatus, error) {
	if req.Action == "status" {
		return vmrunner.ColdStatus{Phase: "hot"}, nil
	}
	return vmrunner.ColdStatus{Phase: "hot"}, r.verify()
}

func TestAuthoritativeOrbCleanup(t *testing.T) {
	for _, scenario := range []string{"success", "template", "snapshot-template", "legacy-vm", "node", "key", "import", "backup", "runtime", "network", "disk"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			cfg := loadProvisionTestConfig(t, nil)
			st := newProvisionTestStore(t, cfg)
			i := provisionTestInstance(cfg, "proof", model.StateStopped, time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC))
			i.Kind, i.Image, i.TailnetNodeID = model.KindOrb, "amp-orb", ""
			if scenario == "legacy-vm" {
				i.Kind = model.KindVM
			}
			if scenario == "node" {
				i.TailnetNodeID = "unexpected-node"
			}
			if err := os.MkdirAll(filepath.Dir(i.RootFSPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(i.RootFSPath, []byte("retained data"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateInstance(ctx, i); err != nil {
				t.Fatal(err)
			}
			if scenario == "key" {
				if err := st.RecordEnrollmentKey(ctx, i.ID, "unexpected-key"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "import" {
				if err := st.PutImportReceipt(ctx, model.ImportReceipt{InstanceID: i.ID, Phase: "imported"}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "backup" {
				if err := os.MkdirAll(filepath.Join(cfg.BackupsDir(), i.Name, "retained"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.AdmitLifecycleRequest(ctx, store.LifecycleRequest{ID: "request", Actor: "owner", Command: "delete-proof", Name: i.Name, Arguments: "bound"}); err != nil {
				t.Fatal(err)
			}
			p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), vmRunner: cleanupRunner{verify: func() error {
				if scenario == "runtime" {
					return errors.New("runtime remains")
				}
				return nil
			}}, networkHelper: cleanupNetwork{verify: func() error {
				if scenario == "network" {
					return errors.New("network remains")
				}
				return nil
			}}}
			var template string
			if scenario == "template" || scenario == "snapshot-template" {
				template = filepath.Join(cfg.DataDirAbs(), "templates", "owner", "retained")
				if scenario == "snapshot-template" {
					template = filepath.Join(cfg.SnapshotsDir(), "snapshot", "templates", "owner", "retained")
				}
				if err := os.MkdirAll(template, 0700); err != nil {
					t.Fatal(err)
				}
				manifest, _ := json.Marshal(templateManifest{Version: 1, ID: "retained", Owner: i.CreatedByUser, SourceID: i.ID, Image: i.Image, Kind: i.Kind})
				if err := os.WriteFile(filepath.Join(template, "manifest.json"), manifest, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(template, "rootfs.img"), []byte("project copy"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "disk" {
				old := removePathAll
				removePathAll = func(string) error { return errors.New("busy") }
				defer func() { removePathAll = old }()
			}
			proof, err := p.DeleteWithProof(ctx, i, "request", "owner", "backend-one", func(context.Context, model.Instance) error { return nil })
			if scenario == "success" || scenario == "template" {
				if err != nil || proof.Version != 3 || !proof.GatewaysAbsent || proof.Kind != model.KindOrb || proof.AuthKeyDisposition != "never-minted" || !proof.StorageAbsent {
					t.Fatal(proof, err)
				}
				if _, err := os.Stat(i.RootFSPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if scenario == "template" {
					if len(proof.RetainedTemplates) != 1 || proof.RetainedTemplates[0] != "owner/retained" {
						t.Fatal(proof)
					}
					if _, err := os.Stat(filepath.Join(template, "rootfs.img")); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				if err == nil {
					t.Fatal("partial proof succeeded")
				}
				if _, err := st.CleanupReceipt(ctx, "request", "owner"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatal(err)
				}
				if _, err := os.Stat(i.RootFSPath); err != nil {
					t.Fatal("disk lost", err)
				}
				if scenario == "runtime" || scenario == "network" || scenario == "disk" {
					if err := st.InterruptLifecycleRequests(ctx); err != nil {
						t.Fatal(err)
					}
					for _, operation := range []func(context.Context, string) (model.Instance, error){p.ReconcileInstance, p.Delete, p.Start, p.Stop} {
						if _, err := operation(ctx, i.Name); err == nil {
							t.Fatal("cleanup fence bypassed")
						}
					}
				}
			}
		})
	}
}
