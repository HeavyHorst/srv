package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"srv/internal/config"
	"srv/internal/model"
	"srv/internal/store"
)

func TestAuthorize(t *testing.T) {
	actor := model.Actor{UserLogin: "alice@example.com"}

	tests := []struct {
		name         string
		allowedUsers []string
		actor        model.Actor
		command      string
		wantAllowed  bool
		wantReason   string
	}{
		{
			name:         "empty allowlist permits all",
			allowedUsers: nil,
			actor:        actor,
			command:      "list",
			wantAllowed:  true,
			wantReason:   "allowed because SRV_ALLOWED_USERS is empty",
		},
		{
			name:         "allowlist matches case-insensitively",
			allowedUsers: []string{"ALICE@example.com"},
			actor:        actor,
			command:      "new",
			wantAllowed:  true,
			wantReason:   "alice@example.com allowed to run new",
		},
		{
			name:         "unknown user denied",
			allowedUsers: []string{"bob@example.com"},
			actor:        actor,
			command:      "delete",
			wantAllowed:  false,
			wantReason:   "alice@example.com is not in SRV_ALLOWED_USERS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := &App{cfg: config.Config{AllowedUsers: tt.allowedUsers}}
			allowed, reason := app.authorize(tt.actor, tt.command)
			if allowed != tt.wantAllowed {
				t.Fatalf("authorize() allowed = %v, want %v", allowed, tt.wantAllowed)
			}
			if reason != tt.wantReason {
				t.Fatalf("authorize() reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func TestCmdListFormatsVisibleInstances(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	app := &App{
		cfg:   config.Config{Hostname: "srv"},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}

	ready := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	ready.TailscaleIP = "100.64.0.10"
	ready.TailscaleName = "alpha.tailnet"
	deleted := serviceTestInstance("beta", model.StateDeleted, ready.CreatedAt.Add(time.Minute))

	if err := st.CreateInstance(ctx, ready); err != nil {
		t.Fatalf("CreateInstance(ready): %v", err)
	}
	if err := st.CreateInstance(ctx, deleted); err != nil {
		t.Fatalf("CreateInstance(deleted): %v", err)
	}

	result, err := app.cmdList(ctx)
	if err != nil {
		t.Fatalf("cmdList(): %v", err)
	}
	if result.exitCode != 0 {
		t.Fatalf("cmdList() exitCode = %d, want 0", result.exitCode)
	}
	if want := "alpha\tready\t100.64.0.10\talpha.tailnet\n"; result.stdout != want {
		t.Fatalf("cmdList() stdout = %q, want %q", result.stdout, want)
	}
}

func TestCmdInspectFormatsInstanceAndEvents(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	app := &App{
		cfg:   config.Config{Hostname: "srv"},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}

	inst := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	inst.TailscaleName = "alpha.tailnet"
	inst.TailscaleIP = "100.64.0.10"
	inst.LastError = "previous boot hiccup"
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if err := st.RecordEvent(ctx, model.InstanceEvent{
		InstanceID: inst.ID,
		CreatedAt:  inst.CreatedAt.Add(10 * time.Second),
		Type:       "create",
		Message:    "instance record created",
	}); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}

	result, err := app.cmdInspect(ctx, []string{"inspect", inst.Name})
	if err != nil {
		t.Fatalf("cmdInspect(): %v", err)
	}
	if result.exitCode != 0 {
		t.Fatalf("cmdInspect() exitCode = %d, want 0", result.exitCode)
	}

	wants := []string{
		"name: alpha\n",
		"state: ready\n",
		"created-by: alice@example.com via laptop\n",
		"tailscale-name: alpha.tailnet\n",
		"tailscale-ip: 100.64.0.10\n",
		"last-error: previous boot hiccup\n",
		"events:\n",
		"- 2026-03-29T12:00:10Z [create] instance record created\n",
	}
	for _, want := range wants {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("cmdInspect() stdout missing %q\nfull output:\n%s", want, result.stdout)
		}
	}
}

func TestCmdInspectMissingInstanceReturnsFriendlyError(t *testing.T) {
	app := &App{
		cfg:   config.Config{Hostname: "srv"},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: newServiceTestStore(t),
	}

	result, err := app.cmdInspect(context.Background(), []string{"inspect", "missing"})
	if err == nil {
		t.Fatalf("cmdInspect() error = nil, want non-nil")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cmdInspect() returned unrelated not-exist error: %v", err)
	}
	if !strings.Contains(err.Error(), `instance "missing" does not exist`) {
		t.Fatalf("cmdInspect() error = %q, want friendly missing-instance message", err.Error())
	}
	if result.exitCode != 1 {
		t.Fatalf("cmdInspect() exitCode = %d, want 1", result.exitCode)
	}
	if !strings.Contains(result.stderr, `inspect missing: instance "missing" does not exist`) {
		t.Fatalf("cmdInspect() stderr = %q", result.stderr)
	}
}

func TestTrimNodeName(t *testing.T) {
	if got := trimNodeName("node.example.", "fallback."); got != "node.example" {
		t.Fatalf("trimNodeName(primary) = %q, want %q", got, "node.example")
	}
	if got := trimNodeName("", "fallback."); got != "fallback" {
		t.Fatalf("trimNodeName(fallback) = %q, want %q", got, "fallback")
	}
}

func TestEnsureHostSignerPersistsKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "host_key")

	signer1, err := ensureHostSigner(path)
	if err != nil {
		t.Fatalf("ensureHostSigner(create): %v", err)
	}
	signer2, err := ensureHostSigner(path)
	if err != nil {
		t.Fatalf("ensureHostSigner(reuse): %v", err)
	}

	if !bytes.Equal(signer1.PublicKey().Marshal(), signer2.PublicKey().Marshal()) {
		t.Fatalf("public keys differ between create and reuse")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("host key mode = %o, want 600", info.Mode().Perm())
	}
}

func TestHelpResultIncludesLifecycleCommands(t *testing.T) {
	result := helpResult()
	for _, want := range []string{"start <name>", "stop <name>", "restart <name>"} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("helpResult() missing %q in %q", want, result.stdout)
		}
	}
}

func newServiceTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "app.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
	})
	return st
}

func serviceTestInstance(name, state string, createdAt time.Time) model.Instance {
	baseDir := filepath.Join("/tmp", name)
	return model.Instance{
		ID:            name + "-id",
		Name:          name,
		State:         state,
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt.Add(30 * time.Second),
		CreatedByUser: "alice@example.com",
		CreatedByNode: "laptop",
		RootFSPath:    filepath.Join(baseDir, "rootfs.img"),
		KernelPath:    filepath.Join(baseDir, "vmlinux"),
		InitrdPath:    filepath.Join(baseDir, "initrd.img"),
		SocketPath:    filepath.Join(baseDir, "firecracker.sock"),
		LogPath:       filepath.Join(baseDir, "firecracker.log"),
		SerialLogPath: filepath.Join(baseDir, "serial.log"),
		TapDevice:     "tap-1234567890",
		GuestMAC:      "02:fc:aa:bb:cc:dd",
		NetworkCIDR:   "172.28.0.0/30",
		HostAddr:      "172.28.0.1/30",
		GuestAddr:     "172.28.0.2/30",
		GatewayAddr:   "172.28.0.1",
	}
}
