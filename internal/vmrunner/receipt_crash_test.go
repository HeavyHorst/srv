package vmrunner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type crashChild struct {
	cmd      *exec.Cmd
	output   *syncBuffer
	finished chan struct{}
	err      error
}

func startCrashChild(t *testing.T, testName string, env ...string) *crashChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$", "-test.count=1")
	cmd.Env = append(os.Environ(), env...)
	output := &syncBuffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &crashChild{cmd: cmd, output: output, finished: make(chan struct{})}
	go func() {
		child.err = cmd.Wait()
		close(child.finished)
	}()
	t.Cleanup(func() {
		if child.cmd.Process != nil {
			_ = child.cmd.Process.Kill()
		}
		<-child.finished
	})
	return child
}

func (c *crashChild) waitReady(t *testing.T, ready string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-c.finished:
			t.Fatalf("child exited before ready: %v\n%s", c.err, c.output.String())
		default:
		}
		if _, err := os.Stat(ready); err == nil {
			return
		}
		if time.Now().After(deadline) {
			_ = c.cmd.Process.Kill()
			<-c.finished
			t.Fatalf("child did not become ready: %v\n%s", c.err, c.output.String())
		}
		time.Sleep(time.Millisecond)
	}
}

func (c *crashChild) killAndWait(t *testing.T) {
	t.Helper()
	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	<-c.finished
	var exitErr *exec.ExitError
	if !errors.As(c.err, &exitErr) {
		t.Fatalf("child did not die by signal: %v\n%s", c.err, c.output.String())
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("child exit = %v, want SIGKILL\n%s", exitErr, c.output.String())
	}
}

// TestReceiptLockCrashProcess holds the name lock until the parent kills it.
// defer unlock keeps the lock file live; SIGKILL skips the defer and the kernel
// releases the flock with the process.
func TestReceiptLockCrashProcess(t *testing.T) {
	payload := os.Getenv("SRV_RECEIPT_LOCK_CONFIG")
	if payload == "" {
		return
	}
	var cfg ServerConfig
	if err := json.Unmarshal([]byte(payload), &cfg); err != nil {
		t.Fatal(err)
	}
	unlock, err := NewServer(nil, cfg).lockName(os.Getenv("SRV_RECEIPT_LOCK_NAME"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := os.WriteFile(os.Getenv("SRV_RECEIPT_LOCK_READY"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Hour)
}

func TestNameLockReleasedAfterProcessKill(t *testing.T) {
	s, _, _ := newRunnerShutdownTestServer(t)
	name := "crash-lock"
	payload, err := json.Marshal(s.config)
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	child := startCrashChild(t, "TestReceiptLockCrashProcess",
		"SRV_RECEIPT_LOCK_CONFIG="+string(payload),
		"SRV_RECEIPT_LOCK_NAME="+name,
		"SRV_RECEIPT_LOCK_READY="+ready,
	)
	child.waitReady(t, ready)

	// Prove exclusion while the child is alive: the parent must time out.
	shortCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	unlock, err := s.lockNameContext(shortCtx, name)
	cancel()
	if err == nil {
		unlock()
		t.Fatal("parent acquired the name lock while the child was alive")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock probe failed for the wrong reason: %v", err)
	}

	child.killAndWait(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	unlock, err = s.lockNameContext(ctx, name)
	if err != nil {
		t.Fatalf("name lock still held after SIGKILL: %v", err)
	}
	unlock()
}

// TestReceiptAcceptedCrashProcess writes the accepted receipt, then blocks in
// the launch handler. The parent kills it at that exact boundary.
func TestReceiptAcceptedCrashProcess(t *testing.T) {
	payload := os.Getenv("SRV_RECEIPT_ACCEPTED_CONFIG")
	if payload == "" {
		return
	}
	var cfg ServerConfig
	if err := json.Unmarshal([]byte(payload), &cfg); err != nil {
		t.Fatal(err)
	}
	var req StartRequest
	if err := json.Unmarshal([]byte(os.Getenv("SRV_RECEIPT_ACCEPTED_REQUEST")), &req); err != nil {
		t.Fatal(err)
	}
	cgroupFSRoot = os.Getenv("SRV_RECEIPT_ACCEPTED_CGROUP")
	currentCgroupPath = func() (string, error) { return "/system.slice/srv-vm-runner.service", nil }
	server := NewServerWithHandlers(nil, cfg, func(context.Context, StartRequest) (StartResponse, error) {
		if err := os.WriteFile(os.Getenv("SRV_RECEIPT_ACCEPTED_READY"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Hour)
		return StartResponse{}, nil
	}, nil)
	if _, err := server.receiptStart(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptedReceiptQuarantinesAfterCrash(t *testing.T) {
	s, _, _ := newRunnerShutdownTestServer(t)
	r := receiptRequest(t)
	configPayload, err := json.Marshal(s.config)
	if err != nil {
		t.Fatal(err)
	}
	requestPayload, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	child := startCrashChild(t, "TestReceiptAcceptedCrashProcess",
		"SRV_RECEIPT_ACCEPTED_CONFIG="+string(configPayload),
		"SRV_RECEIPT_ACCEPTED_REQUEST="+string(requestPayload),
		"SRV_RECEIPT_ACCEPTED_READY="+ready,
		"SRV_RECEIPT_ACCEPTED_CGROUP="+cgroupFSRoot,
	)
	child.waitReady(t, ready)
	child.killAndWait(t)

	restarted := NewServer(nil, s.config)
	resp, err := restarted.resolveStart(resultRequest(r))
	if err != nil {
		t.Fatalf("resolve after accepted crash: %v", err)
	}
	if resp.Outcome != StartUnknown {
		t.Fatalf("outcome after accepted crash = %q (%s), want %q", resp.Outcome, resp.Reason, StartUnknown)
	}
}

// A crash during writeReceipt can leave a .pending-* temp file. Scans must
// ignore it, while a truncated .json receipt must fail closed rather than
// silently disappear.
func TestReceiptScanIgnoresPendingAndRejectsTruncated(t *testing.T) {
	s, _, _ := newRunnerShutdownTestServer(t)
	r := receiptRequest(t)
	q := resultRequest(r)
	unlock, err := s.lockName(r.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := s.writeReceipt(startReceipt{StartResultRequest: q, Digest: strings.Repeat("a", 64), State: "accepted"}); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(s.receiptDir(r.Name), ".pending-deadbeef")
	if err := os.WriteFile(pending, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	active, err := s.activeReceipt(r.Name)
	if err != nil || active == nil || active.State != "accepted" {
		t.Fatalf("pending temp changed the active receipt: %+v %v", active, err)
	}

	corrupt := filepath.Join(s.receiptDir(r.Name), receiptKey("corrupt-request")+".json")
	if err := os.WriteFile(corrupt, []byte(`{"name":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.activeReceipt(r.Name); err == nil {
		t.Fatal("truncated receipt was silently ignored")
	}
}

// A started receipt with a live process but no verifiable socket must resolve
// as unknown and leave the process executing. A heartbeat, not kill(pid, 0),
// proves continued execution: an unreaped zombie also satisfies processExists.
func TestStartedReceiptSurvivesUnverifiableRuntime(t *testing.T) {
	s, _, _ := newRunnerShutdownTestServer(t)
	boot, err := hostBootID()
	if err != nil {
		t.Fatal(err)
	}
	q := StartResultRequest{Name: "started-survival", InstanceID: "instance-survival", RequestID: "request-survival", HostBootID: boot}
	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	file, err := os.Create(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	process := exec.Command("yes", "alive")
	process.Stdout = file
	if err := process.Start(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
		_ = file.Close()
	})
	deadline := time.Now().Add(time.Second)
	for fileSize(t, heartbeat) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("child did not start writing")
		}
		time.Sleep(time.Millisecond)
	}
	ticks, err := processTicks(process.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := s.lockName(q.Name)
	if err != nil {
		t.Fatal(err)
	}
	err = s.writeReceipt(startReceipt{StartResultRequest: q, Digest: strings.Repeat("a", 64), State: "started", PID: process.Process.Pid, Ticks: ticks})
	unlock()
	if err != nil {
		t.Fatal(err)
	}

	response, err := s.resolveStart(q)
	if err != nil {
		t.Fatalf("resolve started receipt: %v", err)
	}
	if response.Outcome != StartUnknown {
		t.Fatalf("outcome = %q (%s), want %q", response.Outcome, response.Reason, StartUnknown)
	}
	before := fileSize(t, heartbeat)
	time.Sleep(50 * time.Millisecond)
	if after := fileSize(t, heartbeat); after <= before {
		t.Fatalf("resolution stopped the live process: heartbeat stayed at %d bytes", after)
	}
	if got, err := processTicks(process.Process.Pid); err != nil || got != ticks {
		t.Fatalf("process identity after resolution = %d, %v; want %d", got, err, ticks)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
