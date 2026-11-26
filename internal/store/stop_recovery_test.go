package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"srv/internal/model"
)

func TestStopRecoverySurvivesInterruptedCompletion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "recovery.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	inst := model.Instance{ID: "instance", Name: "orb", Kind: model.KindOrb, Image: "amp-orb", CreatedAt: time.Now().UTC(), CreatedByUser: "owner", State: model.StateStopping}
	if err := s.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	old := LifecycleRequest{ID: "old", Actor: "owner", Command: "stop", Name: "orb", Arguments: `["stop","orb","instance","created","backend","amp-orb"]`}
	if _, err := s.AdmitLifecycleRequest(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginLifecycleRequest(ctx, old.ID, old.Actor); err != nil {
		t.Fatal(err)
	}
	r := StopRecovery{ID: "recovery", OriginalRequestID: old.ID, Actor: old.Actor, OriginalArguments: old.Arguments, Name: inst.Name, InstanceID: inst.ID, CreatedAt: inst.CreatedAt.Format(time.RFC3339Nano), BackendID: "backend", Image: inst.Image, StartRequestID: "attempt", HostBootID: "boot", SaveState: true}
	if err := s.BeginStopRecovery(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckStopRecoveryFence(ctx, inst.ID); err == nil {
		t.Fatal("pending recovery did not fence new start")
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
	if err := s.BeginStopRecovery(ctx, r); err != nil {
		t.Fatal("crash retry:", err)
	}
	changed := r
	changed.StartRequestID = "successor"
	if err := s.BeginStopRecovery(ctx, changed); err == nil {
		t.Fatal("recovery rebound to successor")
	}
	if err := s.CompleteStopRecovery(ctx, r.ID, r.Actor); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckStopRecoveryFence(ctx, inst.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetLifecycleRequest(ctx, old.ID, old.Actor)
	if err != nil || got.Phase != "recovered" || got.Recovery == nil || got.Recovery.Phase != "done" || got.Recovery.StartRequestID != r.StartRequestID {
		t.Fatalf("missing linked proof: %+v %v", got, err)
	}
	if admitted, err := s.AdmitLifecycleRequest(ctx, old); err != nil || admitted {
		t.Fatal("old request replayed", admitted, err)
	}
	if err := s.BeginLifecycleRequest(ctx, old.ID, old.Actor); err == nil {
		t.Fatal("recovered request re-executed")
	}
	if err := s.CompleteStopRecovery(ctx, r.ID, r.Actor); err != nil {
		t.Fatal("completion retry:", err)
	}
}
