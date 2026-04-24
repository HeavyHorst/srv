package vmrunner

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
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
	if err := (MetricsRequest{Name: "demo"}).Validate(); err != nil {
		t.Fatalf("MetricsRequest.Validate(): %v", err)
	}
	if err := (MemoryPoolRequest{Name: "pool-a"}).Validate(); err != nil {
		t.Fatalf("MemoryPoolRequest.Validate(): %v", err)
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "bad name", err: (StartRequest{Name: "nested/demo"}).Validate()},
		{name: "bad tap", err: func() error { req := valid; req.TapDevice = "nested/demo"; return req.Validate() }()},
		{name: "bad guest ip", err: func() error { req := valid; req.GuestAddr = "10.0.0.2"; return req.Validate() }()},
		{name: "bad stop pid", err: (StopRequest{Name: "demo", PID: -1}).Validate()},
		{name: "bad metrics name", err: (MetricsRequest{Name: "nested/demo"}).Validate()},
		{name: "bad memory pool name", err: (MemoryPoolRequest{Name: "nested/pool"}).Validate()},
	} {
		if tc.err == nil {
			t.Fatalf("%s unexpectedly passed validation", tc.name)
		}
	}
}

func TestNewRootDriveUsesWritebackCache(t *testing.T) {
	path := "/var/lib/srv/instances/demo/rootfs.img"
	drive := newRootDrive(path)

	if drive.CacheType == nil || *drive.CacheType != models.DriveCacheTypeWriteback {
		t.Fatalf("newRootDrive() cache type = %v, want %q", drive.CacheType, models.DriveCacheTypeWriteback)
	}
	if drive.DriveID == nil || *drive.DriveID != "rootfs" {
		t.Fatalf("newRootDrive() drive ID = %v, want %q", drive.DriveID, "rootfs")
	}
	if drive.PathOnHost == nil || *drive.PathOnHost != path {
		t.Fatalf("newRootDrive() path = %v, want %q", drive.PathOnHost, path)
	}
	if drive.IsReadOnly == nil || *drive.IsReadOnly {
		t.Fatalf("newRootDrive() IsReadOnly = %v, want false", drive.IsReadOnly)
	}
	if drive.IsRootDevice == nil || !*drive.IsRootDevice {
		t.Fatalf("newRootDrive() IsRootDevice = %v, want true", drive.IsRootDevice)
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
		pools   []MemoryPoolRequest
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
	server.metrics = func(_ context.Context, req MetricsRequest) (MetricsResponse, error) {
		if req.Name != "demo" {
			t.Fatalf("metrics request name = %q, want demo", req.Name)
		}
		return MetricsResponse{CPUUsageUsec: 12345, MemoryCurrentBytes: 256 << 20, MemoryLimitBytes: 1024 << 20}, nil
	}
	server.deleteMemoryPool = func(_ context.Context, req MemoryPoolRequest) error {
		mu.Lock()
		defer mu.Unlock()
		pools = append(pools, req)
		return nil
	}

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
	metrics, err := client.ReadInstanceMetrics(ctx, MetricsRequest{Name: "demo"})
	if err != nil {
		t.Fatalf("ReadInstanceMetrics(): %v", err)
	}
	if err := client.DeleteMemoryPool(ctx, MemoryPoolRequest{Name: "pool-a"}); err != nil {
		t.Fatalf("DeleteMemoryPool(): %v", err)
	}
	if metrics.CPUUsageUsec != 12345 || metrics.MemoryCurrentBytes != 256<<20 || metrics.MemoryLimitBytes != 1024<<20 {
		t.Fatalf("ReadInstanceMetrics() = %#v", metrics)
	}

	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(started, []StartRequest{valid}) {
		t.Fatalf("started = %#v, want %#v", started, []StartRequest{valid})
	}
	if !reflect.DeepEqual(stopped, []StopRequest{{Name: "demo", PID: 4321}}) {
		t.Fatalf("stopped = %#v", stopped)
	}
	if !reflect.DeepEqual(pools, []MemoryPoolRequest{{Name: "pool-a"}}) {
		t.Fatalf("pools = %#v", pools)
	}
}

func TestCreatePooledBalloonDeviceEnablesFreePageReporting(t *testing.T) {
	server := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), ServerConfig{})
	socketPath := filepath.Join(t.TempDir(), "firecracker.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("net.Listen(unix): %v", err)
	}
	defer listener.Close()

	reqCh := make(chan *http.Request, 1)
	bodyCh := make(chan []byte, 1)
	httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll(request body): %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		reqCh <- r
		bodyCh <- body
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() {
		_ = httpServer.Serve(listener)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	})

	if err := server.createPooledBalloonDevice(context.Background(), socketPath); err != nil {
		t.Fatalf("createPooledBalloonDevice(): %v", err)
	}

	var payload struct {
		AmountMiB                   int64 `json:"amount_mib"`
		DeflateOnOOM                bool  `json:"deflate_on_oom"`
		StatsPollingIntervalSeconds int64 `json:"stats_polling_interval_s"`
		FreePageReporting           bool  `json:"free_page_reporting"`
	}
	if err := json.Unmarshal(<-bodyCh, &payload); err != nil {
		t.Fatalf("json.Unmarshal(request body): %v", err)
	}
	req := <-reqCh
	if req.Method != http.MethodPut {
		t.Fatalf("request method = %q, want %q", req.Method, http.MethodPut)
	}
	if req.URL.Path != "/balloon" {
		t.Fatalf("request path = %q, want %q", req.URL.Path, "/balloon")
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if payload.AmountMiB != 0 {
		t.Fatalf("amount_mib = %d, want 0", payload.AmountMiB)
	}
	if !payload.DeflateOnOOM {
		t.Fatalf("deflate_on_oom = false, want true")
	}
	if payload.StatsPollingIntervalSeconds != pooledBalloonStatsIntervalSeconds {
		t.Fatalf("stats_polling_interval_s = %d, want %d", payload.StatsPollingIntervalSeconds, pooledBalloonStatsIntervalSeconds)
	}
	if !payload.FreePageReporting {
		t.Fatalf("free_page_reporting = false, want true")
	}
}

func TestDesiredPooledBalloonTargetMiB(t *testing.T) {
	tests := []struct {
		name          string
		residentBytes int64
		stats         firecrackerBalloonStats
		wantTargetMiB int64
		wantStepMiB   int64
		wantOK        bool
	}{
		{
			name:          "reclaims conservative slack",
			residentBytes: 640 * miBBytes,
			stats: firecrackerBalloonStats{
				TargetMiB:       int64Ptr(0),
				AvailableMemory: 640 * miBBytes,
				TotalMemory:     1024 * miBBytes,
			},
			wantTargetMiB: 384,
			wantStepMiB:   64,
			wantOK:        true,
		},
		{
			name:          "keeps target at zero when resident set is already low",
			residentBytes: 300 * miBBytes,
			stats: firecrackerBalloonStats{
				TargetMiB:       int64Ptr(128),
				AvailableMemory: 700 * miBBytes,
				TotalMemory:     1024 * miBBytes,
			},
			wantTargetMiB: 0,
			wantStepMiB:   64,
			wantOK:        true,
		},
		{
			name:          "falls back to free memory plus disk caches",
			residentBytes: 768 * miBBytes,
			stats: firecrackerBalloonStats{
				TargetMiB:   int64Ptr(0),
				FreeMemory:  256 * miBBytes,
				DiskCaches:  384 * miBBytes,
				TotalMemory: 1024 * miBBytes,
			},
			wantTargetMiB: 384,
			wantStepMiB:   64,
			wantOK:        true,
		},
		{
			name:          "skips when total guest memory is unavailable",
			residentBytes: 768 * miBBytes,
			stats: firecrackerBalloonStats{
				TargetMiB: int64Ptr(0),
			},
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotTargetMiB, gotStepMiB, gotOK := desiredPooledBalloonTargetMiB(tc.residentBytes, tc.stats)
			if gotOK != tc.wantOK {
				t.Fatalf("desiredPooledBalloonTargetMiB() ok = %v, want %v", gotOK, tc.wantOK)
			}
			if !gotOK {
				return
			}
			if gotTargetMiB != tc.wantTargetMiB {
				t.Fatalf("desiredPooledBalloonTargetMiB() target = %d, want %d", gotTargetMiB, tc.wantTargetMiB)
			}
			if gotStepMiB != tc.wantStepMiB {
				t.Fatalf("desiredPooledBalloonTargetMiB() step = %d, want %d", gotStepMiB, tc.wantStepMiB)
			}
		})
	}
}

func TestReconcilePooledBalloonUpdatesTarget(t *testing.T) {
	server := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), ServerConfig{
		InstancesDir: filepath.Join(t.TempDir(), "instances"),
	})
	cgroupPath := filepath.Join(t.TempDir(), "cgroup", "pool-a", "demo")
	if err := os.MkdirAll(cgroupPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(cgroupPath): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cgroupPath, "memory.current"), []byte("671088640\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(memory.current): %v", err)
	}

	socketPath := filepath.Join(t.TempDir(), "firecracker.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("net.Listen(unix): %v", err)
	}
	defer listener.Close()

	var patchedAmountMiB int64 = -1
	httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/balloon/statistics":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"target_mib":0,"actual_mib":0,"available_memory":671088640,"total_memory":1073741824}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/balloon":
			var payload struct {
				AmountMiB int64 `json:"amount_mib"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("Decode(patch body): %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			patchedAmountMiB = payload.AmountMiB
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go func() {
		_ = httpServer.Serve(listener)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	})

	if err := server.reconcilePooledBalloon(context.Background(), pooledBalloonVM{
		Name:       "demo",
		CgroupPath: cgroupPath,
		SocketPath: socketPath,
	}); err != nil {
		t.Fatalf("reconcilePooledBalloon(): %v", err)
	}
	if patchedAmountMiB != 384 {
		t.Fatalf("patched amount_mib = %d, want 384", patchedAmountMiB)
	}
}

func TestReconcilePooledBalloonCorrectsUnalignedTarget(t *testing.T) {
	server := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), ServerConfig{
		InstancesDir: filepath.Join(t.TempDir(), "instances"),
	})
	cgroupPath := filepath.Join(t.TempDir(), "cgroup", "pool-a", "demo")
	if err := os.MkdirAll(cgroupPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(cgroupPath): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cgroupPath, "memory.current"), []byte("134217728\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(memory.current): %v", err)
	}

	socketPath := filepath.Join(t.TempDir(), "firecracker.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("net.Listen(unix): %v", err)
	}
	defer listener.Close()
	t.Cleanup(func() { server.closeFirecrackerClient(socketPath) })

	var patchedAmountMiB int64 = -1
	httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/balloon/statistics":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"target_mib":32,"actual_mib":32,"available_memory":134217728,"total_memory":1073741824}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/balloon":
			var payload struct {
				AmountMiB int64 `json:"amount_mib"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("Decode(patch body): %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			patchedAmountMiB = payload.AmountMiB
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go func() {
		_ = httpServer.Serve(listener)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	})

	if err := server.reconcilePooledBalloon(context.Background(), pooledBalloonVM{
		Name:       "demo",
		CgroupPath: cgroupPath,
		SocketPath: socketPath,
	}); err != nil {
		t.Fatalf("reconcilePooledBalloon(): %v", err)
	}
	if patchedAmountMiB != 0 {
		t.Fatalf("patched amount_mib = %d, want 0", patchedAmountMiB)
	}
}

func TestReconcilePooledBalloonsDoesNotBlockPeers(t *testing.T) {
	oldRoot := cgroupFSRoot
	oldCurrent := currentCgroupPath
	t.Cleanup(func() {
		cgroupFSRoot = oldRoot
		currentCgroupPath = oldCurrent
	})

	cgroupFSRoot = t.TempDir()
	serviceRel := "/system.slice/srv-vm-runner.service"
	currentCgroupPath = func() (string, error) {
		return serviceRel, nil
	}

	instancesDir := filepath.Join(t.TempDir(), "instances")
	server := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), ServerConfig{
		InstancesDir: instancesDir,
	})

	for _, name := range []string{"a-slow", "z-fast"} {
		cgroupPath := filepath.Join(cgroupFSRoot, "system.slice", "srv-vm-runner.service", firecrackerPoolRootCgroupName, "pool-a", name)
		if err := os.MkdirAll(cgroupPath, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", cgroupPath, err)
		}
		if err := os.WriteFile(filepath.Join(cgroupPath, "memory.current"), []byte("671088640\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(memory.current for %s): %v", name, err)
		}
		if err := os.MkdirAll(filepath.Join(instancesDir, name), 0o755); err != nil {
			t.Fatalf("MkdirAll(instance %s): %v", name, err)
		}
	}

	fastPatched := make(chan int64, 1)
	serveSocket := func(name string, handler http.HandlerFunc) {
		socketPath := filepath.Join(instancesDir, name, "firecracker.sock")
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Fatalf("net.Listen(%s): %v", socketPath, err)
		}
		t.Cleanup(func() {
			server.closeFirecrackerClient(socketPath)
			_ = listener.Close()
		})
		httpServer := &http.Server{Handler: handler}
		go func() {
			_ = httpServer.Serve(listener)
		}()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = httpServer.Shutdown(ctx)
		})
	}

	serveSocket("a-slow", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/balloon/statistics" {
			time.Sleep(400 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"target_mib":0,"actual_mib":0,"available_memory":671088640,"total_memory":1073741824}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	serveSocket("z-fast", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/balloon/statistics":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"target_mib":0,"actual_mib":0,"available_memory":671088640,"total_memory":1073741824}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/balloon":
			var payload struct {
				AmountMiB int64 `json:"amount_mib"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("Decode(fast patch body): %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fastPatched <- payload.AmountMiB
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	server.reconcilePooledBalloons(ctx)

	select {
	case got := <-fastPatched:
		if got != 384 {
			t.Fatalf("fast patch amount_mib = %d, want 384", got)
		}
	default:
		t.Fatal("fast pooled VM was not reconciled before the slow peer timed out")
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

func TestBuildJailedVMCommandIncludesResourceLimits(t *testing.T) {
	server := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), ServerConfig{
		FirecrackerBinary: "/usr/bin/firecracker",
		JailerBinary:      "/usr/bin/jailer",
		JailerBaseDir:     "/var/lib/srv/jailer",
		JailerUID:         123,
		JailerGID:         456,
		InstancesDir:      "/var/lib/srv/instances",
		KernelPath:        "/var/lib/srv/images/arch-base/vmlinux",
		VMPIDsMax:         321,
	})

	cmd, err := server.buildJailedVMCommand(
		context.Background(),
		StartRequest{Name: "demo", VCPUCount: 2, MemoryMiB: 1024},
		"firecracker.sock",
		"system.slice/srv-vm-runner.service/firecracker-vms",
		io.Discard,
	)
	if err != nil {
		t.Fatalf("buildJailedVMCommand(): %v", err)
	}

	want := []string{
		"/usr/bin/jailer",
		"--id", "demo",
		"--uid", "123",
		"--gid", "456",
		"--exec-file", "/usr/bin/firecracker",
		"--cgroup-version", "2",
		"--chroot-base-dir", "/var/lib/srv/jailer",
		"--parent-cgroup", "system.slice/srv-vm-runner.service/firecracker-vms",
		"--cgroup", "cpu.max=200000 100000",
		"--cgroup", "memory.max=1073741824",
		"--cgroup", "memory.swap.max=0",
		"--cgroup", "pids.max=321",
		"--",
		"--no-seccomp",
		"--api-sock", "firecracker.sock",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("buildJailedVMCommand() args = %#v, want %#v", cmd.Args, want)
	}
}

func TestPrepareFirecrackerCgroupParentMovesRunnerAndEnablesControllers(t *testing.T) {
	oldRoot := cgroupFSRoot
	oldCurrent := currentCgroupPath
	oldCreateDirAll := createDirAll
	t.Cleanup(func() {
		cgroupFSRoot = oldRoot
		currentCgroupPath = oldCurrent
		createDirAll = oldCreateDirAll
	})

	cgroupFSRoot = t.TempDir()
	serviceRel := "/system.slice/srv-vm-runner.service"
	servicePath := filepath.Join(cgroupFSRoot, "system.slice", "srv-vm-runner.service")
	seedCgroup := func(path string) error {
		if err := os.WriteFile(filepath.Join(path, "cgroup.controllers"), []byte("cpu memory pids"), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(path, "cgroup.subtree_control"), nil, 0o644); err != nil {
			return err
		}
		return nil
	}
	createDirAll = func(path string, mode os.FileMode) error {
		if err := os.MkdirAll(path, mode); err != nil {
			return err
		}
		if strings.HasPrefix(path, cgroupFSRoot) {
			if err := seedCgroup(path); err != nil {
				return err
			}
		}
		return nil
	}
	if err := createDirAll(servicePath, 0o755); err != nil {
		t.Fatalf("createDirAll(servicePath): %v", err)
	}
	currentCgroupPath = func() (string, error) {
		return serviceRel, nil
	}

	server := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), ServerConfig{
		FirecrackerBinary: "/usr/bin/firecracker",
		JailerBinary:      "/usr/bin/jailer",
		JailerBaseDir:     "/var/lib/srv/jailer",
		JailerUID:         123,
		JailerGID:         456,
		InstancesDir:      "/var/lib/srv/instances",
		KernelPath:        "/var/lib/srv/images/arch-base/vmlinux",
	})

	parent, err := server.prepareFirecrackerCgroupParent()
	if err != nil {
		t.Fatalf("prepareFirecrackerCgroupParent(): %v", err)
	}
	if parent != "system.slice/srv-vm-runner.service/firecracker-vms" {
		t.Fatalf("prepareFirecrackerCgroupParent() = %q", parent)
	}

	supervisorProcs, err := os.ReadFile(filepath.Join(servicePath, firecrackerSupervisorCgroupName, "cgroup.procs"))
	if err != nil {
		t.Fatalf("ReadFile(supervisor cgroup.procs): %v", err)
	}
	if strings.TrimSpace(string(supervisorProcs)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("supervisor cgroup.procs = %q, want %d", strings.TrimSpace(string(supervisorProcs)), os.Getpid())
	}

	for _, path := range []string{
		filepath.Join(servicePath, "cgroup.subtree_control"),
		filepath.Join(servicePath, firecrackerVMRootCgroupName, "cgroup.subtree_control"),
	} {
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		if got := strings.Fields(string(payload)); !reflect.DeepEqual(got, []string{"+cpu", "+memory", "+pids"}) {
			t.Fatalf("%s = %#v, want [+cpu +memory +pids]", path, got)
		}
	}
}

func TestReadInstanceMetricsReadsVMResourcesFromCgroup(t *testing.T) {
	oldRoot := cgroupFSRoot
	oldCurrent := currentCgroupPath
	oldReadTextFile := readTextFile
	t.Cleanup(func() {
		cgroupFSRoot = oldRoot
		currentCgroupPath = oldCurrent
		readTextFile = oldReadTextFile
	})

	cgroupFSRoot = t.TempDir()
	currentCgroupPath = func() (string, error) {
		return "/system.slice/srv-vm-runner.service", nil
	}
	server := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), ServerConfig{
		FirecrackerBinary: "/usr/bin/firecracker",
		InstancesDir:      "/var/lib/srv/instances",
		KernelPath:        "/var/lib/srv/images/arch-base/vmlinux",
	})
	cgroupPath := filepath.Join(cgroupFSRoot, "system.slice", "srv-vm-runner.service", firecrackerVMRootCgroupName, "demo")
	if err := os.MkdirAll(cgroupPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(cgroupPath): %v", err)
	}
	for path, payload := range map[string]string{
		filepath.Join(cgroupPath, "cpu.stat"):       "usage_usec 12345\nuser_usec 10000\nsystem_usec 2345\n",
		filepath.Join(cgroupPath, "memory.current"): "268435456\n",
		filepath.Join(cgroupPath, "memory.max"):     "1073741824\n",
	} {
		if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}

	metrics, err := server.readInstanceMetrics(context.Background(), MetricsRequest{Name: "demo"})
	if err != nil {
		t.Fatalf("readInstanceMetrics(): %v", err)
	}
	if metrics.CPUUsageUsec != 12345 || metrics.MemoryCurrentBytes != 256<<20 || metrics.MemoryLimitBytes != 1024<<20 {
		t.Fatalf("readInstanceMetrics() = %#v", metrics)
	}
}

func TestHandleMetricsReturnsNotFoundForMissingCgroup(t *testing.T) {
	server := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), ServerConfig{
		FirecrackerBinary: "/usr/bin/firecracker",
		InstancesDir:      "/var/lib/srv/instances",
		KernelPath:        "/var/lib/srv/images/arch-base/vmlinux",
	})
	server.metrics = func(context.Context, MetricsRequest) (MetricsResponse, error) {
		return MetricsResponse{}, os.ErrNotExist
	}

	req := httptest.NewRequest(http.MethodPost, "/vm/metrics", strings.NewReader(`{"name":"demo"}`))
	w := httptest.NewRecorder()
	server.handleMetrics(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("handleMetrics() status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestReadInt64FileAcceptsMax(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.max")
	if err := os.WriteFile(path, []byte("max\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(memory.max): %v", err)
	}
	got, err := readInt64File(path)
	if err != nil {
		t.Fatalf("readInt64File(): %v", err)
	}
	if got != 0 {
		t.Fatalf("readInt64File(max) = %d, want 0", got)
	}
}

func int64Ptr(value int64) *int64 {
	return &value
}

func TestReadInstanceMetricsFindsPooledCgroupWithoutRequestMetadata(t *testing.T) {
	oldRoot := cgroupFSRoot
	oldCurrent := currentCgroupPath
	t.Cleanup(func() {
		cgroupFSRoot = oldRoot
		currentCgroupPath = oldCurrent
	})

	cgroupFSRoot = t.TempDir()
	serviceRel := "/system.slice/srv-vm-runner.service"
	currentCgroupPath = func() (string, error) {
		return serviceRel, nil
	}

	pooledPath := filepath.Join(cgroupFSRoot, "system.slice", "srv-vm-runner.service", firecrackerPoolRootCgroupName, "pool-a", "demo")
	if err := os.MkdirAll(pooledPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(pooledPath): %v", err)
	}
	for path, payload := range map[string]string{
		filepath.Join(pooledPath, "cpu.stat"):       "usage_usec 54321\nuser_usec 50000\nsystem_usec 4321\n",
		filepath.Join(pooledPath, "memory.current"): "134217728\n",
		filepath.Join(pooledPath, "memory.max"):     "1073741824\n",
	} {
		if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}

	server := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), ServerConfig{
		FirecrackerBinary: "/usr/bin/firecracker",
		JailerBinary:      "/usr/bin/jailer",
		JailerBaseDir:     "/var/lib/srv/jailer",
		JailerUID:         123,
		JailerGID:         456,
		InstancesDir:      "/var/lib/srv/instances",
		KernelPath:        "/var/lib/srv/images/arch-base/vmlinux",
	})

	metrics, err := server.readInstanceMetrics(context.Background(), MetricsRequest{Name: "demo"})
	if err != nil {
		t.Fatalf("readInstanceMetrics(): %v", err)
	}
	if metrics.CPUUsageUsec != 54321 || metrics.MemoryCurrentBytes != 128<<20 || metrics.MemoryLimitBytes != 1024<<20 {
		t.Fatalf("readInstanceMetrics() = %#v", metrics)
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

func TestServerCleanupFirecrackerCgroupPropagatesBusyLeaf(t *testing.T) {
	oldRoot := cgroupFSRoot
	oldCurrent := currentCgroupPath
	oldRemove := removePath
	t.Cleanup(func() {
		cgroupFSRoot = oldRoot
		currentCgroupPath = oldCurrent
		removePath = oldRemove
	})

	cgroupFSRoot = t.TempDir()
	serviceRel := "/system.slice/srv-vm-runner.service"
	currentCgroupPath = func() (string, error) {
		return serviceRel, nil
	}

	serviceCgroup := filepath.Join(cgroupFSRoot, "system.slice", "srv-vm-runner.service")
	cgroupPath := filepath.Join(serviceCgroup, firecrackerVMRootCgroupName, "demo")
	if err := os.MkdirAll(cgroupPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(demo): %v", err)
	}

	server := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), ServerConfig{
		FirecrackerBinary: "/usr/bin/firecracker",
		JailerBinary:      "/usr/bin/jailer",
		JailerBaseDir:     "/var/lib/srv/jailer",
		JailerUID:         123,
		JailerGID:         456,
		InstancesDir:      "/var/lib/srv/instances",
		KernelPath:        "/var/lib/srv/images/arch-base/vmlinux",
	})

	var removed []string
	removePath = func(path string) error {
		removed = append(removed, path)
		if path == cgroupPath {
			return syscall.ENOTEMPTY
		}
		return os.Remove(path)
	}

	err := server.cleanupFirecrackerCgroup("demo")
	if err == nil || !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("cleanupFirecrackerCgroup() error = %v, want ENOTEMPTY", err)
	}
	if len(removed) != 1 || removed[0] != cgroupPath {
		t.Fatalf("cleanupFirecrackerCgroup() removed %#v, want only %q", removed, cgroupPath)
	}
}

func TestServerCleanupFirecrackerCgroupKeepsPooledParent(t *testing.T) {
	oldRoot := cgroupFSRoot
	oldCurrent := currentCgroupPath
	oldRemove := removePath
	t.Cleanup(func() {
		cgroupFSRoot = oldRoot
		currentCgroupPath = oldCurrent
		removePath = oldRemove
	})

	cgroupFSRoot = t.TempDir()
	serviceRel := "/system.slice/srv-vm-runner.service"
	currentCgroupPath = func() (string, error) {
		return serviceRel, nil
	}

	serviceCgroup := filepath.Join(cgroupFSRoot, "system.slice", "srv-vm-runner.service")
	poolPath := filepath.Join(serviceCgroup, firecrackerPoolRootCgroupName, "pool-a")
	cgroupPath := filepath.Join(poolPath, "demo")
	if err := os.MkdirAll(cgroupPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(demo): %v", err)
	}

	server := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), ServerConfig{
		FirecrackerBinary: "/usr/bin/firecracker",
		JailerBinary:      "/usr/bin/jailer",
		JailerBaseDir:     "/var/lib/srv/jailer",
		JailerUID:         123,
		JailerGID:         456,
		InstancesDir:      "/var/lib/srv/instances",
		KernelPath:        "/var/lib/srv/images/arch-base/vmlinux",
	})

	var removed []string
	removePath = func(path string) error {
		removed = append(removed, path)
		return os.Remove(path)
	}

	if err := server.cleanupFirecrackerCgroup("demo"); err != nil {
		t.Fatalf("cleanupFirecrackerCgroup(): %v", err)
	}
	if len(removed) != 1 || removed[0] != cgroupPath {
		t.Fatalf("cleanupFirecrackerCgroup() removed %#v, want only %q", removed, cgroupPath)
	}
	if _, err := os.Stat(poolPath); err != nil {
		t.Fatalf("pooled parent cgroup should remain after leaf cleanup: %v", err)
	}
}

func TestServerDeleteMemoryPoolCgroupRemovesPoolParent(t *testing.T) {
	oldRoot := cgroupFSRoot
	oldCurrent := currentCgroupPath
	oldRemove := removePath
	t.Cleanup(func() {
		cgroupFSRoot = oldRoot
		currentCgroupPath = oldCurrent
		removePath = oldRemove
	})

	cgroupFSRoot = t.TempDir()
	serviceRel := "/system.slice/srv-vm-runner.service"
	currentCgroupPath = func() (string, error) {
		return serviceRel, nil
	}

	serviceCgroup := filepath.Join(cgroupFSRoot, "system.slice", "srv-vm-runner.service")
	poolPath := filepath.Join(serviceCgroup, firecrackerPoolRootCgroupName, "pool-a")
	if err := os.MkdirAll(poolPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(pool): %v", err)
	}

	server := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), ServerConfig{
		FirecrackerBinary: "/usr/bin/firecracker",
		JailerBinary:      "/usr/bin/jailer",
		JailerBaseDir:     "/var/lib/srv/jailer",
		JailerUID:         123,
		JailerGID:         456,
		InstancesDir:      "/var/lib/srv/instances",
		KernelPath:        "/var/lib/srv/images/arch-base/vmlinux",
	})

	var removed []string
	removePath = func(path string) error {
		removed = append(removed, path)
		return os.Remove(path)
	}

	if err := server.deleteMemoryPoolCgroup(context.Background(), MemoryPoolRequest{Name: "pool-a"}); err != nil {
		t.Fatalf("deleteMemoryPoolCgroup(): %v", err)
	}
	if len(removed) != 1 || removed[0] != poolPath {
		t.Fatalf("deleteMemoryPoolCgroup() removed %#v, want only %q", removed, poolPath)
	}
	if _, err := os.Stat(poolPath); !os.IsNotExist(err) {
		t.Fatalf("pool cgroup should be removed, stat err = %v", err)
	}

	if err := server.deleteMemoryPoolCgroup(context.Background(), MemoryPoolRequest{Name: "pool-a"}); err != nil {
		t.Fatalf("deleteMemoryPoolCgroup() should ignore missing pools: %v", err)
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
		{name: "negative vm pids max", cfg: ServerConfig{FirecrackerBinary: valid.FirecrackerBinary, JailerBinary: valid.JailerBinary, JailerBaseDir: valid.JailerBaseDir, InstancesDir: valid.InstancesDir, KernelPath: valid.KernelPath, JailerUID: valid.JailerUID, JailerGID: valid.JailerGID, VMPIDsMax: -1}},
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
