package vmrunner

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
)

func receiptRequest(t *testing.T) StartRequest {
	t.Helper()
	boot, err := hostBootID()
	if err != nil {
		t.Fatal(err)
	}
	return StartRequest{Name: "demo", InstanceID: "instance-1", RequestID: "92629fe2-dafb-498b-92d8-934e579109ad", HostBootID: boot,
		TapDevice: "tap-demo", GuestMAC: "02:fc:aa:bb:cc:dd", GuestAddr: "10.0.0.2/30", GatewayAddr: "10.0.0.1",
		VCPUCount: 1, MemoryMiB: 512, Bootstrap: Bootstrap{Hostname: "demo", TailscaleAuthKey: "secret-not-on-disk"}}
}
func resultRequest(r StartRequest) StartResultRequest {
	return StartResultRequest{r.Name, r.InstanceID, r.RequestID, r.HostBootID}
}

func shortReceiptDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "vrr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func receiptHTTPClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	path := filepath.Join(shortReceiptDir(t), "runner.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(l) }()
	c := NewClient(path)
	t.Cleanup(func() { c.httpClient.CloseIdleConnections(); _ = srv.Close() })
	return c
}

// A real Unix listener owned by this process stands in for the VMM socket.
func receiptRuntime(t *testing.T, cfg ServerConfig, name string) {
	t.Helper()
	dir := filepath.Join(cfg.InstancesDir, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(dir, "firecracker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.NotFoundHandler()}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
}

func TestManagedLostResponseAndRestart(t *testing.T) {
	s, _, _ := newRunnerShutdownTestServer(t)
	r := receiptRequest(t)
	var launches atomic.Int32
	s.start = func(context.Context, StartRequest) (StartResponse, error) {
		launches.Add(1)
		receiptRuntime(t, s.config, r.Name)
		return StartResponse{PID: os.Getpid()}, nil
	}
	var drop atomic.Bool
	drop.Store(true)
	h := s.Handler()
	c := receiptHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/vm/start" && drop.Swap(false) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Errorf("start status %d: %s", rec.Code, rec.Body.String())
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		h.ServeHTTP(w, req)
	}))
	ctx := context.Background()
	if _, err := c.StartInstanceVM(ctx, r); err == nil {
		t.Fatal("lost response must return transport error")
	}
	resp, err := c.ResolveInstanceStart(ctx, resultRequest(r))
	if err != nil || resp.Outcome != StartRunning || resp.PID != os.Getpid() {
		t.Fatalf("resolve: %+v %v", resp, err)
	}
	// A fresh helper reads the committed receipt and adopts, never boots again.
	restarted := NewServerWithHandlers(nil, s.config, func(context.Context, StartRequest) (StartResponse, error) {
		launches.Add(1)
		return StartResponse{}, errors.New("must not launch")
	}, nil)
	c2 := receiptHTTPClient(t, restarted.Handler())
	resp, err = c2.StartInstanceVM(ctx, r)
	if err != nil || resp.Outcome != StartRunning || launches.Load() != 1 {
		t.Fatalf("replay: %+v %v launches=%d", resp, err, launches.Load())
	}
	r.MemoryMiB++
	if _, err := c2.StartInstanceVM(ctx, r); err == nil {
		t.Fatal("same UUID with changed payload accepted")
	}
	entries, err := os.ReadDir(s.receiptDir(r.Name))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(s.receiptDir(r.Name), e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), r.Bootstrap.TailscaleAuthKey) {
			t.Fatal("bootstrap secret persisted")
		}
	}
}

func TestManagedResolveBeforeStartAndBootMismatch(t *testing.T) {
	for _, oldBoot := range []bool{false, true} {
		t.Run(map[bool]string{false: "resolve-before-start", true: "old-boot"}[oldBoot], func(t *testing.T) {
			s, _, _ := newRunnerShutdownTestServer(t)
			s.config.InstancesDir = shortReceiptDir(t)
			r := receiptRequest(t)
			var launches atomic.Int32
			s.start = func(context.Context, StartRequest) (StartResponse, error) {
				launches.Add(1)
				return StartResponse{}, nil
			}
			c := receiptHTTPClient(t, s.Handler())
			ctx := context.Background()
			if oldBoot {
				r.HostBootID = "previous-host-boot"
			} else {
				resp, err := c.ResolveInstanceStart(ctx, resultRequest(r))
				if err != nil || resp.Outcome != StartAbsent {
					t.Fatalf("fence: %+v %v", resp, err)
				}
			}
			resp, err := c.StartInstanceVM(ctx, r)
			if err != nil || resp.Outcome != StartAbsent || launches.Load() != 0 {
				t.Fatalf("delayed start: %+v %v launches=%d", resp, err, launches.Load())
			}
			// A fenced stop must not touch another/unowned runtime at this name.
			receiptRuntime(t, s.config, r.Name)
			if err := c.StopInstanceVM(ctx, StopRequest{Name: r.Name, InstanceID: r.InstanceID, RequestID: r.RequestID, HostBootID: r.HostBootID, PID: os.Getpid()}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(s.config.InstancesDir, r.Name, "firecracker.sock")); err != nil {
				t.Fatal("fenced stop removed unowned socket", err)
			}
		})
	}
}

func TestManagedAcceptedRestartQuarantinesAndFencesPeers(t *testing.T) {
	s, _, _ := newRunnerShutdownTestServer(t)
	r := receiptRequest(t)
	var launches atomic.Int32
	s.start = func(context.Context, StartRequest) (StartResponse, error) {
		launches.Add(1)
		return StartResponse{}, errors.New("interrupted after possible launch")
	}
	c := receiptHTTPClient(t, s.Handler())
	ctx := context.Background()
	resp, err := c.StartInstanceVM(ctx, r)
	if err != nil || resp.Outcome != StartUnknown {
		t.Fatalf("start: %+v %v", resp, err)
	}
	restarted := NewServerWithHandlers(nil, s.config, s.start, nil)
	c2 := receiptHTTPClient(t, restarted.Handler())
	resp, err = c2.ResolveInstanceStart(ctx, resultRequest(r))
	if err != nil || resp.Outcome != StartUnknown {
		t.Fatalf("resolve: %+v %v", resp, err)
	}
	resp, err = c2.StartInstanceVM(ctx, r)
	if err != nil || resp.Outcome != StartUnknown {
		t.Fatalf("replay: %+v %v", resp, err)
	}
	other := r
	other.RequestID = "other-request"
	resp, err = c2.StartInstanceVM(ctx, other)
	if err != nil || resp.Outcome != StartUnknown {
		t.Fatalf("peer: %+v %v", resp, err)
	}
	legacy := r
	legacy.RequestID = ""
	resp, err = c2.StartInstanceVM(ctx, legacy)
	if err != nil || resp.Outcome != StartUnknown {
		t.Fatalf("legacy: %+v %v", resp, err)
	}
	if err := c2.StopInstanceVM(ctx, StopRequest{Name: r.Name, InstanceID: r.InstanceID, RequestID: r.RequestID, HostBootID: r.HostBootID, PID: os.Getpid()}); err == nil {
		t.Fatal("quarantined attempt stopped")
	}
	if err := c2.StopInstanceVM(ctx, StopRequest{Name: r.Name, PID: os.Getpid()}); err == nil {
		t.Fatal("legacy stop bypassed managed quarantine")
	}
	if launches.Load() != 1 {
		t.Fatalf("launches=%d", launches.Load())
	}
	// Receipt corruption must fail closed, rather than becoming absent.
	if err := os.WriteFile(filepath.Join(s.receiptDir(r.Name), receiptKey(r.RequestID)+".json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.ResolveInstanceStart(ctx, resultRequest(r)); err == nil {
		t.Fatal("corruption ignored")
	}
}

func TestFailedStopPreservesRuntimeArtifacts(t *testing.T) {
	for _, failure := range []string{"process", "cgroup"} {
		t.Run(failure, func(t *testing.T) {
			s := newStopVMTestServer(t)
			pid := startStopVMTestProcess(t)
			receiptRuntime(t, s.config, "demo")
			jail, err := resolveJailerRuntimePaths(s.config.JailerBaseDir, s.config.FirecrackerBinary, "demo")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(jail.RootDir, 0700); err != nil {
				t.Fatal(err)
			}
			oldRequest, oldForce, oldRemove := requestGuestShutdown, forceStopProcess, removePath
			t.Cleanup(func() { requestGuestShutdown = oldRequest; forceStopProcess = oldForce; removePath = oldRemove })
			requestGuestShutdown = func(context.Context, string) error { return errors.New("guest unavailable") }
			forceStopProcess = func(int) error {
				if failure == "process" {
					return errors.New("kill failed")
				}
				return nil
			}
			removePath = func(string) error { return syscall.EBUSY }
			c := receiptHTTPClient(t, s.Handler())
			if err := c.StopInstanceVM(context.Background(), StopRequest{Name: "demo", PID: pid}); err == nil {
				t.Fatal("stop succeeded")
			}
			for _, path := range []string{jail.RootDir, filepath.Join(s.config.InstancesDir, "demo", "firecracker.sock")} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("failed stop removed %s: %v", path, err)
				}
			}
		})
	}
}

// This subprocess owns a disposable Unix API socket, not a VM. Exiting on the
// SDK shutdown action exercises peer credentials, pidfd wait and real cleanup.
func TestReceiptRuntimeProcess(t *testing.T) {
	path := os.Getenv("SRV_RECEIPT_TEST_SOCKET")
	if path == "" {
		return
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		os.Exit(2)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		if r.URL.Path == "/actions" {
			go func() { time.Sleep(20 * time.Millisecond); os.Exit(0) }()
		}
	})}
	_ = srv.Serve(l)
	os.Exit(3)
}

func TestManagedStopValidatesRuntimeAndCommitsAfterCleanup(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "busy-cgroup"}[busy], func(t *testing.T) {
			s, _, serviceCgroup := newRunnerShutdownTestServer(t)
			s.config.InstancesDir = shortReceiptDir(t)
			r := receiptRequest(t)
			var launches atomic.Int32
			s.start = func(context.Context, StartRequest) (StartResponse, error) {
				launches.Add(1)
				dir := filepath.Join(s.config.InstancesDir, r.Name)
				if err := os.MkdirAll(dir, 0700); err != nil {
					return StartResponse{}, err
				}
				path := filepath.Join(dir, "firecracker.sock")
				cmd := exec.Command(os.Args[0], "-test.run=^TestReceiptRuntimeProcess$")
				cmd.Env = append(os.Environ(), "SRV_RECEIPT_TEST_SOCKET="+path)
				if err := cmd.Start(); err != nil {
					return StartResponse{}, err
				}
				done := make(chan struct{})
				go func() { _ = cmd.Wait(); close(done) }()
				t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
				deadline := time.Now().Add(5 * time.Second)
				for {
					if _, err := os.Stat(path); err == nil {
						break
					}
					if time.Now().After(deadline) {
						return StartResponse{}, errors.New("fixture socket timeout")
					}
					time.Sleep(time.Millisecond)
				}
				return StartResponse{PID: cmd.Process.Pid}, nil
			}
			c := receiptHTTPClient(t, s.Handler())
			ctx := context.Background()
			resp, err := c.StartInstanceVM(ctx, r)
			if err != nil || resp.Outcome != StartRunning {
				t.Fatalf("start: %+v %v", resp, err)
			}
			if err := os.MkdirAll(filepath.Join(serviceCgroup, firecrackerVMRootCgroupName, r.Name), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(serviceCgroup, firecrackerVMRootCgroupName, r.Name, "cgroup.procs"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			jail, err := resolveJailerRuntimePaths(s.config.JailerBaseDir, s.config.FirecrackerBinary, r.Name)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(jail.RootDir, 0700); err != nil {
				t.Fatal(err)
			}
			old := removePath
			t.Cleanup(func() { removePath = old })
			removePath = func(path string) error {
				if busy {
					return syscall.EBUSY
				}
				// Real cgroup pseudo-files do not prevent rmdir.
				_ = os.Remove(filepath.Join(path, "cgroup.procs"))
				return os.Remove(path)
			}
			// A malicious/stale supplied PID must never be used for managed stop.
			err = c.StopInstanceVM(ctx, StopRequest{Name: r.Name, InstanceID: r.InstanceID, RequestID: r.RequestID, HostBootID: r.HostBootID, PID: os.Getpid()})
			if busy && err == nil {
				t.Fatal("busy cgroup stop succeeded")
			}
			if !busy && err != nil {
				t.Fatal(err)
			}
			unlock, err := s.lockName(r.Name)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := s.readReceipt(resultRequest(r))
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			if busy {
				if receipt.State != "started" {
					t.Fatalf("failed cleanup closed receipt: %+v", receipt)
				}
				for _, path := range []string{jail.RootDir, filepath.Join(s.config.InstancesDir, r.Name, "firecracker.sock")} {
					if _, err := os.Lstat(path); err != nil {
						t.Fatalf("failed cleanup removed %s: %v", path, err)
					}
				}
			} else {
				if receipt.State != "closed" {
					t.Fatalf("successful cleanup did not close receipt: %+v", receipt)
				}
				resp, err := c.StartInstanceVM(ctx, r)
				if err != nil || resp.Outcome != StartAbsent || launches.Load() != 1 {
					t.Fatalf("stopped replay: %+v %v launches=%d", resp, err, launches.Load())
				}
			}
		})
	}
}

func TestManagedRuntimeOwnershipMismatch(t *testing.T) {
	s, _, _ := newRunnerShutdownTestServer(t)
	s.config.InstancesDir = shortReceiptDir(t)
	r := receiptRequest(t)
	pid := startStopVMTestProcess(t)
	s.start = func(context.Context, StartRequest) (StartResponse, error) {
		receiptRuntime(t, s.config, r.Name)
		return StartResponse{PID: pid}, nil
	}
	c := receiptHTTPClient(t, s.Handler())
	ctx := context.Background()
	resp, err := c.StartInstanceVM(ctx, r)
	if err != nil || resp.Outcome != StartUnknown {
		t.Fatalf("foreign socket adopted: %+v %v", resp, err)
	}
	if err := c.StopInstanceVM(ctx, StopRequest{Name: r.Name, InstanceID: r.InstanceID, RequestID: r.RequestID, HostBootID: r.HostBootID, PID: pid}); err == nil {
		t.Fatal("foreign socket authorized stop")
	}
	if !processExists(pid) {
		t.Fatal("unverified process signaled")
	}
	resp, err = c.ResolveInstanceStart(ctx, resultRequest(r))
	if err != nil || resp.Outcome != StartUnknown {
		t.Fatalf("resolve: %+v %v", resp, err)
	}
}

// Run the exported drain entry point in a different helper process. Its fake
// cgroup only references a disposable sleep process. The accepted receipt must
// quarantine it rather than allowing drain to signal or unlink anything.
func TestReceiptDrainProcess(t *testing.T) {
	payload := os.Getenv("SRV_RECEIPT_DRAIN_CONFIG")
	if payload == "" {
		return
	}
	var cfg ServerConfig
	if err := json.Unmarshal([]byte(payload), &cfg); err != nil {
		t.Fatal(err)
	}
	cgroupFSRoot = os.Getenv("SRV_RECEIPT_DRAIN_CGROUP")
	currentCgroupPath = func() (string, error) { return "/system.slice/srv-vm-runner.service", nil }
	if err := os.WriteFile(os.Getenv("SRV_RECEIPT_DRAIN_READY"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := NewServer(nil, cfg).ShutdownRunningVMs(context.Background()); err == nil {
		t.Fatal("drain bypassed accepted quarantine")
	}
}

func TestManagedDrainSharesCrossProcessNameLock(t *testing.T) {
	s, _, service := newRunnerShutdownTestServer(t)
	r := receiptRequest(t)
	pid := startStopVMTestProcess(t)
	s.start = func(context.Context, StartRequest) (StartResponse, error) {
		return StartResponse{}, errors.New("interrupted")
	}
	c := receiptHTTPClient(t, s.Handler())
	if resp, err := c.StartInstanceVM(context.Background(), r); err != nil || resp.Outcome != StartUnknown {
		t.Fatalf("accept: %+v %v", resp, err)
	}
	writeRunningVMCgroup(t, service, filepath.Join(firecrackerVMRootCgroupName, r.Name), pid)
	unlock, err := s.lockName(r.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	b, err := json.Marshal(s.config)
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestReceiptDrainProcess$")
	cmd.Env = append(os.Environ(), "SRV_RECEIPT_DRAIN_CONFIG="+string(b), "SRV_RECEIPT_DRAIN_CGROUP="+cgroupFSRoot, "SRV_RECEIPT_DRAIN_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drain fixture did not start")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("drain did not wait for name lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	unlock = nil
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain failed to finish after unlock")
	}
	if !processExists(pid) {
		t.Fatal("drain signaled quarantined process")
	}
}

func TestManagedSDKStartFailurePreservesLaunchedSocket(t *testing.T) {
	s, _, _ := newRunnerShutdownTestServer(t)
	s.config.InstancesDir = shortReceiptDir(t)
	r := receiptRequest(t)
	var pid atomic.Int64
	s.start = func(ctx context.Context, req StartRequest) (StartResponse, error) {
		dir := filepath.Join(s.config.InstancesDir, req.Name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return StartResponse{}, err
		}
		path := filepath.Join(dir, "firecracker.sock")
		cmd := exec.Command(os.Args[0], "-test.run=^TestReceiptRuntimeProcess$")
		cmd.Env = append(os.Environ(), "SRV_RECEIPT_TEST_SOCKET="+path)
		machine, err := firecracker.NewMachine(ctx, firecracker.Config{SocketPath: path}, firecracker.WithProcessRunner(cmd))
		if err != nil {
			return StartResponse{}, err
		}
		machine.Handlers = firecracker.Handlers{FcInit: firecracker.HandlerList{}.Append(
			preserveRuntimeLaunchHandler(cmd),
			firecracker.Handler{Name: "fixture.failed-configuration", Fn: func(context.Context, *firecracker.Machine) error {
				return errors.New("configuration failed after launch")
			}},
		)}
		err = machine.Start(ctx)
		if cmd.Process != nil {
			pid.Store(int64(cmd.Process.Pid))
			t.Cleanup(func() { _ = cmd.Process.Kill() })
		}
		return StartResponse{}, err
	}
	c := receiptHTTPClient(t, s.Handler())
	resp, err := c.StartInstanceVM(context.Background(), r)
	if err != nil || resp.Outcome != StartUnknown {
		t.Fatalf("start: %+v %v", resp, err)
	}
	if pid.Load() == 0 || !processExists(int(pid.Load())) {
		t.Fatal("fixture process did not survive failed configuration")
	}
	if _, err := os.Lstat(filepath.Join(s.config.InstancesDir, r.Name, "firecracker.sock")); err != nil {
		t.Fatal("SDK failure removed live runtime socket", err)
	}
	resp, err = c.ResolveInstanceStart(context.Background(), resultRequest(r))
	if err != nil || resp.Outcome != StartUnknown {
		t.Fatalf("resolve: %+v %v", resp, err)
	}
}

func TestManagedDeadReceiptCleanupProtocol(t *testing.T) {
	for _, mode := range []string{"resolve", "stop", "old-boot", "reused-pid", "foreign-socket", "peer", "cleanup-failure"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _ := newRunnerShutdownTestServer(t)
			s.config.InstancesDir = shortReceiptDir(t)
			r := receiptRequest(t)
			q := resultRequest(r)
			if mode == "old-boot" {
				q.HostBootID = "previous-boot"
			}
			// A mismatching start tick models a reused PID without signaling it.
			ticks, err := processTicks(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			receipt := startReceipt{StartResultRequest: q, State: "started", Digest: strings.Repeat("a", 64), PID: os.Getpid(), Ticks: ticks + 1}
			if mode != "reused-pid" {
				cmd := exec.Command("true")
				if err := cmd.Run(); err != nil {
					t.Fatal(err)
				}
				receipt.PID = cmd.Process.Pid
				receipt.Ticks = 1
			}
			unlock, err := s.lockName(r.Name)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.writeReceipt(receipt); err != nil {
				t.Fatal(err)
			}
			if mode == "peer" {
				peer := receipt
				peer.RequestID = "peer"
				peer.State = "accepted"
				if err := s.writeReceipt(peer); err != nil {
					t.Fatal(err)
				}
			}
			unlock()
			jail, err := resolveJailerRuntimePaths(s.config.JailerBaseDir, s.config.FirecrackerBinary, r.Name)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(jail.RootDir, 0700); err != nil {
				t.Fatal(err)
			}
			if mode == "foreign-socket" {
				receiptRuntime(t, s.config, r.Name)
			} else {
				dir := filepath.Join(s.config.InstancesDir, r.Name)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				l, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: filepath.Join(dir, "firecracker.sock")})
				if err != nil {
					t.Fatal(err)
				}
				l.SetUnlinkOnClose(false)
				if err := l.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "cleanup-failure" {
				root, err := s.delegatedCgroupRoot()
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(cgroupPathOnHost(root), firecrackerVMRootCgroupName, r.Name)
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "cgroup.procs"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				old := removePath
				t.Cleanup(func() { removePath = old })
				removePath = func(string) error { return syscall.EBUSY }
			}
			c := receiptHTTPClient(t, s.Handler())
			if mode == "stop" || mode == "reused-pid" {
				if err := c.StopInstanceVM(context.Background(), StopRequest{Name: q.Name, InstanceID: q.InstanceID, RequestID: q.RequestID, HostBootID: q.HostBootID, PID: os.Getpid()}); err != nil {
					t.Fatal(err)
				}
			} else {
				resp, err := c.ResolveInstanceStart(context.Background(), q)
				switch mode {
				case "foreign-socket", "peer":
					if err != nil || resp.Outcome != StartUnknown {
						t.Fatalf("unsafe resolution: %+v %v", resp, err)
					}
				case "cleanup-failure":
					if err == nil {
						t.Fatal("cleanup failure reported absence")
					}
				default:
					if err != nil || resp.Outcome != StartAbsent {
						t.Fatalf("resolve: %+v %v", resp, err)
					}
				}
			}
			_, err = os.Stat(jail.RootDir)
			blocked := mode == "foreign-socket" || mode == "peer" || mode == "cleanup-failure"
			if blocked && err != nil {
				t.Fatalf("ambiguous runtime cleaned: %v", err)
			}
			if !blocked && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("dead runtime left behind: %v", err)
			}
		})
	}
}

func TestManagedFencedResolutionDoesNotAuthorizeForeignCleanup(t *testing.T) {
	for _, occupancy := range []string{"socket", "workspace", "receipt", "cgroup"} {
		t.Run(occupancy, func(t *testing.T) {
			s, _, service := newRunnerShutdownTestServer(t)
			s.config.InstancesDir = shortReceiptDir(t)
			r := receiptRequest(t)
			c := receiptHTTPClient(t, s.Handler())
			q := resultRequest(r)
			if resp, err := c.ResolveInstanceStart(context.Background(), q); err != nil || resp.Outcome != StartAbsent {
				t.Fatalf("fence: %+v %v", resp, err)
			}
			switch occupancy {
			case "socket":
				receiptRuntime(t, s.config, r.Name)
			case "workspace":
				p, err := resolveJailerRuntimePaths(s.config.JailerBaseDir, s.config.FirecrackerBinary, r.Name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(p.RootDir, 0700); err != nil {
					t.Fatal(err)
				}
			case "receipt":
				unlock, err := s.lockName(r.Name)
				if err != nil {
					t.Fatal(err)
				}
				peer := q
				peer.RequestID = "peer"
				err = s.writeReceipt(startReceipt{StartResultRequest: peer, State: "accepted", Digest: strings.Repeat("a", 64)})
				unlock()
				if err != nil {
					t.Fatal(err)
				}
			case "cgroup":
				writeRunningVMCgroup(t, service, filepath.Join(firecrackerVMRootCgroupName, r.Name), os.Getpid())
			}
			for _, missing := range []bool{false, true} {
				if missing {
					q.RequestID = "missing"
				}
				resp, err := c.ResolveInstanceStart(context.Background(), q)
				if err != nil || resp.Outcome != StartUnknown {
					t.Fatalf("foreign occupancy reported absent: %+v %v", resp, err)
				}
			}
		})
	}
}

func TestManagedOldBootAcceptedReceiptCloses(t *testing.T) {
	s, _, _ := newRunnerShutdownTestServer(t)
	r := receiptRequest(t)
	var launches atomic.Int32
	s.start = func(context.Context, StartRequest) (StartResponse, error) {
		launches.Add(1)
		return StartResponse{}, errors.New("interrupted")
	}
	c := receiptHTTPClient(t, s.Handler())
	if resp, err := c.StartInstanceVM(context.Background(), r); err != nil || resp.Outcome != StartUnknown {
		t.Fatalf("accept: %+v %v", resp, err)
	}
	// Model a receipt carried across host reboot without touching host state.
	unlock, err := s.lockName(r.Name)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := s.readReceipt(resultRequest(r))
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	receipt.HostBootID = "previous-host-boot"
	err = s.writeReceipt(*receipt)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	r.HostBootID = receipt.HostBootID
	resp, err := c.ResolveInstanceStart(context.Background(), resultRequest(r))
	if err != nil || resp.Outcome != StartAbsent {
		t.Fatalf("reboot resolve: %+v %v", resp, err)
	}
	unlock, err = s.lockName(r.Name)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err = s.readReceipt(resultRequest(r))
	unlock()
	if err != nil || receipt.State != "closed" || launches.Load() != 1 {
		t.Fatalf("reboot did not durably close: %+v %v", receipt, err)
	}
}

func startReceiptSocketProcess(t *testing.T, cfg ServerConfig, name string) int {
	t.Helper()
	dir := filepath.Join(cfg.InstancesDir, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "firecracker.sock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestReceiptRuntimeProcess$")
	cmd.Env = append(os.Environ(), "SRV_RECEIPT_TEST_SOCKET="+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return cmd.Process.Pid
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture socket timeout")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDrainDiscardsEnumerationAfterReceiptClosed(t *testing.T) {
	s, _, service := newRunnerShutdownTestServer(t)
	r := receiptRequest(t)
	foreign := startStopVMTestProcess(t)
	writeRunningVMCgroup(t, service, filepath.Join(firecrackerVMRootCgroupName, r.Name), foreign)
	vms, err := s.listRunningVMs()
	if err != nil || len(vms) != 1 {
		t.Fatalf("enumerate: %+v %v", vms, err)
	}
	// A concurrent managed stop completes after enumeration, before drain locks.
	unlock, err := s.lockName(r.Name)
	if err != nil {
		t.Fatal(err)
	}
	err = s.writeReceipt(startReceipt{StartResultRequest: resultRequest(r), State: "closed"})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(vms[0].CgroupPath); err != nil {
		t.Fatal(err)
	}
	if err := s.shutdownRunningVM(context.Background(), vms[0]); err != nil {
		t.Fatal(err)
	}
	if !processExists(foreign) {
		t.Fatal("stale enumeration signaled foreign PID")
	}
}

func TestLegacyAbsentCleanupAndRetry(t *testing.T) {
	for _, foreignSocket := range []bool{false, true} {
		t.Run(strconv.FormatBool(foreignSocket), func(t *testing.T) {
			s, _, _ := newRunnerShutdownTestServer(t)
			s.config.InstancesDir = shortReceiptDir(t)
			jail, err := resolveJailerRuntimePaths(s.config.JailerBaseDir, s.config.FirecrackerBinary, "demo")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(jail.RootDir, 0700); err != nil {
				t.Fatal(err)
			}
			if foreignSocket {
				receiptRuntime(t, s.config, "demo")
			}
			c := receiptHTTPClient(t, s.Handler())
			for range 2 {
				err := c.StopInstanceVM(context.Background(), StopRequest{Name: "demo", PID: os.Getpid()})
				if foreignSocket && err == nil || !foreignSocket && err != nil {
					t.Fatalf("cleanup foreign=%v: %v", foreignSocket, err)
				}
			}
			_, err = os.Stat(jail.RootDir)
			if foreignSocket && err != nil || !foreignSocket && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("artifact preservation foreign=%v: %v", foreignSocket, err)
			}
		})
	}
}

func TestStalledInitializationReleasesLockWithoutKillingGuest(t *testing.T) {
	s, _, _ := newRunnerShutdownTestServer(t)
	s.config.InstancesDir = shortReceiptDir(t)
	r := receiptRequest(t)
	var pid int
	s.start = func(ctx context.Context, _ StartRequest) (StartResponse, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > launchInitializationTimeout {
			t.Fatal("initialization has no deadline")
		}
		dir := filepath.Join(s.config.InstancesDir, r.Name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return StartResponse{}, err
		}
		path := filepath.Join(dir, "firecracker.sock")
		// Use the same immortal process context as startVM. The SDK's
		// configuration context must not become exec.CommandContext's context.
		cmd := exec.CommandContext(vmContextForRequest(ctx), os.Args[0], "-test.run=^TestReceiptRuntimeProcess$")
		cmd.Env = append(os.Environ(), "SRV_RECEIPT_TEST_SOCKET="+path)
		machine, err := firecracker.NewMachine(vmContextForRequest(ctx), firecracker.Config{SocketPath: path}, firecracker.WithProcessRunner(cmd))
		if err != nil {
			return StartResponse{}, err
		}
		machine.Handlers = firecracker.Handlers{FcInit: firecracker.HandlerList{}.Append(
			preserveRuntimeLaunchHandler(cmd),
			firecracker.Handler{Name: "fixture.stalled-initialization", Fn: func(ctx context.Context, _ *firecracker.Machine) error {
				<-ctx.Done()
				return ctx.Err()
			}},
		)}
		err = machine.Start(ctx)
		if cmd.Process != nil {
			pid = cmd.Process.Pid
			t.Cleanup(func() { _ = cmd.Process.Kill() })
		}
		return StartResponse{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	resp, err := s.receiptStart(ctx, r)
	if err != nil || resp.Outcome != StartUnknown {
		t.Fatalf("start: %+v %v", resp, err)
	}
	if !processExists(pid) {
		t.Fatal("initialization cancellation killed guest")
	}
	resp, err = s.resolveStart(resultRequest(r))
	if err != nil || resp.Outcome != StartUnknown {
		t.Fatalf("resolve after stalled initialization: %+v %v", resp, err)
	}
	unlock, err := s.lockName(r.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, operation := range []func(context.Context) error{
		func(ctx context.Context) error { _, err := s.receiptStart(ctx, r); return err },
		func(ctx context.Context) error { _, err := s.resolveStartContext(ctx, resultRequest(r)); return err },
		func(ctx context.Context) error { return s.shutdownRunningVM(ctx, runningVM{Name: r.Name}) },
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		err := operation(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lock wait did not honor cancellation: %v", err)
		}
	}
	if !processExists(pid) {
		t.Fatal("lock timeout killed guest")
	}
}

func TestInitializationDeadlineIndependentOfRequestDeadline(t *testing.T) {
	s, _, _ := newRunnerShutdownTestServer(t)
	s.start = func(ctx context.Context, _ StartRequest) (StartResponse, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > launchInitializationTimeout {
			t.Error("background request did not receive bounded initialization context")
		}
		return StartResponse{}, errors.New("fixture initialization failure")
	}
	resp, err := s.receiptStart(context.Background(), receiptRequest(t))
	if err != nil || resp.Outcome != StartUnknown {
		t.Fatalf("start: %+v %v", resp, err)
	}
}

func TestServeUnixReportsHTTPShutdownTimeout(t *testing.T) {
	s, _, _ := newRunnerShutdownTestServer(t)
	entered := make(chan struct{})
	finished := make(chan struct{})
	s.start = func(ctx context.Context, _ StartRequest) (StartResponse, error) {
		defer close(finished)
		close(entered)
		<-ctx.Done()
		return StartResponse{}, ctx.Err()
	}
	path := filepath.Join(shortReceiptDir(t), "runner.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.ServeUnix(ctx, path, "") }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runner socket timeout")
		}
		time.Sleep(time.Millisecond)
	}
	c := NewClient(path)
	defer c.httpClient.CloseIdleConnections()
	requestDone := make(chan struct{})
	r := receiptRequest(t)
	go func() { defer close(requestDone); _, _ = c.StartInstanceVM(context.Background(), r) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("start request did not enter")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("HTTP shutdown timeout not reported: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("HTTP shutdown hung")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("HTTP close did not cancel stalled initialization")
	}
	<-requestDone
}

func TestLegacyOwnedStopCleansFreshlyDiscoveredPooledCgroup(t *testing.T) {
	s, _, service := newRunnerShutdownTestServer(t)
	s.config.InstancesDir = shortReceiptDir(t)
	pid := startReceiptSocketProcess(t, s.config, "demo")
	fixed := filepath.Join(service, firecrackerVMRootCgroupName, "demo")
	pooled := filepath.Join(service, firecrackerPoolRootCgroupName, "pool-a", "demo")
	writeRunningVMCgroup(t, service, filepath.Join(firecrackerVMRootCgroupName, "demo"))
	writeRunningVMCgroup(t, service, filepath.Join(firecrackerPoolRootCgroupName, "pool-a", "demo"), pid)
	old := removePath
	t.Cleanup(func() { removePath = old })
	removePath = os.RemoveAll // fake cgroup files are ordinary files
	c := receiptHTTPClient(t, s.Handler())
	if err := c.StopInstanceVM(context.Background(), StopRequest{Name: "demo", PID: pid}); err != nil {
		t.Fatal(err)
	}
	if processExists(pid) {
		t.Fatal("verified legacy runtime did not exit")
	}
	if _, err := os.Stat(pooled); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned pooled cgroup not removed: %v", err)
	}
	if _, err := os.Stat(fixed); err != nil {
		t.Fatalf("unrelated stale fixed cgroup removed: %v", err)
	}
	for range 2 {
		if err := c.StopInstanceVM(context.Background(), StopRequest{Name: "demo", PID: pid}); err != nil {
			t.Fatalf("already stopped legacy retry: %v", err)
		}
	}
}
