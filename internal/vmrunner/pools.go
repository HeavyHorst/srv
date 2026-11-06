package vmrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"srv/internal/storage"
)

type ResizeMemoryPoolRequest struct {
	RequestID string `json:"request_id"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	Running   bool   `json:"running"`
}

func (r ResizeMemoryPoolRequest) Validate() error {
	if _, err := uuid.Parse(r.RequestID); err != nil {
		return errors.New("valid pool resize request ID required")
	}
	if err := (MemoryPoolRequest{Name: r.Name}).Validate(); err != nil {
		return err
	}
	if r.SizeBytes <= 0 {
		return errors.New("memory pool size must be positive")
	}
	return nil
}

func (c *Client) ResizeMemoryPool(ctx context.Context, req ResizeMemoryPoolRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	var result memoryPoolResizeResult
	if err := c.post(ctx, "/pool/resize", req, &result); err != nil {
		return err
	}
	if !result.Done || result.Request != req {
		return errors.New("pool resize outcome unknown")
	}
	if result.Error != "" {
		return &MemoryPoolResizeRejected{Reason: result.Error}
	}
	return nil
}

type MemoryPoolResizeRejected struct{ Reason string }

func (e *MemoryPoolResizeRejected) Error() string { return e.Reason }

type memoryPoolResizeResult struct {
	Request ResizeMemoryPoolRequest `json:"request"`
	Done    bool                    `json:"done"`
	Error   string                  `json:"error,omitempty"`
}

func (s *Server) handleResizeMemoryPool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s is not allowed", r.Method))
		return
	}
	var req ResizeMemoryPoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, fmt.Errorf("decode memory pool resize request: %w", err))
		return
	}
	if err := req.Validate(); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.resizeMemoryPool(req)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// Receipts outlive pools: a delayed duplicate must not mutate a replacement
// pool with the same name, even after a runner restart.
func (s *Server) resizeMemoryPool(req ResizeMemoryPoolRequest) (memoryPoolResizeResult, error) {
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	dir := filepath.Join(s.config.InstancesDir, ".pool-resizes")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return memoryPoolResizeResult{}, err
	}
	if err := storage.SyncPath(s.config.InstancesDir); err != nil {
		return memoryPoolResizeResult{}, err
	}
	path := filepath.Join(dir, req.RequestID+".json")
	result := memoryPoolResizeResult{Request: req}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &result); err != nil {
			return result, err
		}
		if result.Request != req {
			return result, errors.New("pool resize request ID reused with different arguments")
		}
	} else if !os.IsNotExist(err) {
		return result, err
	}
	if err := atomicColdJSON(path, result, 0600); err != nil {
		return result, err
	}
	if result.Done {
		return result, nil
	}
	if err := s.resizeMemoryPoolCgroup(req); err != nil {
		result.Error = err.Error()
	}
	result.Done = true
	if err := atomicColdJSON(path, result, 0600); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Server) resizeMemoryPoolCgroup(req ResizeMemoryPoolRequest) error {
	rootRel, err := s.delegatedCgroupRoot()
	if err != nil {
		return err
	}
	poolPath, err := directChildPath(filepath.Join(cgroupPathOnHost(rootRel), firecrackerPoolRootCgroupName), req.Name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(poolPath); err != nil {
		if os.IsNotExist(err) && !req.Running {
			// Pools that have never run are materialized on their first start.
			return nil
		}
		return err
	}
	limit, err := readInt64File(filepath.Join(poolPath, "memory.max"))
	if err != nil {
		return err
	}
	if limit == req.SizeBytes {
		return nil
	}
	if limit == 0 || req.SizeBytes < limit {
		usage, err := readTextFile(filepath.Join(poolPath, "memory.current"))
		if err != nil {
			return err
		}
		current, err := strconv.ParseInt(strings.TrimSpace(string(usage)), 10, 64)
		if err != nil || current < 0 {
			return errors.New("invalid pool memory usage")
		}
		headroom := max(256*miBBytes, req.SizeBytes/10)
		if current > 0 && current > req.SizeBytes-headroom {
			return fmt.Errorf("memory pool %q is too busy to shrink: current usage %d bytes plus safety headroom %d bytes exceeds target %d bytes", req.Name, current, headroom, req.SizeBytes)
		}
	}
	// This is a guarded snapshot, not a guarantee against future allocations.
	// Do not force reclaim or lower the limit when the usage check fails.
	return writeCgroupValue(poolPath, "memory.max", strconv.FormatInt(req.SizeBytes, 10))
}
