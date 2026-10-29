package vmrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"srv/internal/storage"
)

const (
	StartRunning = "running"
	StartAbsent  = "absent"
	StartUnknown = "unknown"
)

type StartResultRequest struct {
	Name       string `json:"name"`
	InstanceID string `json:"instance_id"`
	RequestID  string `json:"request_id"`
	HostBootID string `json:"host_boot_id"`
}

func (r StartResultRequest) Validate() error {
	if !validName.MatchString(r.Name) || r.InstanceID == "" || r.RequestID == "" || r.HostBootID == "" {
		return errors.New("complete start attempt identity is required")
	}
	return nil
}

func (c *Client) ResolveInstanceStart(ctx context.Context, req StartResultRequest) (StartResponse, error) {
	if err := req.Validate(); err != nil {
		return StartResponse{}, err
	}
	var resp StartResponse
	err := c.post(ctx, "/vm/start-result", req, &resp)
	if err == nil {
		err = validateStartResponse(resp)
	}
	return resp, err
}

func validateStartResponse(r StartResponse) error {
	switch r.Outcome {
	case StartRunning:
		if r.PID > 0 {
			return nil
		}
	case StartAbsent, StartUnknown:
		if r.PID == 0 {
			return nil
		}
	}
	return errors.New("invalid start outcome from runner")
}

// Receipts contain only a digest, never the request or bootstrap credentials.
// All reads and writes hold the name flock, including drain in another process.
type startReceipt struct {
	StartResultRequest
	Digest string
	State  string
	PID    int
	Ticks  uint64
}

func receiptKey(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func (s *Server) receiptDir(name string) string {
	return filepath.Join(s.config.JailerBaseDir, ".start-receipts", receiptKey(name))
}
func (s *Server) lockName(name string) (func(), error) {
	return s.lockNameContext(context.Background(), name)
}

func (s *Server) lockNameContext(ctx context.Context, name string) (func(), error) {
	_, release, err := s.lockNameInherited(ctx, name)
	return release, err
}

// lockNameInherited also pins the name lock in storage commands launched by ctx.
// VM launches deliberately do not opt into storage.InheritLocks.
func (s *Server) lockNameInherited(ctx context.Context, name string) (context.Context, func(), error) {
	if !validName.MatchString(name) {
		return ctx, nil, errors.New("invalid instance name")
	}
	if s.config.JailerBaseDir == "" {
		return ctx, nil, errors.New("jailer base directory required for lifecycle locking")
	}
	dir := s.receiptDir(name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ctx, nil, err
	}
	// Sync both newly created directory entries before accepting an operation.
	for _, p := range []string{filepath.Dir(s.config.JailerBaseDir), s.config.JailerBaseDir, filepath.Dir(dir), dir} {
		if err := syncDirectory(p); err != nil {
			return ctx, nil, err
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return ctx, nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return ctx, nil, err
		}
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			f.Close()
			return ctx, nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return ctx, nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return storage.WithInheritedLock(ctx, f), func() { _ = f.Close() }, nil
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (s *Server) writeReceipt(r startReceipt) error {
	dir := s.receiptDir(r.Name)
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, receiptKey(r.RequestID)+".json")); err != nil {
		return err
	}
	return syncDirectory(dir)
}
func (s *Server) readReceipt(q StartResultRequest) (*startReceipt, error) {
	b, err := os.ReadFile(filepath.Join(s.receiptDir(q.Name), receiptKey(q.RequestID)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r startReceipt
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.StartResultRequest != q {
		return nil, errors.New("receipt identity mismatch")
	}
	if err := r.StartResultRequest.Validate(); err != nil {
		return nil, err
	}
	if r.State != "accepted" && r.State != "started" && r.State != "closed" {
		return nil, errors.New("corrupt receipt state")
	}
	if r.State != "closed" && len(r.Digest) != 64 {
		return nil, errors.New("corrupt receipt digest")
	}
	if r.State == "started" && (r.PID <= 0 || r.Ticks == 0 || r.Digest == "") {
		return nil, errors.New("corrupt started receipt")
	}
	return &r, nil
}
func hostBootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", errors.New("empty host boot ID")
	}
	return id, nil
}
func processTicks(pid int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	i := strings.LastIndex(string(b), ")")
	if i < 0 {
		return 0, errors.New("invalid process stat")
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return 0, errors.New("short process stat")
	}
	if f[0] == "Z" || f[0] == "X" {
		return 0, os.ErrNotExist
	}
	return strconv.ParseUint(f[19], 10, 64)
}

// A successful connect must identify the recorded process, not merely a socket
// at the expected pathname. Re-check process ticks after obtaining peer creds.
func (s *Server) validateRuntime(r startReceipt) error {
	path := filepath.Join(s.config.InstancesDir, r.Name, "firecracker.sock")
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	raw, err := c.(*net.UnixConn).SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	var sockErr error
	if err := raw.Control(func(fd uintptr) { cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return err
	}
	if sockErr != nil {
		return sockErr
	}
	if int(cred.Pid) != r.PID {
		return errors.New("socket belongs to another process")
	}
	ticks, err := processTicks(r.PID)
	if err != nil {
		return err
	}
	if ticks != r.Ticks {
		return errors.New("process identity changed")
	}
	return nil
}

func (s *Server) resultLocked(r *startReceipt) (StartResponse, error) {
	boot, err := hostBootID()
	if err != nil {
		return StartResponse{}, err
	}
	if r.State == "closed" {
		if err := s.nameVacantLocked(r.Name); err != nil {
			return StartResponse{Outcome: StartUnknown, Reason: "name is not proven vacant"}, nil
		}
		return StartResponse{Outcome: StartAbsent, Reason: "attempt fenced"}, nil
	}
	if r.HostBootID != boot {
		return s.finishDeadReceiptLocked(r)
	}
	if r.State == "accepted" {
		return StartResponse{Outcome: StartUnknown, Reason: "launch may have occurred; attempt quarantined"}, nil
	}
	a, err := s.activeReceipt(r.Name)
	if err != nil || a == nil || a.StartResultRequest != r.StartResultRequest {
		return StartResponse{Outcome: StartUnknown, Reason: "receipt ownership ambiguous"}, nil
	}
	ticks, err := processTicks(r.PID)
	if errors.Is(err, os.ErrNotExist) || (err == nil && ticks != r.Ticks) {
		return s.finishDeadReceiptLocked(r)
	}
	if err != nil {
		return StartResponse{}, err
	}
	if err := s.validateRuntime(*r); err != nil {
		return StartResponse{Outcome: StartUnknown, Reason: "runtime ownership cannot be verified"}, nil
	}
	return StartResponse{Outcome: StartRunning, PID: r.PID}, nil
}

func (s *Server) activeReceipt(name string) (*startReceipt, error) {
	entries, err := os.ReadDir(s.receiptDir(name))
	if err != nil {
		return nil, err
	}
	var active *startReceipt
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.receiptDir(name), e.Name()))
		if err != nil {
			return nil, err
		}
		var r startReceipt
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		if r.Name != name || e.Name() != receiptKey(r.RequestID)+".json" {
			return nil, errors.New("corrupt receipt identity")
		}
		checked, err := s.readReceipt(r.StartResultRequest)
		if err != nil {
			return nil, err
		}
		if checked.State != "closed" {
			if active != nil {
				return nil, errors.New("multiple active attempts")
			}
			active = checked
		}
	}
	return active, nil
}

// Receipt scanning is deliberately read-only: callers decide whether an owned
// dead attempt can be cleaned. Never resolve recursively while scanning peers.
func (s *Server) nameVacantLocked(name string) error {
	a, err := s.activeReceipt(name)
	if err != nil {
		return err
	}
	if a != nil {
		return errors.New("another attempt occupies name")
	}
	if err := s.noLiveRuntimeLocked(name); err != nil {
		return err
	}
	paths, err := resolveJailerRuntimePaths(s.config.JailerBaseDir, s.config.FirecrackerBinary, name)
	if err != nil {
		return err
	}
	for _, path := range []string{paths.WorkspaceDir, filepath.Join(s.config.InstancesDir, name, "firecracker.sock")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("unowned runtime path %s: %v", path, err)
		}
	}
	return nil
}

func (s *Server) noLiveRuntimeLocked(name string) error {
	vms, err := s.listRunningVMs()
	if err != nil {
		return err
	}
	for _, vm := range vms {
		if vm.Name == name {
			return errors.New("live cgroup occupies name")
		}
	}
	c, err := net.DialTimeout("unix", filepath.Join(s.config.InstancesDir, name, "firecracker.sock"), time.Second)
	if err == nil {
		c.Close()
		return errors.New("live socket occupies name")
	}
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ECONNREFUSED) {
		return err
	}
	return nil
}

func (s *Server) finishDeadReceiptLocked(r *startReceipt) (StartResponse, error) {
	a, err := s.activeReceipt(r.Name)
	if err != nil || a == nil || a.StartResultRequest != r.StartResultRequest {
		return StartResponse{Outcome: StartUnknown, Reason: "receipt ownership ambiguous"}, nil
	}
	if err := s.noLiveRuntimeLocked(r.Name); err != nil {
		return StartResponse{Outcome: StartUnknown, Reason: "runtime absence cannot be verified"}, nil
	}
	if err := s.cleanupFirecrackerCgroup(r.Name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return StartResponse{}, err
	}
	if err := s.cleanupVMRuntime(r.Name); err != nil {
		return StartResponse{}, err
	}
	r.State = "closed"
	if err := s.writeReceipt(*r); err != nil {
		return StartResponse{}, err
	}
	return StartResponse{Outcome: StartAbsent, Reason: "dead owned runtime cleaned"}, nil
}

func (s *Server) receiptStart(ctx context.Context, req StartRequest) (StartResponse, error) {
	// Empty configurations exist only in legacy injected-handler fixtures.
	if req.RequestID == "" && s.config.JailerBaseDir == "" {
		return s.start(ctx, req)
	}
	if err := validateHostSocketPaths(s.config, req.Name, req.Kind); err != nil {
		return StartResponse{}, err
	}
	unlock, err := s.lockNameContext(ctx, req.Name)
	if err != nil {
		return StartResponse{}, err
	}
	defer unlock()
	if req.RequestID == "" {
		if err := s.coldStartFence(ColdRequest{Name: req.Name, InstanceID: req.InstanceID}); err != nil {
			return StartResponse{}, err
		}
		a, err := s.activeReceipt(req.Name)
		if err != nil {
			return StartResponse{}, err
		}
		if a != nil {
			return StartResponse{Outcome: StartUnknown, Reason: "managed attempt occupies name"}, nil
		}
		return s.start(ctx, req)
	}
	q := StartResultRequest{req.Name, req.InstanceID, req.RequestID, req.HostBootID}
	if err := q.Validate(); err != nil {
		return StartResponse{}, err
	}
	b, err := json.Marshal(req)
	if err != nil {
		return StartResponse{}, err
	}
	digest := receiptKey(string(b))
	r, err := s.readReceipt(q)
	if err != nil {
		return StartResponse{}, err
	}
	if r != nil {
		if r.Digest != "" && r.Digest != digest {
			return StartResponse{}, errors.New("start request payload mismatch")
		}
		return s.resultLocked(r)
	}
	// Replays resolve the recorded outcome; only a new attempt may start a VM
	// and must pass the disk/incarnation fence.
	if err := s.coldStartFence(ColdRequest{Name: req.Name, InstanceID: req.InstanceID}); err != nil {
		return StartResponse{}, err
	}
	boot, err := hostBootID()
	if err != nil {
		return StartResponse{}, err
	}
	r = &startReceipt{StartResultRequest: q, Digest: digest, State: "closed"}
	if boot != req.HostBootID {
		if err := s.writeReceipt(*r); err != nil {
			return StartResponse{}, err
		}
		return s.resultLocked(r)
	}
	a, err := s.activeReceipt(req.Name)
	if err != nil {
		return StartResponse{}, err
	}
	if a != nil {
		return StartResponse{Outcome: StartUnknown, Reason: "another attempt occupies name"}, nil
	}
	vms, err := s.listRunningVMs()
	if err != nil {
		return StartResponse{}, err
	}
	for _, vm := range vms {
		if vm.Name == req.Name {
			return StartResponse{Outcome: StartUnknown, Reason: "unowned cgroup runtime occupies name"}, nil
		}
	}
	// Never overwrite an unowned runtime, even if its socket is not responsive.
	paths, err := resolveJailerRuntimePaths(s.config.JailerBaseDir, s.config.FirecrackerBinary, req.Name)
	if err != nil {
		return StartResponse{}, err
	}
	for _, path := range []string{paths.WorkspaceDir, filepath.Join(s.config.InstancesDir, req.Name, "firecracker.sock")} {
		if _, err := os.Lstat(path); err == nil {
			return StartResponse{Outcome: StartUnknown, Reason: "unowned runtime occupies name"}, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return StartResponse{}, err
		}
	}
	r.State = "accepted"
	if err := s.writeReceipt(*r); err != nil {
		return StartResponse{}, err
	}
	initCtx, cancel := context.WithTimeout(ctx, launchInitializationTimeout)
	defer cancel()
	resp, launchErr := s.start(initCtx, req)
	if launchErr != nil {
		return StartResponse{Outcome: StartUnknown, Reason: "launch failed after acceptance; quarantined"}, nil
	}
	r.PID = resp.PID
	r.Ticks, err = processTicks(r.PID)
	if err != nil {
		return StartResponse{Outcome: StartUnknown, Reason: "launched process identity unavailable"}, nil
	}
	r.State = "started"
	if err := s.writeReceipt(*r); err != nil {
		return StartResponse{}, err
	}
	return s.resultLocked(r)
}

func (s *Server) handleStartResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, errors.New("POST required"))
		return
	}
	var q StartResultRequest
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		respondError(w, 400, err)
		return
	}
	if err := q.Validate(); err != nil {
		respondError(w, 400, err)
		return
	}
	resp, err := s.resolveStartContext(r.Context(), q)
	if err != nil {
		respondError(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
func (s *Server) resolveStart(q StartResultRequest) (StartResponse, error) {
	return s.resolveStartContext(context.Background(), q)
}
func (s *Server) resolveStartContext(ctx context.Context, q StartResultRequest) (StartResponse, error) {
	unlock, err := s.lockNameContext(ctx, q.Name)
	if err != nil {
		return StartResponse{}, err
	}
	defer unlock()
	r, err := s.readReceipt(q)
	if err != nil {
		return StartResponse{}, err
	}
	if r == nil {
		r = &startReceipt{StartResultRequest: q, State: "closed"}
		if err := s.writeReceipt(*r); err != nil {
			return StartResponse{}, err
		}
	}
	return s.resultLocked(r)
}

func (s *Server) receiptStop(ctx context.Context, req StopRequest) error {
	if req.RequestID == "" && s.config.JailerBaseDir == "" {
		return s.stop(ctx, req)
	}
	unlock, err := s.lockNameContext(ctx, req.Name)
	if err != nil {
		return err
	}
	defer unlock()
	if req.RequestID == "" {
		if req.InstanceID != "" {
			data, err := os.ReadFile(s.coldDeletedPath(ColdRequest{Name: req.Name, InstanceID: req.InstanceID}))
			if err == nil {
				var id string
				if err := json.Unmarshal(data, &id); err != nil || id != req.InstanceID {
					return errors.New("invalid incarnation deletion fence")
				}
				return nil // A delayed legacy stop must not touch a successor.
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		a, err := s.activeReceipt(req.Name)
		if err != nil {
			return err
		}
		if a != nil {
			return errors.New("managed attempt occupies name")
		}
		return s.stop(ctx, req)
	}
	q := StartResultRequest{req.Name, req.InstanceID, req.RequestID, req.HostBootID}
	if err := q.Validate(); err != nil {
		return err
	}
	r, err := s.readReceipt(q)
	if err != nil {
		return err
	}
	if r == nil {
		return errors.New("start receipt not found")
	}
	return s.stopReceiptLocked(ctx, *r)
}

func (s *Server) stopReceiptLocked(ctx context.Context, r startReceipt) error {
	boot, err := hostBootID()
	if err != nil {
		return err
	}
	if r.State == "closed" {
		return nil
	} // Never name-clean a closed attempt, even if another runtime occupies it.
	if r.HostBootID != boot {
		resp, err := s.finishDeadReceiptLocked(&r)
		if err == nil && resp.Outcome != StartAbsent {
			err = errors.New(resp.Reason)
		}
		return err
	}
	if r.State != "started" {
		return errors.New("cannot stop quarantined or unverified attempt")
	}
	ticks, err := processTicks(r.PID)
	if errors.Is(err, os.ErrNotExist) || (err == nil && ticks != r.Ticks) {
		resp, err := s.finishDeadReceiptLocked(&r)
		if err == nil && resp.Outcome != StartAbsent {
			err = errors.New(resp.Reason)
		}
		return err
	}
	if err != nil {
		return err
	}
	a, err := s.activeReceipt(r.Name)
	if err != nil || a == nil || a.StartResultRequest != r.StartResultRequest {
		return errors.New("receipt ownership ambiguous")
	}
	fd, err := unix.PidfdOpen(r.PID, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := s.validateRuntime(r); err != nil {
		return err
	}
	// Signals target the stable pidfd, never a caller-supplied or reused PID.
	stopCtx, cancel := context.WithTimeout(context.Background(), gracefulStopRequestTimeout)
	_ = requestGuestShutdown(stopCtx, filepath.Join(s.config.InstancesDir, r.Name, "firecracker.sock"))
	cancel()
	if err := waitPidfd(fd, gracefulStopTimeout); err != nil {
		if !errors.Is(err, errProcessExitTimeout) {
			return err
		}
		if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
			return err
		}
		if err := waitPidfd(fd, postKillWaitTimeout); err != nil {
			return err
		}
	}
	if err := s.noLiveRuntimeLocked(r.Name); err != nil {
		return err
	}
	if err := s.cleanupFirecrackerCgroup(r.Name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := s.cleanupVMRuntime(r.Name); err != nil {
		return err
	}
	r.State = "closed"
	return s.writeReceipt(r)
}

// Legacy runtimes lack durable identity. Re-enumerate under the name lock and
// require a single process whose API peer credentials verify its identity.
// A caller's stale PID is never authority to signal anything.
func (s *Server) stopLegacyLocked(ctx context.Context, name string, suppliedPID int) error {
	vms, err := s.listRunningVMs()
	if err != nil {
		return err
	}
	var owned *runningVM
	for i := range vms {
		if vms[i].Name == name {
			if owned != nil || len(vms[i].PIDs) != 1 {
				return errors.New("legacy runtime ownership ambiguous")
			}
			owned = &vms[i]
		}
	}
	if owned != nil {
		pid := owned.PIDs[0]
		if suppliedPID > 0 && suppliedPID != pid {
			return errors.New("legacy runtime PID mismatch")
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			return err
		}
		defer unix.Close(fd)
		ticks, err := processTicks(pid)
		if err != nil {
			return err
		}
		if err := s.validateRuntime(startReceipt{StartResultRequest: StartResultRequest{Name: name}, PID: pid, Ticks: ticks}); err != nil {
			return fmt.Errorf("unverified legacy runtime: %w", err)
		}
		stopCtx, cancel := context.WithTimeout(ctx, gracefulStopRequestTimeout)
		_ = requestGuestShutdown(stopCtx, owned.SocketPath)
		cancel()
		if err := waitPidfd(fd, gracefulStopTimeout); err != nil {
			if !errors.Is(err, errProcessExitTimeout) {
				return err
			}
			if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
				return err
			}
			if err := waitPidfd(fd, postKillWaitTimeout); err != nil {
				return err
			}
		}
	}
	if err := s.noLiveRuntimeLocked(name); err != nil {
		return err
	}
	if owned != nil {
		err = cleanupFirecrackerCgroupPath(name, owned.CgroupPath)
	} else {
		err = s.cleanupFirecrackerCgroup(name)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.cleanupVMRuntime(name)
}

func waitPidfd(fd int, timeout time.Duration) error {
	p := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return errProcessExitTimeout
		}
		n, err := unix.Poll(p, int(remaining.Milliseconds())+1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return errProcessExitTimeout
		}
		if p[0].Revents&unix.POLLIN != 0 {
			return nil
		}
		return errors.New("invalid pidfd poll result")
	}
}
