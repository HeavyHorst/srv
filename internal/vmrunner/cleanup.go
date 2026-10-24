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
