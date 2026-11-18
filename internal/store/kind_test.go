package store

import (
	"context"
	"testing"
	"time"

	"srv/internal/model"
)

func TestInstanceKindPersistenceAndLegacyMigration(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	legacy := testInstance("legacy", model.StateStopped, time.Now().UTC())
	legacy.Kind = ""
	if err := s.CreateInstance(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	// Reproduce a pre-kind database with an existing row, then migrate it.
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE instances DROP COLUMN kind"); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetInstance(ctx, legacy.Name)
	if err != nil || got.Kind != model.KindVM {
		t.Fatalf("legacy kind = %q, err = %v", got.Kind, err)
	}
	orb := testInstance("orb", model.StateAwaitingGuest, time.Now().UTC())
	orb.Kind = model.KindOrb
	if err := s.CreateInstance(ctx, orb); err != nil {
		t.Fatal(err)
	}
	orb.State = model.StateReady
	if err := s.UpdateInstance(ctx, orb); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetInstance(ctx, orb.Name)
	if err != nil || got.Kind != model.KindOrb || got.State != model.StateReady {
		t.Fatalf("orb = %#v, err = %v", got, err)
	}
	all, err := s.ListInstances(ctx, false)
	if err != nil || len(all) != 2 {
		t.Fatalf("unfiltered store list = %#v, err = %v", all, err)
	}
	orb.Kind = "invalid"
	if err := s.UpdateInstance(ctx, orb); err == nil {
		t.Fatal("invalid kind accepted")
	}
}

func TestGuestNetworkPersistsAndIsImmutable(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	guest := testInstance("guest", model.StateProvisioning, time.Now().UTC())
	guest.GuestNetwork = model.GuestNetworkHost
	if err := s.CreateInstance(ctx, guest); err != nil {
		t.Fatal(err)
	}
	// Lifecycle updates cannot turn a guest without tailnet into a tailnet one.
	guest.GuestNetwork, guest.State = model.GuestNetworkTailnet, model.StateReady
	if err := s.UpdateInstance(ctx, guest); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetInstance(ctx, "guest")
	if err != nil || got.GuestNetwork != model.GuestNetworkHost || got.State != model.StateReady {
		t.Fatalf("guest network = %q, state = %s, err = %v", got.GuestNetwork, got.State, err)
	}
	listed, err := s.ListInstances(ctx, false)
	if err != nil || len(listed) != 1 || listed[0].GuestNetwork != model.GuestNetworkHost {
		t.Fatalf("listed = %#v, err = %v", listed, err)
	}
	bad := testInstance("bad", model.StateProvisioning, time.Now().UTC())
	bad.GuestNetwork = "plain"
	if err := s.CreateInstance(ctx, bad); err == nil {
		t.Fatal("unknown guest network accepted")
	}
}
