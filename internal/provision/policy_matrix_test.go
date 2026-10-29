package provision

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"srv/internal/config"
	"srv/internal/model"
	"srv/internal/store"
)

func policyLabel(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

func newPolicyInstance(t *testing.T, cfg config.Config, st *store.Store, name, migration, importPhase, fence string) model.Instance {
	t.Helper()
	ctx := context.Background()
	inst := provisionTestInstance(cfg, name, model.StateStopped, time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC))
	inst.Kind, inst.Image, inst.TailnetNodeID = model.KindOrb, "amp-orb", ""
	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.RootFSPath, []byte("policy data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	requestID := "req-" + name
	if admitted, err := st.AdmitLifecycleRequest(ctx, store.LifecycleRequest{ID: requestID, Actor: "owner", Command: "delete-proof", Name: name, Arguments: "bound"}); err != nil || !admitted {
		t.Fatalf("admit %s: admitted=%v err=%v", name, admitted, err)
	}
	if migration != "" {
		if err := st.PutMigration(ctx, model.Migration{InstanceID: inst.ID, Token: "tok", Target: "target", Phase: migration}); err != nil {
			t.Fatal(err)
		}
	}
	if importPhase != "" {
		if err := st.PutImportReceipt(ctx, model.ImportReceipt{InstanceID: inst.ID, Phase: importPhase}); err != nil {
			t.Fatal(err)
		}
	}
	if fence != "" {
		if err := st.BeginCleanup(ctx, requestID, "owner", inst); err != nil {
			t.Fatal(err)
		}
	}
	return inst
}

func TestProvisionPolicyMatrix(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{
		cfg:           cfg,
		store:         st,
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		vmRunner:      cleanupRunner{verify: func() error { return nil }},
		networkHelper: cleanupNetwork{verify: func() error { return nil }},
	}

	migrations := []string{"", "preparing", "prepared", "completed", "aborted"}
	imports := []string{"", "importing", "imported"}
	fences := []string{"", "fenced"}
	for _, migration := range migrations {
		for _, importPhase := range imports {
			for _, fence := range fences {
				// A preparing/prepared/completed migration cannot coexist with
				// an importing receipt through the public import path:
				// ReserveImportReceipt refuses an existing instance ID, and
				// PrepareMigration is fenced while the receipt is importing. A
				// preparing retry would bypass that fence, so do not model an
				// unreachable mixed state as policy.
				if importPhase == "importing" && (migration == "preparing" || migration == "prepared" || migration == "completed") {
					continue
				}
				name := "policy-" + policyLabel(migration) + "-" + policyLabel(importPhase) + "-" + policyLabel(fence)
				t.Run(name, func(t *testing.T) {
					prepareInst := newPolicyInstance(t, cfg, st, name+"-prep", migration, importPhase, fence)
					deleteInst := newPolicyInstance(t, cfg, st, name+"-del", migration, importPhase, fence)

					prepareWant := fence == "" && importPhase != "importing"
					token, target := "tok-new", "target-new"
					if migration == "preparing" || migration == "prepared" || migration == "completed" {
						// Interrupted preparation retries with the same binding;
						// a changed token or target must be rejected.
						token, target = "tok", "target"
					}
					if migration == "preparing" && fence == "" && importPhase != "importing" {
						if _, err := p.PrepareMigration(ctx, prepareInst.Name, target+"-changed", token+"-changed"); err == nil {
							t.Fatal("PrepareMigration() accepted a changed binding for an interrupted preparation")
						}
					}
					prepared, err := p.PrepareMigration(ctx, prepareInst.Name, target, token)
					if prepareWant {
						if err != nil {
							t.Fatalf("PrepareMigration() = %+v, %v; want success", prepared, err)
						}
						wantPhase := "prepared"
						if migration == "completed" {
							wantPhase = "completed"
						}
						if prepared.Phase != wantPhase {
							t.Fatalf("PrepareMigration() phase = %q, want %q", prepared.Phase, wantPhase)
						}
					} else if err == nil {
						t.Fatalf("PrepareMigration() succeeded in fence=%q import=%q", fence, importPhase)
					}

					// This is the owning delete request, so its own fence permits
					// resumption. Only an active outgoing migration excludes delete.
					deleteWant := migration == "" || migration == "aborted" || migration == "completed"
					_, err = p.DeleteWithProof(ctx, deleteInst, "req-"+deleteInst.Name, "owner", "backend-one", func(context.Context, model.Instance) error { return nil })
					stored, getErr := st.GetInstance(ctx, deleteInst.Name)
					if getErr != nil {
						t.Fatal(getErr)
					}
					if deleteWant {
						if err != nil {
							t.Fatalf("DeleteWithProof() = %v; want success", err)
						}
						if stored.State != model.StateDeleted || stored.DeletedAt == nil {
							t.Fatalf("deleted instance = %+v", stored)
						}
						if _, statErr := os.Stat(deleteInst.RootFSPath); !errors.Is(statErr, os.ErrNotExist) {
							t.Fatalf("rootfs remains after delete: %v", statErr)
						}
						if _, receiptErr := st.CleanupReceipt(ctx, "req-"+deleteInst.Name, "owner"); receiptErr != nil {
							t.Fatalf("missing receipt: %v", receiptErr)
						}
					} else {
						if err == nil {
							t.Fatalf("DeleteWithProof() succeeded in migration=%q import=%q fence=%q", migration, importPhase, fence)
						}
						if stored.State == model.StateDeleted {
							t.Fatalf("failed delete tombstoned the instance: %+v", stored)
						}
						if _, statErr := os.Stat(deleteInst.RootFSPath); statErr != nil {
							t.Fatalf("failed delete removed the rootfs: %v", statErr)
						}
						if _, receiptErr := st.CleanupReceipt(ctx, "req-"+deleteInst.Name, "owner"); receiptErr == nil {
							t.Fatal("failed delete published a receipt")
						}
					}
				})
			}
		}
	}
}
