package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"srv/internal/config"
	"srv/internal/model"
)

func TestAuthorizeSnapshotRequiresAdmin(t *testing.T) {
	app := &App{cfg: config.Config{AdminUsers: []string{"ops@example.com"}}}

	allowed, reason := app.authorize(model.Actor{UserLogin: "alice@example.com"}, "snapshot")
	if allowed {
		t.Fatal("authorize(snapshot) unexpectedly allowed non-admin user")
	}
	if reason != "alice@example.com is not in SRV_ADMIN_USERS" {
		t.Fatalf("authorize(snapshot) reason = %q", reason)
	}

	allowed, reason = app.authorize(model.Actor{UserLogin: "ops@example.com"}, "snapshot")
	if !allowed {
		t.Fatal("authorize(snapshot) denied admin user")
	}
	if reason != "ops@example.com allowed to run snapshot as admin" {
		t.Fatalf("authorize(snapshot) reason = %q", reason)
	}
}

func TestAuthorizeSnapshotRequiresAllowlistMembershipWhenEnabled(t *testing.T) {
	app := &App{cfg: config.Config{AllowedUsers: []string{"alice@example.com"}, AdminUsers: []string{"ops@example.com"}}}

	allowed, reason := app.authorize(model.Actor{UserLogin: "ops@example.com"}, "snapshot")
	if allowed {
		t.Fatal("authorize(snapshot) unexpectedly allowed admin outside SRV_ALLOWED_USERS")
	}
	if reason != "ops@example.com is not in SRV_ALLOWED_USERS" {
		t.Fatalf("authorize(snapshot) reason = %q", reason)
	}
}

func TestCommandGateBlocksNewCommandsDuringSnapshotBarrier(t *testing.T) {
	var app App

	first, err := app.beginCommand()
	if err != nil {
		t.Fatalf("beginCommand(first): %v", err)
	}
	snapshot, err := app.beginCommand()
	if err != nil {
		first.Release()
		t.Fatalf("beginCommand(snapshot): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	promoteDone := make(chan error, 1)
	go func() {
		promoteDone <- snapshot.PromoteToSnapshot(ctx)
	}()

	waitForSnapshotBarrier(t, &app)

	if lease, err := app.beginCommand(); !errors.Is(err, errSnapshotInProgress) {
		if lease != nil {
			lease.Release()
		}
		t.Fatalf("beginCommand() during snapshot barrier error = %v, want %v", err, errSnapshotInProgress)
	}
	select {
	case err := <-promoteDone:
		t.Fatalf("PromoteToSnapshot() returned early: %v", err)
	default:
	}

	first.Release()
	select {
	case err := <-promoteDone:
		if err != nil {
			t.Fatalf("PromoteToSnapshot(): %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PromoteToSnapshot() did not finish after in-flight command drained")
	}

	if lease, err := app.beginCommand(); !errors.Is(err, errSnapshotInProgress) {
		if lease != nil {
			lease.Release()
		}
		t.Fatalf("beginCommand() after snapshot promotion error = %v, want %v", err, errSnapshotInProgress)
	}

	snapshot.Release()
	next, err := app.beginCommand()
	if err != nil {
		t.Fatalf("beginCommand() after snapshot release: %v", err)
	}
	next.Release()
}

func TestPromoteToSnapshotContextCancelRestoresGate(t *testing.T) {
	var app App

	first, err := app.beginCommand()
	if err != nil {
		t.Fatalf("beginCommand(first): %v", err)
	}
	snapshot, err := app.beginCommand()
	if err != nil {
		first.Release()
		t.Fatalf("beginCommand(snapshot): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- snapshot.PromoteToSnapshot(ctx)
	}()

	waitForSnapshotBarrier(t, &app)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("PromoteToSnapshot() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("PromoteToSnapshot() did not return after context cancellation")
	}

	next, err := app.beginCommand()
	if err != nil {
		t.Fatalf("beginCommand() after cancellation: %v", err)
	}
	next.Release()
	snapshot.Release()
	first.Release()
}

func waitForSnapshotBarrier(t *testing.T, app *App) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		lease, err := app.beginCommand()
		if errors.Is(err, errSnapshotInProgress) {
			return
		}
		if err != nil {
			t.Fatalf("beginCommand() while waiting for snapshot barrier: %v", err)
		}
		lease.Release()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("snapshot barrier never became active")
}
