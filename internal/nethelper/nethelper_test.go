package nethelper

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRequestsValidate(t *testing.T) {
	if err := (SetupRequest{
		TapDevice:         "tap-demo",
		HostAddr:          "10.0.0.1/30",
		NetworkCIDR:       "10.0.0.0/30",
		OutboundInterface: "eth0",
	}).Validate(); err != nil {
		t.Fatalf("SetupRequest.Validate(): %v", err)
	}
	if err := (CleanupRequest{TapDevice: "tap-demo", NetworkCIDR: "10.0.0.0/30"}).Validate(); err != nil {
		t.Fatalf("CleanupRequest.Validate(): %v", err)
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "bad tap", err: (SetupRequest{TapDevice: "nested/demo", HostAddr: "10.0.0.1/30", NetworkCIDR: "10.0.0.0/30", OutboundInterface: "eth0"}).Validate()},
		{name: "bad host", err: (SetupRequest{TapDevice: "tap-demo", HostAddr: "10.0.0.1", NetworkCIDR: "10.0.0.0/30", OutboundInterface: "eth0"}).Validate()},
		{name: "bad network", err: (CleanupRequest{TapDevice: "tap-demo", NetworkCIDR: "nope"}).Validate()},
	} {
		if tc.err == nil {
			t.Fatalf("%s unexpectedly passed validation", tc.name)
		}
	}
}

func TestClientAndServerOverUnixSocket(t *testing.T) {
	oldRuleExists := iptablesRuleExists
	t.Cleanup(func() { iptablesRuleExists = oldRuleExists })
	iptablesRuleExists = func(context.Context, string, string, ...string) bool { return false }

	var (
		mu    sync.Mutex
		calls []string
	)
	runner := func(_ context.Context, name string, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
		return nil
	}

	server := NewServerWithRunner(slog.New(slog.NewTextHandler(io.Discard, nil)), "srv", runner)
	socketPath := filepath.Join(t.TempDir(), "net-helper.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("net.Listen(unix): %v", err)
	}
	defer listener.Close()

	httpServer := &http.Server{Handler: server.Handler()}
	go func() {
		_ = httpServer.Serve(listener)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	})

	client := NewClient(socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.SetupInstanceNetwork(ctx, SetupRequest{
		TapDevice:         "tap-demo",
		HostAddr:          "10.0.0.1/30",
		NetworkCIDR:       "10.0.0.0/30",
		OutboundInterface: "eth0",
	}); err != nil {
		t.Fatalf("SetupInstanceNetwork(): %v", err)
	}
	if err := client.CleanupInstanceNetwork(ctx, CleanupRequest{
		TapDevice:         "tap-demo",
		NetworkCIDR:       "10.0.0.0/30",
		OutboundInterface: "eth0",
	}); err != nil {
		t.Fatalf("CleanupInstanceNetwork(): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	got := append([]string(nil), calls...)
	want := []string{
		"ip tuntap add dev tap-demo mode tap user srv",
		"ip addr add 10.0.0.1/30 dev tap-demo",
		"ip link set dev tap-demo up",
		"iptables -t nat -A POSTROUTING -s 10.0.0.0/30 -o eth0 -j MASQUERADE",
		"iptables -t filter -A FORWARD -i tap-demo -j ACCEPT",
		"iptables -t filter -A FORWARD -o tap-demo -m state --state RELATED,ESTABLISHED -j ACCEPT",
		"iptables -t nat -D POSTROUTING -s 10.0.0.0/30 -o eth0 -j MASQUERADE",
		"iptables -t filter -D FORWARD -i tap-demo -j ACCEPT",
		"iptables -t filter -D FORWARD -o tap-demo -m state --state RELATED,ESTABLISHED -j ACCEPT",
		"ip link set dev tap-demo down",
		"ip tuntap del dev tap-demo mode tap",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("helper commands = %#v, want %#v", got, want)
	}
}

func TestServeUnixSetsSocketPermissions(t *testing.T) {
	server := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), "srv")
	socketPath := filepath.Join(t.TempDir(), "net-helper.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ServeUnix(ctx, socketPath, "")
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := os.Stat(socketPath)
		if err == nil {
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("socket mode = %o, want 600", info.Mode().Perm())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket was not created: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("ServeUnix(): %v", err)
	}
}
