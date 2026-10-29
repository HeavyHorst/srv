package vmrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// Drain executes in the running server, which owns the artifact mounts.
// A separate systemd ExecStop process has a different mount namespace.
func (c *Client) Drain(ctx context.Context) error {
	return c.post(ctx, "/vm/drain", struct{}{}, nil)
}

func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, errors.New("POST required"))
		return
	}
	// Wait for admitted starts before taking the inventory. The fence stays
	// closed even if draining fails; a service restart is required to admit VMs.
	s.startAdmission.Lock()
	s.draining = true
	s.startAdmission.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), runnerShutdownTimeout)
	defer cancel()
	if err := s.shutdownRunningVMs(ctx); err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// VerifyInstanceIdle is for storage conversion of instances which have never
// started. It checks name occupancy, not an incarnation's deletion proof.
func (c *Client) VerifyInstanceIdle(ctx context.Context, name string) error {
	if !validName.MatchString(name) {
		return errors.New("invalid instance name")
	}
	return c.post(ctx, "/vm/storage-idle-v1", struct {
		Name string `json:"name"`
	}{name}, nil)
}

func (s *Server) handleStorageIdle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, errors.New("POST required"))
		return
	}
	var q struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil || !validName.MatchString(q.Name) {
		respondError(w, http.StatusBadRequest, errors.New("valid instance name required"))
		return
	}
	unlock, err := s.lockNameContext(r.Context(), q.Name)
	if err != nil {
		respondError(w, http.StatusConflict, err)
		return
	}
	defer unlock()
	if err := s.storageIdleLocked(q.Name); err != nil {
		respondError(w, http.StatusConflict, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) storageIdleLocked(name string) error {
	if err := s.nameVacantLocked(name); err != nil {
		return err
	}
	root, err := s.delegatedCgroupRoot()
	if err == nil {
		var path string
		path, _, err = resolveFirecrackerCgroupPath(root, name)
		if err == nil {
			err = fmt.Errorf("cgroup still present: %s", path)
		} else if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	}
	return err
}

func (c *Client) VerifyInstanceAbsent(ctx context.Context, q StartResultRequest) error {
	if err := q.Validate(); err != nil {
		return err
	}
	return c.post(ctx, "/vm/cleanup-proof-v1", q, nil)
}

func (s *Server) handleCleanupProof(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, errors.New("POST required"))
		return
	}
	var q StartResultRequest
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	cfg := s.config
	root, err := s.delegatedCgroupRoot()
	if err != nil {
		respondError(w, http.StatusConflict, err)
		return
	}
	cfg.CgroupRoot = root
	if err := VerifyCleanup(r.Context(), cfg, q); err != nil {
		respondError(w, http.StatusConflict, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// VerifyCleanup reads the runner's existing durable journal under its existing
// flock. It does not start a runner, create journal entries, or infer the runner's
// cgroup from the calling process. Only the runner supplies its runtime paths.
func VerifyCleanup(ctx context.Context, cfg ServerConfig, q StartResultRequest) error {
	if err := q.Validate(); err != nil {
		return err
	}
	for _, path := range []string{cfg.InstancesDir, cfg.JailerBaseDir, cfg.FirecrackerBinary, cfg.CgroupRoot} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return errors.New("explicit pinned runtime proof paths required")
		}
	}
	s := NewServer(nil, cfg)
	lock, err := os.Open(filepath.Join(s.receiptDir(q.Name), "lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(lock.Fd()), unix.LOCK_SH|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	r, err := s.readReceipt(q)
	if err != nil {
		return err
	}
	if r == nil || r.State != "closed" {
		return errors.New("exact attempt not closed")
	}
	if cold, err := s.readCold(ColdRequest{Name: q.Name, InstanceID: q.InstanceID}); err != nil || cold != nil {
		return errors.New("cold disk journal must be settled before runtime cleanup proof")
	}
	if err := s.nameVacantLocked(q.Name); err != nil {
		return err
	}
	root, err := s.delegatedCgroupRoot()
	if err != nil {
		return err
	}
	path, _, err := resolveFirecrackerCgroupPath(root, q.Name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("cgroup still present: %s", path)
}
