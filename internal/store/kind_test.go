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
