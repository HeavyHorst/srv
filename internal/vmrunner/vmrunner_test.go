package vmrunner

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
)

func TestRequestsValidate(t *testing.T) {
	valid := StartRequest{
		Name:        "demo",
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
	valid := StartRequest{
		Name:        "demo",
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
	server := NewServerWithHandlers(slog.New(slog.NewTextHandler(io.Discard, nil)), ServerConfig{
		FirecrackerBinary: "/usr/bin/firecracker",
		InstancesDir:      filepath.Join(t.TempDir(), "instances"),
		KernelPath:        "/var/lib/srv/images/arch-base/vmlinux",
	},
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
	server := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), ServerConfig{
		FirecrackerBinary: "/usr/bin/firecracker",
		InstancesDir:      filepath.Join(t.TempDir(), "instances"),
		KernelPath:        "/var/lib/srv/images/arch-base/vmlinux",
	})
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

func TestStopVMUsesGracefulGuestShutdown(t *testing.T) {
	server := newStopVMTestServer(t)
	pid := startStopVMTestProcess(t)

	oldRequest := requestGuestShutdown
	oldWait := waitForProcessExit
	oldForce := forceStopProcess
	oldKillNow := killProcessNow
	t.Cleanup(func() {
		requestGuestShutdown = oldRequest
		waitForProcessExit = oldWait
		forceStopProcess = oldForce
		killProcessNow = oldKillNow
	})

	var gotSocket string
	var gotPID int
	requestGuestShutdown = func(_ context.Context, socketPath string) error {
		gotSocket = socketPath
		return nil
	}
	waitForProcessExit = func(waitPID int, timeout time.Duration) error {
		gotPID = waitPID
		if timeout != gracefulStopTimeout {
			t.Fatalf("waitForProcessExit timeout = %s, want %s", timeout, gracefulStopTimeout)
		}
		return nil
	}
	forceStopProcess = func(pid int) error {
		t.Fatalf("forceStopProcess(%d) should not be called after a graceful stop", pid)
		return nil
	}
	killProcessNow = func(pid int) error {
		t.Fatalf("killProcessNow(%d) should not be called after a graceful stop", pid)
		return nil
	}

	if err := server.stopVM(context.Background(), StopRequest{Name: "demo", PID: pid}); err != nil {
		t.Fatalf("stopVM(): %v", err)
	}
	if gotSocket != filepath.Join(server.config.InstancesDir, "demo", "firecracker.sock") {
		t.Fatalf("requestGuestShutdown socket = %q", gotSocket)
	}
	if gotPID != pid {
		t.Fatalf("waitForProcessExit pid = %d, want %d", gotPID, pid)
	}
}

func TestStopVMKillsImmediatelyAfterGracefulTimeout(t *testing.T) {
	server := newStopVMTestServer(t)
	pid := startStopVMTestProcess(t)

	oldRequest := requestGuestShutdown
	oldWait := waitForProcessExit
	oldForce := forceStopProcess
	oldKillNow := killProcessNow
	t.Cleanup(func() {
		requestGuestShutdown = oldRequest
		waitForProcessExit = oldWait
		forceStopProcess = oldForce
		killProcessNow = oldKillNow
	})

	requestGuestShutdown = func(_ context.Context, socketPath string) error {
		return nil
	}
	waitForProcessExit = func(waitPID int, timeout time.Duration) error {
		return errProcessExitTimeout
	}
	forceStopProcess = func(pid int) error {
		t.Fatalf("forceStopProcess(%d) should not be used after a graceful shutdown timeout", pid)
		return nil
	}
	var killedPID int
	killProcessNow = func(pid int) error {
		killedPID = pid
		return nil
	}

	if err := server.stopVM(context.Background(), StopRequest{Name: "demo", PID: pid}); err != nil {
		t.Fatalf("stopVM(): %v", err)
	}
	if killedPID != pid {
		t.Fatalf("killProcessNow pid = %d, want %d", killedPID, pid)
	}
}

func TestStopProcessWithGraceWaitsAfterSIGKILL(t *testing.T) {
	pid := startStopVMTestProcess(t)

	oldWait := waitForProcessExit
	t.Cleanup(func() {
		waitForProcessExit = oldWait
	})

	var calls []time.Duration
	waitForProcessExit = func(waitPID int, timeout time.Duration) error {
		if waitPID != pid {
			t.Fatalf("waitForProcessExit pid = %d, want %d", waitPID, pid)
		}
		calls = append(calls, timeout)
		if len(calls) == 1 {
			return errProcessExitTimeout
		}
		if timeout != postKillWaitTimeout {
			t.Fatalf("post-kill wait timeout = %s, want %s", timeout, postKillWaitTimeout)
		}
		return nil
	}

	if err := stopProcessWithGrace(pid, time.Second); err != nil {
		t.Fatalf("stopProcessWithGrace(): %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("waitForProcessExit calls = %d, want 2", len(calls))
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

func TestDetectJailerCgroupVersion(t *testing.T) {
	oldCurrent := currentCgroupPath
	t.Cleanup(func() {
		currentCgroupPath = oldCurrent
	})

	currentCgroupPath = func() (string, error) {
		return "/system.slice/srv-vm-runner.service", nil
	}
	if got := detectJailerCgroupVersion(); got != "2" {
		t.Fatalf("detectJailerCgroupVersion() = %q, want %q", got, "2")
	}

	currentCgroupPath = func() (string, error) {
		return "", errors.New("no unified hierarchy")
	}
	if got := detectJailerCgroupVersion(); got != "1" {
		t.Fatalf("detectJailerCgroupVersion() = %q, want %q", got, "1")
	}
}

func TestDisabledJailerNumaNodeOmitsCpusetCgroups(t *testing.T) {
	args := firecracker.NewJailerCommandBuilder().
		WithID("demo").
		WithUID(123).
		WithGID(456).
		WithNumaNode(disabledJailerNumaNode).
		WithExecFile("/usr/bin/firecracker").
		WithCgroupVersion("2").
		Args()

	want := []string{
		"--id", "demo",
		"--uid", "123",
		"--gid", "456",
		"--exec-file", "/usr/bin/firecracker",
		"--cgroup-version", "2",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("JailerCommandBuilder.Args() = %#v, want %#v", args, want)
	}
}

func TestValidateJailedFirecrackerBinary(t *testing.T) {
	t.Run("accepts static elf without interpreter", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "firecracker-static")
		if err := writeTestELF(path, ""); err != nil {
			t.Fatalf("writeTestELF(static): %v", err)
		}
		if err := validateJailedFirecrackerBinary(path); err != nil {
			t.Fatalf("validateJailedFirecrackerBinary(static): %v", err)
		}
	})

	t.Run("rejects dynamic elf with interpreter", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "firecracker-dynamic")
		const interp = "/lib64/ld-linux-x86-64.so.2"
		if err := writeTestELF(path, interp); err != nil {
			t.Fatalf("writeTestELF(dynamic): %v", err)
		}
		err := validateJailedFirecrackerBinary(path)
		if err == nil {
			t.Fatal("validateJailedFirecrackerBinary(dynamic) unexpectedly succeeded")
		}
		if !strings.Contains(err.Error(), interp) {
			t.Fatalf("validateJailedFirecrackerBinary(dynamic) error = %q, want interpreter %q", err, interp)
		}
	})
}

func TestAssignAndCleanupFirecrackerCgroup(t *testing.T) {
	oldRoot := cgroupFSRoot
	oldCurrent := currentCgroupPath
	oldRemove := removePath
	t.Cleanup(func() {
		cgroupFSRoot = oldRoot
		currentCgroupPath = oldCurrent
		removePath = oldRemove
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

func TestCleanupFirecrackerCgroupIgnoresBusySharedRoot(t *testing.T) {
	oldRoot := cgroupFSRoot
	oldCurrent := currentCgroupPath
	oldRemove := removePath
	t.Cleanup(func() {
		cgroupFSRoot = oldRoot
		currentCgroupPath = oldCurrent
		removePath = oldRemove
	})

	cgroupFSRoot = t.TempDir()
	currentCgroupPath = func() (string, error) {
		return "/system.slice/srv-vm-runner.service", nil
	}

	serviceCgroup := filepath.Join(cgroupFSRoot, "system.slice", "srv-vm-runner.service")
	if err := os.MkdirAll(filepath.Join(serviceCgroup, "firecracker-vms", "demo"), 0o755); err != nil {
		t.Fatalf("MkdirAll(demo): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(serviceCgroup, "firecracker-vms", "other"), 0o755); err != nil {
		t.Fatalf("MkdirAll(other): %v", err)
	}

	vmRoot := filepath.Join(serviceCgroup, "firecracker-vms")
	removePath = func(path string) error {
		if path == vmRoot {
			return syscall.EBUSY
		}
		return os.Remove(path)
	}

	if err := cleanupFirecrackerCgroup("demo"); err != nil {
		t.Fatalf("cleanupFirecrackerCgroup(): %v", err)
	}
	if _, err := os.Stat(filepath.Join(vmRoot, "demo")); !os.IsNotExist(err) {
		t.Fatalf("demo cgroup should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(vmRoot, "other")); err != nil {
		t.Fatalf("other cgroup should remain, stat err = %v", err)
	}
}

func TestServerConfigValidate(t *testing.T) {
	valid := ServerConfig{
		FirecrackerBinary: "/usr/bin/firecracker",
		JailerBinary:      "/usr/bin/jailer",
		JailerBaseDir:     "/var/lib/srv/jailer",
		JailerUID:         1001,
		JailerGID:         1002,
		InstancesDir:      "/var/lib/srv/instances",
		KernelPath:        "/var/lib/srv/images/arch-base/vmlinux",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("ServerConfig.Validate(): %v", err)
	}
	for _, tc := range []struct {
		name string
		cfg  ServerConfig
	}{
		{name: "missing jailer base dir", cfg: ServerConfig{FirecrackerBinary: valid.FirecrackerBinary, JailerBinary: valid.JailerBinary, InstancesDir: valid.InstancesDir, KernelPath: valid.KernelPath, JailerUID: valid.JailerUID, JailerGID: valid.JailerGID}},
		{name: "missing instances dir", cfg: ServerConfig{FirecrackerBinary: valid.FirecrackerBinary, JailerBinary: valid.JailerBinary, JailerBaseDir: valid.JailerBaseDir, KernelPath: valid.KernelPath, JailerUID: valid.JailerUID, JailerGID: valid.JailerGID}},
		{name: "relative jailer path", cfg: ServerConfig{FirecrackerBinary: valid.FirecrackerBinary, JailerBinary: "bin/jailer", JailerBaseDir: valid.JailerBaseDir, InstancesDir: valid.InstancesDir, KernelPath: valid.KernelPath, JailerUID: valid.JailerUID, JailerGID: valid.JailerGID}},
		{name: "relative kernel path", cfg: ServerConfig{FirecrackerBinary: valid.FirecrackerBinary, JailerBinary: valid.JailerBinary, JailerBaseDir: valid.JailerBaseDir, InstancesDir: valid.InstancesDir, KernelPath: "images/vmlinux", JailerUID: valid.JailerUID, JailerGID: valid.JailerGID}},
		{name: "relative initrd path", cfg: ServerConfig{FirecrackerBinary: valid.FirecrackerBinary, JailerBinary: valid.JailerBinary, JailerBaseDir: valid.JailerBaseDir, InstancesDir: valid.InstancesDir, KernelPath: valid.KernelPath, InitrdPath: "images/initrd", JailerUID: valid.JailerUID, JailerGID: valid.JailerGID}},
		{name: "negative jailer uid", cfg: ServerConfig{FirecrackerBinary: valid.FirecrackerBinary, JailerBinary: valid.JailerBinary, JailerBaseDir: valid.JailerBaseDir, InstancesDir: valid.InstancesDir, KernelPath: valid.KernelPath, JailerUID: -1, JailerGID: valid.JailerGID}},
	} {
		if err := tc.cfg.Validate(); err == nil {
			t.Fatalf("%s unexpectedly passed validation", tc.name)
		}
	}
}

func TestResolveInstanceRuntimePaths(t *testing.T) {
	got, err := resolveInstanceRuntimePaths("/var/lib/srv/instances", "demo")
	if err != nil {
		t.Fatalf("resolveInstanceRuntimePaths(): %v", err)
	}
	want := instanceRuntimePaths{
		SocketPath: "/var/lib/srv/instances/demo/firecracker.sock",
		LogPath:    "/var/lib/srv/instances/demo/firecracker.log",
		SerialLog:  "/var/lib/srv/instances/demo/serial.log",
		RootFSPath: "/var/lib/srv/instances/demo/rootfs.img",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveInstanceRuntimePaths() = %#v, want %#v", got, want)
	}
	if _, err := resolveInstanceRuntimePaths("/var/lib/srv/instances", "nested/demo"); err == nil {
		t.Fatalf("resolveInstanceRuntimePaths() accepted an unsafe name")
	}
}

func TestResolveJailerRuntimePaths(t *testing.T) {
	got, err := resolveJailerRuntimePaths("/var/lib/srv/jailer", "/usr/bin/firecracker", "demo")
	if err != nil {
		t.Fatalf("resolveJailerRuntimePaths(): %v", err)
	}
	want := jailerRuntimePaths{
		WorkspaceDir: "/var/lib/srv/jailer/firecracker/demo",
		RootDir:      "/var/lib/srv/jailer/firecracker/demo/root",
		SocketPath:   "/var/lib/srv/jailer/firecracker/demo/root/firecracker.sock",
		LogPath:      "/var/lib/srv/jailer/firecracker/demo/root/firecracker.log",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveJailerRuntimePaths() = %#v, want %#v", got, want)
	}
	if _, err := resolveJailerRuntimePaths("/var/lib/srv/jailer", "/usr/bin/firecracker", "nested/demo"); err == nil {
		t.Fatalf("resolveJailerRuntimePaths() accepted an unsafe name")
	}
}

func writeTestELF(path, interp string) error {
	var (
		phoff uint64
		phnum uint16
		data  []byte
	)
	if interp != "" {
		phoff = 64
		phnum = 1
		data = append([]byte(interp), 0)
	}

	buf := bytes.NewBuffer(make([]byte, 0, 64+56+len(data)))
	ident := [16]byte{0x7f, 'E', 'L', 'F', 2, 1, 1}
	if _, err := buf.Write(ident[:]); err != nil {
		return err
	}
	for _, value := range []any{
		uint16(2),
		uint16(62),
		uint32(1),
		uint64(0),
		phoff,
		uint64(0),
		uint32(0),
		uint16(64),
		uint16(56),
		phnum,
		uint16(0),
		uint16(0),
		uint16(0),
	} {
		if err := binary.Write(buf, binary.LittleEndian, value); err != nil {
			return err
		}
	}
	if interp != "" {
		for _, value := range []any{
			uint32(3),
			uint32(0),
			uint64(64 + 56),
			uint64(0),
			uint64(0),
			uint64(len(data)),
			uint64(len(data)),
			uint64(1),
		} {
			if err := binary.Write(buf, binary.LittleEndian, value); err != nil {
				return err
			}
		}
		if _, err := buf.Write(data); err != nil {
			return err
		}
	}

	return os.WriteFile(path, buf.Bytes(), 0o755)
}

func newStopVMTestServer(t *testing.T) *Server {
	t.Helper()

	oldRoot := cgroupFSRoot
	oldCurrent := currentCgroupPath
	t.Cleanup(func() {
		cgroupFSRoot = oldRoot
		currentCgroupPath = oldCurrent
	})

	instancesDir := filepath.Join(t.TempDir(), "instances")
	if err := os.MkdirAll(filepath.Join(instancesDir, "demo"), 0o755); err != nil {
		t.Fatalf("MkdirAll(instance dir): %v", err)
	}
	cgroupFSRoot = t.TempDir()
	currentCgroupPath = func() (string, error) {
		return "/system.slice/srv-vm-runner.service", nil
	}
	if err := os.MkdirAll(filepath.Join(cgroupFSRoot, "system.slice", "srv-vm-runner.service", "firecracker-vms", "demo"), 0o755); err != nil {
		t.Fatalf("MkdirAll(cgroup): %v", err)
	}

	return NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), ServerConfig{
		FirecrackerBinary: "/usr/bin/firecracker",
		JailerBinary:      "/usr/bin/jailer",
		JailerBaseDir:     filepath.Join(t.TempDir(), "jailer"),
		InstancesDir:      instancesDir,
		KernelPath:        "/var/lib/srv/images/arch-base/vmlinux",
	})
}

func startStopVMTestProcess(t *testing.T) int {
	t.Helper()

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start(sleep): %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process == nil {
			return
		}
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process.Pid
}
