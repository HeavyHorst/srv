package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestLifecycleRequestRecoveryAndBinding(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	i, receipt := cleanupFixture(t, s)
	r := LifecycleRequest{ID: receipt.RequestID, Actor: receipt.Actor, Command: "delete-proof", Name: i.Name, Arguments: "bound-arguments"}
	if err := s.BeginCleanup(ctx, r.ID, r.Actor, i); err != nil {
		t.Fatal(err)
	}
	queued := r
	queued.ID = "queued"
	if ok, err := s.AdmitLifecycleRequest(ctx, queued); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.InterruptLifecycleRequests(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{r.ID: "interrupted", "queued": "cancelled"} {
		got, err := s.GetLifecycleRequest(ctx, id, r.Actor)
		if err != nil || got.Phase != want {
			t.Fatal(got, err)
		}
	}
	changed := r
	changed.Arguments = `["start","different"]`
	if _, err := s.AdmitLifecycleRequest(ctx, changed); err == nil {
		t.Fatal("changed arguments accepted")
	}
	if ok, err := s.AdmitLifecycleRequest(ctx, r); err != nil || ok {
		t.Fatal("interrupted request readmitted", ok, err)
	}
}

func TestLifecycleRequestSurvivesReopenWithBoundCleanupResume(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	i, receipt := cleanupFixture(t, s)
	r := LifecycleRequest{ID: receipt.RequestID, Actor: receipt.Actor, Command: "delete-proof", Name: i.Name, Arguments: "bound-arguments"}
	if err := s.BeginCleanup(ctx, r.ID, r.Actor, i); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Opening the DB for inspection must not rewrite live execution state.
	got, err := s.GetLifecycleRequest(ctx, r.ID, r.Actor)
	if err != nil || got.Phase != "executing" {
		t.Fatal(got, err)
	}
	if err := s.InterruptLifecycleRequests(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AdmitLifecycleRequest(ctx, r); err != nil || ok {
		t.Fatalf("interrupted delete replay: %v %v", ok, err)
	}
	if _, err := s.GetLifecycleRequest(ctx, r.ID, "bob"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign actor obtained receipt: %v", err)
	}
	foreign := r
	foreign.Actor = "bob"
	if _, err := s.AdmitLifecycleRequest(ctx, foreign); err == nil {
		t.Fatal("foreign actor rebound request")
	}
	got, err = s.GetLifecycleRequest(ctx, r.ID, r.Actor)
	if err != nil || got.Phase != "interrupted" || got.Arguments != r.Arguments {
		t.Fatal(got, err)
	}
	if finished, err := s.FinishLifecycleRequest(ctx, r.ID, r.Actor, 0); err != nil || finished {
		t.Fatalf("interrupted request completed: %v %v", finished, err)
	}
	if err := s.BeginCleanup(ctx, r.ID, r.Actor, i); err != nil {
		t.Fatal("bound cleanup resume refused", err)
	}
	if err := s.CheckCleanupFence(ctx, i.ID); err == nil {
		t.Fatal("resume removed its fence before completion")
	}
}

func TestLifecycleRequestCannotSucceedBeforeExecution(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	r := LifecycleRequest{ID: "validation", Actor: "alice", Command: "delete", Name: "vm", Arguments: `["delete","vm"]`}
	if ok, err := s.AdmitLifecycleRequest(ctx, r); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if finished, err := s.FinishLifecycleRequest(ctx, r.ID, r.Actor, 0); err != nil || finished {
		t.Fatalf("unexecuted request succeeded: %v %v", finished, err)
	}
	got, err := s.GetLifecycleRequest(ctx, r.ID, r.Actor)
	if err != nil || got.Phase != "admitted" {
		t.Fatal(got, err)
	}
	if finished, err := s.FinishLifecycleRequest(ctx, r.ID, r.Actor, 2); err != nil || !finished {
		t.Fatalf("validation failure not recorded: %v %v", finished, err)
	}
	got, err = s.GetLifecycleRequest(ctx, r.ID, r.Actor)
	if err != nil || got.Phase != "failed" || got.ExitCode != 2 {
		t.Fatal(got, err)
	}
	if finished, err := s.FinishLifecycleRequest(ctx, r.ID, r.Actor, 0); err != nil || finished {
		t.Fatalf("failure overwritten: %v %v", finished, err)
	}
}

func TestBootstrapAdmissionDurabilityAndRollbackRefusal(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	i, _ := cleanupFixture(t, s)
	r := LifecycleRequest{ID: "bootstrap-one", Actor: "owner", Command: "bootstrap", Name: i.Name, Arguments: "incarnation-generation-one"}
	if err := s.AdmitBootstrap(ctx, r, i.ID, i.CreatedAt, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InterruptLifecycleRequests(ctx); err != nil {
		t.Fatal(err)
	}
	old, err := s.GetLifecycleRequest(ctx, r.ID, r.Actor)
	if err != nil || old.Phase != "interrupted" {
		t.Fatal(old, err)
	}
	if n, err := s.BootstrapGeneration(ctx, i.ID, i.CreatedAt); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if err := s.AdmitBootstrap(ctx, r, i.ID, i.CreatedAt, 2); err == nil {
		t.Fatal("request ID reused")
	}
	if n, _ := s.BootstrapGeneration(ctx, i.ID, i.CreatedAt); n != 1 {
		t.Fatal("partial admission committed")
	}
	r.ID = "bootstrap-two"
	if err := s.AdmitBootstrap(ctx, r, i.ID, i.CreatedAt.Add(time.Nanosecond), 2); err == nil {
		t.Fatal("created-at binding ignored")
	}
	if err := s.AdmitBootstrap(ctx, r, i.ID, i.CreatedAt, 2); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.FinishLifecycleRequest(ctx, r.ID, r.Actor, 0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	for _, generation := range []int64{0, 1, 2} {
		late := r
		late.ID = "late"
		if err := s.AdmitBootstrap(ctx, late, i.ID, i.CreatedAt, generation); err == nil {
			t.Fatal("old/equal generation admitted", generation)
		}
	}
	if err := s.DeleteInstance(ctx, i.Name); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateInstance(ctx, i); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.BootstrapGeneration(ctx, i.ID, i.CreatedAt); n != 2 {
		t.Fatal("name reuse/import reset generation")
	}
	if done, err := s.GetLifecycleRequest(ctx, r.ID, r.Actor); err != nil || done.Phase != "done" {
		t.Fatal(done, err)
	}
	if _, err := s.db.Exec(`DELETE FROM orb_bootstrap`); err != nil {
		t.Fatal(err)
	}
	r.ID = "after-loss"
	if err := s.AdmitBootstrap(ctx, r, i.ID, i.CreatedAt, 3); err == nil {
		t.Fatal("missing state adopted")
	}
}
