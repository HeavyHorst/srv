package vmrunner

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
	root := t.TempDir()
	valid := StartRequest{
		Name:        "demo",
		SocketPath:  filepath.Join(root, "firecracker.sock"),
		LogPath:     filepath.Join(root, "firecracker.log"),
		SerialLog:   filepath.Join(root, "serial.log"),
		KernelPath:  filepath.Join(root, "vmlinux"),
		RootFSPath:  filepath.Join(root, "rootfs.img"),
		TapDevice:   "tap-demo",
		GuestMAC:    "02:fc:aa:bb:cc:dd",
		GuestAddr:   "10.0.0.2/30",
		GatewayAddr: "10.0.0.1",
		Nameservers: []string{"1.1.1.1"},
		VCPUCount:   2,
		MemoryMiB:   1024,
		Bootstrap:   Bootstrap{Version: 1, Hostname: "demo"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("StartRequest.Validate(): %v", err)
	}
	if err := (StopRequest{Name: "demo", PID: 1234}).Validate(); err != nil {
		t.Fatalf("StopRequest.Validate(): %v", err)
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "bad name", err: (StartRequest{Name: "nested/demo"}).Validate()},
		{name: "bad tap", err: func() error { req := valid; req.TapDevice = "nested/demo"; return req.Validate() }()},
		{name: "bad guest ip", err: func() error { req := valid; req.GuestAddr = "10.0.0.2"; return req.Validate() }()},
		{name: "bad stop pid", err: (StopRequest{Name: "demo", PID: -1}).Validate()},
	} {
		if tc.err == nil {
			t.Fatalf("%s unexpectedly passed validation", tc.name)
		}
	}
}

func TestClientAndServerOverUnixSocket(t *testing.T) {
	root := t.TempDir()
	valid := StartRequest{
		Name:        "demo",
		SocketPath:  filepath.Join(root, "firecracker.sock"),
		LogPath:     filepath.Join(root, "firecracker.log"),
		SerialLog:   filepath.Join(root, "serial.log"),
		KernelPath:  filepath.Join(root, "vmlinux"),
		RootFSPath:  filepath.Join(root, "rootfs.img"),
		TapDevice:   "tap-demo",
		GuestMAC:    "02:fc:aa:bb:cc:dd",
		GuestAddr:   "10.0.0.2/30",
		GatewayAddr: "10.0.0.1",
		Nameservers: []string{"1.1.1.1", "8.8.8.8"},
		VCPUCount:   2,
		MemoryMiB:   1024,
		KernelArgs:  "console=ttyS0",
		Bootstrap:   Bootstrap{Version: 1, Hostname: "demo", TailscaleTags: []string{"tag:microvm"}},
	}

	var (
		mu      sync.Mutex
		started []StartRequest
		stopped []StopRequest
	)
	server := NewServerWithHandlers(slog.New(slog.NewTextHandler(io.Discard, nil)), "/usr/bin/firecracker",
		func(_ context.Context, req StartRequest) (StartResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			started = append(started, req)
			return StartResponse{PID: 4321}, nil
		},
		func(_ context.Context, req StopRequest) error {
			mu.Lock()
			defer mu.Unlock()
			stopped = append(stopped, req)
			return nil
		},
	)

	socketPath := filepath.Join(t.TempDir(), "vm-runner.sock")
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

	resp, err := client.StartInstanceVM(ctx, valid)
	if err != nil {
		t.Fatalf("StartInstanceVM(): %v", err)
	}
	if resp.PID != 4321 {
		t.Fatalf("StartInstanceVM() pid = %d, want 4321", resp.PID)
	}
	if err := client.StopInstanceVM(ctx, StopRequest{Name: "demo", PID: 4321}); err != nil {
		t.Fatalf("StopInstanceVM(): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(started, []StartRequest{valid}) {
		t.Fatalf("started = %#v, want %#v", started, []StartRequest{valid})
	}
	if !reflect.DeepEqual(stopped, []StopRequest{{Name: "demo", PID: 4321}}) {
		t.Fatalf("stopped = %#v", stopped)
	}
}

func TestServeUnixSetsSocketPermissions(t *testing.T) {
	server := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), "/usr/bin/firecracker")
	socketPath := filepath.Join(t.TempDir(), "vm-runner.sock")
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

func TestVMContextForRequestIsDetached(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	vmCtx := vmContextForRequest(ctx)
	cancel()

	select {
	case <-vmCtx.Done():
		t.Fatalf("vm context should not be canceled with the request context")
	default:
	}
}

func TestReadUnifiedCgroupPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(path, []byte("12:memory:/system.slice/srv-vm-runner.service\n0::/system.slice/srv-vm-runner.service\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(cgroup): %v", err)
	}

	got, err := readUnifiedCgroupPath(path)
	if err != nil {
		t.Fatalf("readUnifiedCgroupPath(): %v", err)
	}
	if got != "/system.slice/srv-vm-runner.service" {
		t.Fatalf("readUnifiedCgroupPath() = %q, want %q", got, "/system.slice/srv-vm-runner.service")
	}

	missing := filepath.Join(t.TempDir(), "missing-cgroup")
	if err := os.WriteFile(missing, []byte("12:memory:/system.slice/srv-vm-runner.service\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(missing): %v", err)
	}
	if _, err := readUnifiedCgroupPath(missing); err == nil {
		t.Fatalf("readUnifiedCgroupPath() unexpectedly accepted a file without a unified entry")
	}
}

func TestAssignAndCleanupFirecrackerCgroup(t *testing.T) {
	oldRoot := cgroupFSRoot
	oldCurrent := currentCgroupPath
	t.Cleanup(func() {
		cgroupFSRoot = oldRoot
		currentCgroupPath = oldCurrent
	})

	cgroupFSRoot = t.TempDir()
	currentCgroupPath = func() (string, error) {
		return "/system.slice/srv-vm-runner.service", nil
	}

	serviceCgroup := filepath.Join(cgroupFSRoot, "system.slice", "srv-vm-runner.service")
	if err := os.MkdirAll(serviceCgroup, 0o755); err != nil {
		t.Fatalf("MkdirAll(serviceCgroup): %v", err)
	}

	if err := assignFirecrackerToCgroup("demo", 4321); err != nil {
		t.Fatalf("assignFirecrackerToCgroup(): %v", err)
	}

	cgroupPath := filepath.Join(serviceCgroup, "firecracker-vms", "demo")
	payload, err := os.ReadFile(filepath.Join(cgroupPath, "cgroup.procs"))
	if err != nil {
		t.Fatalf("ReadFile(cgroup.procs): %v", err)
	}
	if strings.TrimSpace(string(payload)) != "4321" {
		t.Fatalf("cgroup.procs = %q, want %q", strings.TrimSpace(string(payload)), "4321")
	}

	if err := cleanupFirecrackerCgroup("demo"); err != nil {
		t.Fatalf("cleanupFirecrackerCgroup(): %v", err)
	}
	if _, err := os.Stat(cgroupPath); !os.IsNotExist(err) {
		t.Fatalf("cgroup path should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(serviceCgroup, "firecracker-vms")); !os.IsNotExist(err) {
		t.Fatalf("firecracker cgroup root should be removed when empty, stat err = %v", err)
	}
	if err := assignFirecrackerToCgroup("nested/demo", 1); err == nil {
		t.Fatalf("assignFirecrackerToCgroup() accepted an unsafe name")
	}
}
