package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"srv/internal/config"
	"srv/internal/model"
	"srv/internal/provision"
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
		cfg:   config.Config{Hostname: "srv", VCPUCount: 1, MemoryMiB: 1024},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}

	inst := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	inst.VCPUCount = 4
	inst.MemoryMiB = 4096
	inst.RootFSSizeBytes = 8 << 30
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
		"vcpus: 4\n",
		"memory: 4096 MiB\n",
		"rootfs-size: 8.0 GiB\n",
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
	for _, want := range []string{"new <name> [--cpus N] [--ram SIZE] [--rootfs-size SIZE]", "resize <name> [--cpus N] [--ram SIZE] [--rootfs-size SIZE]", "start <name>", "stop <name>", "restart <name>"} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("helpResult() missing %q in %q", want, result.stdout)
		}
	}
}

func TestCmdResizeUpdatesStoppedInstance(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{Hostname: "srv", VCPUCount: 1, MemoryMiB: 1024}
	prov, err := provision.New(cfg, logger, st)
	if err != nil {
		t.Fatalf("provision.New(): %v", err)
	}
	app := &App{cfg: cfg, log: logger, store: st, provisioner: prov}

	inst := serviceTestInstance("alpha", model.StateStopped, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	result, err := app.cmdResize(ctx, []string{"resize", inst.Name, "--cpus", "4", "--ram", "6G"})
	if err != nil {
		t.Fatalf("cmdResize(): %v", err)
	}
	if result.exitCode != 0 {
		t.Fatalf("cmdResize() exitCode = %d, want 0", result.exitCode)
	}
	for _, want := range []string{"resized: alpha\n", "state: stopped\n", "vcpus: 4\n", "memory: 6144 MiB\n"} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("cmdResize() stdout missing %q\nfull output:\n%s", want, result.stdout)
		}
	}

	updated, err := st.GetInstance(ctx, inst.Name)
	if err != nil {
		t.Fatalf("GetInstance(): %v", err)
	}
	if updated.VCPUCount != 4 || updated.MemoryMiB != 6144 || updated.RootFSSizeBytes != inst.RootFSSizeBytes {
		t.Fatalf("updated instance = %#v", updated)
	}
	if !updated.UpdatedAt.After(inst.UpdatedAt) {
		t.Fatalf("updated timestamp did not advance: before=%s after=%s", inst.UpdatedAt, updated.UpdatedAt)
	}
}

func TestParseNewArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantName string
		wantOpts provision.CreateOptions
		wantErr  string
	}{
		{
			name:     "parses name before flags",
			args:     []string{"new", "demo", "--cpus", "2", "--ram", "4G", "--rootfs-size", "12G"},
			wantName: "demo",
			wantOpts: provision.CreateOptions{VCPUCount: 2, MemoryMiB: 4096, RootFSSizeBytes: 12 << 30},
		},
		{
			name:     "parses flags before name and plain mib values",
			args:     []string{"new", "--ram=1536", "--cpus=4", "demo"},
			wantName: "demo",
			wantOpts: provision.CreateOptions{VCPUCount: 4, MemoryMiB: 1536},
		},
		{
			name:    "rejects unknown options",
			args:    []string{"new", "demo", "--wat", "1"},
			wantErr: `unknown option "--wat"`,
		},
		{
			name:    "requires a name",
			args:    []string{"new", "--cpus", "2"},
			wantErr: newUsage(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotOpts, err := parseNewArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseNewArgs() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseNewArgs() error = %v", err)
			}
			if gotName != tt.wantName {
				t.Fatalf("parseNewArgs() name = %q, want %q", gotName, tt.wantName)
			}
			if !reflect.DeepEqual(gotOpts, tt.wantOpts) {
				t.Fatalf("parseNewArgs() opts = %#v, want %#v", gotOpts, tt.wantOpts)
			}
		})
	}
}

func TestParseResizeArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantName string
		wantOpts provision.CreateOptions
		wantErr  string
	}{
		{
			name:     "parses name and one flag",
			args:     []string{"resize", "demo", "--rootfs-size", "12G"},
			wantName: "demo",
			wantOpts: provision.CreateOptions{RootFSSizeBytes: 12 << 30},
		},
		{
			name:    "requires at least one option",
			args:    []string{"resize", "demo"},
			wantErr: "resize requires at least one of --cpus, --ram, or --rootfs-size",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotOpts, err := parseResizeArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseResizeArgs() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseResizeArgs() error = %v", err)
			}
			if gotName != tt.wantName {
				t.Fatalf("parseResizeArgs() name = %q, want %q", gotName, tt.wantName)
			}
			if !reflect.DeepEqual(gotOpts, tt.wantOpts) {
				t.Fatalf("parseResizeArgs() opts = %#v, want %#v", gotOpts, tt.wantOpts)
			}
		})
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
		ID:              name + "-id",
		Name:            name,
		State:           state,
		CreatedAt:       createdAt,
		UpdatedAt:       createdAt.Add(30 * time.Second),
		CreatedByUser:   "alice@example.com",
		CreatedByNode:   "laptop",
		VCPUCount:       2,
		MemoryMiB:       2048,
		RootFSSizeBytes: 4 << 30,
		RootFSPath:      filepath.Join(baseDir, "rootfs.img"),
		KernelPath:      filepath.Join(baseDir, "vmlinux"),
		InitrdPath:      filepath.Join(baseDir, "initrd.img"),
		SocketPath:      filepath.Join(baseDir, "firecracker.sock"),
		LogPath:         filepath.Join(baseDir, "firecracker.log"),
		SerialLogPath:   filepath.Join(baseDir, "serial.log"),
		TapDevice:       "tap-1234567890",
		GuestMAC:        "02:fc:aa:bb:cc:dd",
		NetworkCIDR:     "172.28.0.0/30",
		HostAddr:        "172.28.0.1/30",
		GuestAddr:       "172.28.0.2/30",
		GatewayAddr:     "172.28.0.1",
	}
}
