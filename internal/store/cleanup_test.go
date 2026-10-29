package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"srv/internal/model"
)

// cleanupValues builds the fixture without touching the store so crash tests can
// interleave the individual writes.
func cleanupValues() (model.Instance, model.CleanupReceipt) {
	i := testInstance("cleanup", model.StateStopped, time.Date(2026, 10, 22, 1, 2, 3, 4, time.UTC))
	i.Kind, i.Image, i.TailnetNodeID = model.KindOrb, "amp-orb", ""
	r := model.CleanupReceipt{Version: 3, BackendID: "backend-one", Kind: i.Kind, Image: i.Image, RetainedTemplates: []string{}, RequestID: "request-one", Actor: "owner", InstanceID: i.ID, Name: i.Name, CreatedAt: i.CreatedAt, CreatedByUser: i.CreatedByUser, CreatedByNode: i.CreatedByNode, AuthKeyDisposition: "never-minted", IngressClosed: true, GatewaysAbsent: true, RuntimeAbsent: true, JailerAbsent: true, CgroupAbsent: true, NetworkAbsent: true, TapDevice: i.TapDevice, NetworkCIDR: i.NetworkCIDR, InstanceDir: filepath.Dir(i.RootFSPath), RootFSPath: i.RootFSPath, BackupsDir: "/backups/cleanup", StorageAbsent: true, BackupsAbsent: true, SnapshotsAbsent: true, DeletedAt: i.CreatedAt.Add(time.Hour)}
	return i, r
}

func cleanupFixture(t *testing.T, s *Store) (model.Instance, model.CleanupReceipt) {
	t.Helper()
	ctx := context.Background()
	i, r := cleanupValues()
	if err := s.CreateInstance(ctx, i); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AdmitLifecycleRequest(ctx, LifecycleRequest{ID: r.RequestID, Actor: r.Actor, Command: "delete-proof", Name: i.Name, Arguments: "bound-arguments"}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	return i, r
}

func TestCleanupAtomicCommitAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	i, r := cleanupFixture(t, s)
	if err := s.BeginCleanup(ctx, r.RequestID, r.Actor, i); err != nil {
		t.Fatal(err)
	}
	// Fail the last statement after the tombstone UPDATE, before COMMIT.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_receipt BEFORE INSERT ON cleanup_receipts BEGIN SELECT RAISE(ABORT,'injected commit failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitCleanup(ctx, r); err == nil {
		t.Fatal("injected failure ignored")
	}
	got, err := s.GetInstance(ctx, i.Name)
	if err != nil || got.State != model.StateDeleting || got.TailnetNodeID != i.TailnetNodeID {
		t.Fatal(got, err)
	}
	if _, err := s.CleanupReceipt(ctx, r.RequestID, r.Actor); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	request, err := s.GetLifecycleRequest(ctx, r.RequestID, r.Actor)
	if err != nil || request.Phase != "executing" {
		t.Fatal(request, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_receipt`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InterruptLifecycleRequests(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginCleanup(ctx, r.RequestID, r.Actor, i); err != nil {
		t.Fatal("resume after failed commit", err)
	}
	if err := s.CommitCleanup(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = nil
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InterruptLifecycleRequests(ctx); err != nil {
		t.Fatal(err)
	}
	proof, err := s.CleanupReceipt(ctx, r.RequestID, r.Actor)
	if err != nil || !reflect.DeepEqual(proof, r) {
		t.Fatal(proof, err)
	}
	got, err = s.GetInstance(ctx, i.Name)
	if err != nil || got.State != model.StateDeleted || got.DeletedAt == nil || !got.DeletedAt.Equal(r.DeletedAt) {
		t.Fatal(got, err)
	}
	if ok, err := s.AdmitLifecycleRequest(ctx, LifecycleRequest{ID: r.RequestID, Actor: r.Actor, Command: "delete-proof", Name: i.Name, Arguments: "bound-arguments"}); err != nil || ok {
		t.Fatal("readmitted committed delete", ok, err)
	}
	if _, err := s.CleanupReceipt(ctx, r.RequestID, "foreign"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign receipt", err)
	}
}

func TestCleanupRejectsChangedIdentityAndPartialProof(t *testing.T) {
	for _, change := range []string{"uuid", "created-at", "kind", "key", "request", "version", "gateways", "backup", "interrupted"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			i, r := cleanupFixture(t, s)
			if err := s.BeginCleanup(ctx, r.RequestID, r.Actor, i); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "uuid":
				r.InstanceID = "recreated-id"
			case "created-at":
				r.CreatedAt = r.CreatedAt.Add(time.Nanosecond)
			case "kind":
				r.Kind = model.KindVM
			case "key":
				if err := s.RecordEnrollmentKey(ctx, i.ID, "unexpected-key"); err != nil {
					t.Fatal(err)
				}
			case "request":
				r.RequestID = "foreign"
			case "version":
				r.Version = 2
			case "gateways":
				r.GatewaysAbsent = false
			case "backup":
				r.BackupsAbsent = false
			case "interrupted":
				if err := s.InterruptLifecycleRequests(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.CommitCleanup(ctx, r); err == nil {
				t.Fatal("invalid proof committed")
			}
			got, err := s.GetInstance(ctx, i.Name)
			if err != nil || got.State != model.StateDeleting {
				t.Fatal(got, err)
			}
			if _, err := s.CleanupReceipt(ctx, "request-one", "owner"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal(err)
			}
		})
	}
}

func TestHistoricalCleanupReceiptIsNotUpgraded(t *testing.T) {
	s := newTestStore(t)
	_, receipt := cleanupFixture(t, s)
	receipt.Version, receipt.GatewaysAbsent = 2, false
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	// Represent a receipt committed by the old binary, not a new v2 write.
	if _, err := s.db.Exec(`INSERT INTO cleanup_receipts(request_id,actor,receipt) VALUES(?,?,?)`, receipt.RequestID, receipt.Actor, string(data)); err != nil {
		t.Fatal(err)
	}
	got, err := s.CleanupReceipt(context.Background(), receipt.RequestID, receipt.Actor)
	if err != nil || !reflect.DeepEqual(got, receipt) {
		t.Fatal("historical receipt changed", got, err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT receipt FROM cleanup_receipts WHERE request_id=?`, receipt.RequestID).Scan(&stored); err != nil || stored != string(data) {
		t.Fatal("read rewrote receipt", stored, err)
	}
}

func TestCleanupCommitRejectsReadyInstanceWithFence(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	inst, receipt := cleanupValues()
	inst.State = model.StateReady
	if err := s.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AdmitLifecycleRequest(ctx, LifecycleRequest{ID: receipt.RequestID, Actor: receipt.Actor, Command: "delete-proof", Name: inst.Name, Arguments: "bound-arguments"}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// Reach the instance-state guard without BeginCleanup's deleting update.
	if _, err := s.db.ExecContext(ctx, `UPDATE lifecycle_requests SET phase='executing' WHERE id=? AND actor=?`, receipt.RequestID, receipt.Actor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO cleanup_fences(instance_id,request_id) VALUES(?,?)`, inst.ID, receipt.RequestID); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitCleanup(ctx, receipt); err == nil {
		t.Fatal("commit accepted a ready instance with a fence")
	}
	stored, err := s.GetInstance(ctx, inst.Name)
	if err != nil || stored.State != model.StateReady {
		t.Fatalf("instance after rejected commit = %+v, %v", stored, err)
	}
	if _, err := s.CleanupReceipt(ctx, receipt.RequestID, receipt.Actor); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("receipt after rejected commit: %v", err)
	}
	if err := s.CheckCleanupFence(ctx, inst.ID); err == nil {
		t.Fatal("fence cleared after rejected commit")
	}
}

func TestCleanupResumeRequiresOwningFence(t *testing.T) {
	for _, scenario := range []string{"failed-preflight", "cancelled-admission", "interrupted", "other-request", "missing-fence", "new-delete-missing-fence"} {
		t.Run(scenario, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			i, r := cleanupFixture(t, s)
			r.Version, r.RetainedTemplates, r.SnapshotsAbsent = 4, nil, false
			if scenario == "failed-preflight" {
				if ok, err := s.FinishLifecycleRequest(ctx, r.RequestID, r.Actor, 75); err != nil || !ok {
					t.Fatal(ok, err)
				}
			} else if scenario != "cancelled-admission" {
				if err := s.BeginCleanup(ctx, r.RequestID, r.Actor, i); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.InterruptLifecycleRequests(ctx); err != nil {
				t.Fatal(err)
			}
			if scenario == "other-request" {
				r.RequestID = "another-request"
				if _, err := s.AdmitLifecycleRequest(ctx, LifecycleRequest{ID: r.RequestID, Actor: r.Actor, Command: "delete-proof", Name: i.Name, Arguments: "same-instance"}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "missing-fence" || scenario == "new-delete-missing-fence" {
				if _, err := s.db.Exec(`DELETE FROM cleanup_fences`); err != nil {
					t.Fatal(err)
				}
				if scenario == "new-delete-missing-fence" {
					r.RequestID = "ordinary-delete"
					if _, err := s.AdmitLifecycleRequest(ctx, LifecycleRequest{ID: r.RequestID, Actor: r.Actor, Command: "delete", Name: i.Name, Arguments: "bound"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := s.BeginCleanup(ctx, r.RequestID, r.Actor, i)
			if scenario == "other-request" || scenario == "missing-fence" || scenario == "new-delete-missing-fence" {
				if err == nil {
					t.Fatal("unsafe resume accepted")
				}
				return
			}
			if err != nil {
				t.Fatal("safe retry refused", err)
			}
			if err := s.CommitCleanup(ctx, r); err != nil {
				t.Fatal(err)
			}
			got, err := s.CleanupReceipt(ctx, r.RequestID, r.Actor)
			if err != nil || !reflect.DeepEqual(got, r) {
				t.Fatal(got, err)
			}
		})
	}
}
