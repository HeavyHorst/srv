package vmrunner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"srv/internal/coldstore"
	"srv/internal/storage"
)

type ColdRequest struct {
	Name         string                    `json:"name"`
	InstanceID   string                    `json:"instance_id"`
	Action       string                    `json:"action"`
	ExpectedDisk storage.SubvolumeIdentity `json:"expected_disk"`
}

type ColdStatus struct {
	Phase          string `json:"phase"`
	Remote         bool   `json:"remote"`
	JournalPresent bool   `json:"journal_present"`
	Error          string `json:"error,omitempty"`
}

// The root-owned journal is authoritative. The instance marker is only a
// conservative fence for unprivileged disk consumers, never deletion authority.
type coldDiskRecord struct {
	Version    int                       `json:"version"`
	InstanceID string                    `json:"instance_id"`
	Phase      string                    `json:"phase"`
	Remote     bool                      `json:"remote"`
	Host       string                    `json:"host"`
	Disk       storage.SubvolumeIdentity `json:"disk"`
	Candidate  storage.SubvolumeIdentity `json:"candidate"`
	Object     coldstore.Object          `json:"object"`
	Base       storage.SubvolumeIdentity `json:"base"`
	BaseName   string                    `json:"base_name"`
}

type coldRemote interface {
	Put(context.Context, coldstore.Object, string, string) error
	Get(context.Context, coldstore.Object, string, string) (string, error)
	Delete(context.Context, string) error
}

var newColdRemote = func(cfg coldstore.Config) coldRemote { return &coldstore.Client{Config: cfg} }

func (c *Client) ColdDisk(ctx context.Context, q ColdRequest) (ColdStatus, error) {
	var wire json.RawMessage
	if err := c.post(ctx, "/vm/cold-disk-v1", q, &wire); err != nil {
		return ColdStatus{}, err
	}
	var result ColdStatus
	if err := json.Unmarshal(wire, &result); err != nil {
		return result, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(wire, &fields); err != nil {
		return result, err
	}
	if flag := string(fields["journal_present"]); flag != "true" && flag != "false" {
		return result, errors.New("upgrade runner: cold status must explicitly report journal presence")
	}
	return result, nil
}

func (s *Server) coldDir(name string) string { return filepath.Join(s.receiptDir(name), "cold") }

// Only payload stages move to the data Btrfs. Journals and lifetime fences
// stay in their existing root-owned namespace outside instance directories.
func (s *Server) coldStageDir(name string) string {
	return filepath.Join(filepath.Dir(s.config.InstancesDir), ".cold-staging", receiptKey(name))
}
func (s *Server) coldMarker(name string) string {
	return filepath.Join(s.config.InstancesDir, name, "cold-disk.json")
}

func (s *Server) readCold(q ColdRequest) (*coldDiskRecord, error) {
	path := filepath.Join(s.coldDir(q.Name), "state")
	if err := storage.ValidatePath(path); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r coldDiskRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.Version != 1 || r.InstanceID != q.InstanceID || r.Disk.UUID == "" {
		return nil, errors.New("cold disk identity mismatch; operator inspection required")
	}
	switch r.Phase {
	case "offloading", "evicting", "cold", "restoring", "hot", "purged":
	default:
		return nil, errors.New("unknown cold disk phase")
	}
	return &r, nil
}

func atomicColdJSON(path string, value any, mode os.FileMode) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cold-state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	err = errors.Join(err, f.Chmod(mode), f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return storage.SyncPath(filepath.Dir(path))
}

func (s *Server) writeCold(q ColdRequest, r *coldDiskRecord) error {
	if err := atomicColdJSON(filepath.Join(s.coldDir(q.Name), "state"), r, 0600); err != nil {
		return err
	}
	return atomicColdJSON(s.coldMarker(q.Name), struct {
		InstanceID string `json:"instance_id"`
		Phase      string `json:"phase"`
		Remote     bool   `json:"remote"`
	}{r.InstanceID, r.Phase, r.Remote}, 0644)
}

func (s *Server) handleColdDisk(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, errors.New("POST required"))
		return
	}
	var q ColdRequest
	if err := json.NewDecoder(io.LimitReader(request.Body, 4096)).Decode(&q); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	if !validName.MatchString(q.Name) || !canonicalColdUUID(q.InstanceID) {
		respondError(w, http.StatusBadRequest, errors.New("valid instance name and UUID required"))
		return
	}
	if q.Action == "status" {
		// Journals are atomically replaced. Status is an advisory snapshot, not
		// lifecycle admission, and must remain available during long transfers.
		r, err := s.readCold(q)
		if err != nil {
			respondError(w, http.StatusConflict, err)
			return
		}
		result := ColdStatus{Phase: "hot"}
		if r != nil {
			result = ColdStatus{Phase: r.Phase, Remote: r.Remote, JournalPresent: true}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	timeout := s.config.ColdStorage.Timeout
	if timeout <= 0 {
		timeout = coldstore.DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	defer cancel()
	// Drain waits for storage operations just as it waits for admitted starts.
	s.startAdmission.RLock()
	defer s.startAdmission.RUnlock()
	if s.draining {
		respondError(w, http.StatusServiceUnavailable, errors.New("runner is draining"))
		return
	}
	if q.Action == "hydrate" {
		var release func()
		var err error
		ctx, release, err = storage.AcquirePayload(ctx, filepath.Dir(s.config.InstancesDir))
		if err != nil {
			respondError(w, http.StatusConflict, err)
			return
		}
		defer release()
	}
	ctx, unlock, err := s.lockNameInherited(ctx, q.Name)
	if err != nil {
		respondError(w, http.StatusConflict, err)
		return
	}
	defer unlock()
	result, err := s.coldDiskLocked(ctx, q)
	if err != nil {
		respondError(w, http.StatusConflict, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func canonicalColdUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func (s *Server) coldDiskLocked(ctx context.Context, q ColdRequest) (ColdStatus, error) {
	var result ColdStatus
	dir := filepath.Join(s.config.InstancesDir, q.Name)
	if err := storage.ValidatePath(dir); err != nil {
		return result, err
	}
	r, err := s.readCold(q)
	if err != nil {
		return result, err
	}
	if q.Action == "status" {
		if r == nil {
			return ColdStatus{Phase: "hot"}, nil
		}
		return ColdStatus{Phase: r.Phase, Remote: r.Remote, JournalPresent: true}, nil
	}
	switch q.Action {
	case "offload", "hydrate", "purge", "delete", "fence-delete":
	default:
		return result, errors.New("unknown cold disk action")
	}
	// Never silently abandon an interrupted transfer from the older layout.
	for _, name := range []string{"send", "receive", "candidate"} {
		path := filepath.Join(s.coldDir(q.Name), name)
		if _, err := os.Lstat(path); err == nil {
			return result, fmt.Errorf("legacy cold transfer stage %s requires operator recovery before layout upgrade", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
	}
	if q.Action == "fence-delete" && r != nil {
		return result, errors.New("cleanup proof refuses retained cold disk state; hydrate and purge first")
	}
	if q.Action != "delete" && q.Action != "fence-delete" {
		if err := s.coldDeletionFence(q); err != nil {
			return result, err
		}
	}
	if err := s.storageIdleLocked(q.Name); err != nil {
		return result, err
	}
	if q.Action == "delete" || q.Action == "fence-delete" {
		// Keep a small incarnation tombstone after clearing the generation
		// journal. An older offload must not publish after delete was acknowledged.
		if err := atomicColdJSON(s.coldDeletedPath(q), q.InstanceID, 0600); err != nil {
			return result, err
		}
	}
	if err := coldNoLoop(ctx, filepath.Join(dir, "disk", "rootfs.img")); err != nil {
		return result, err
	}
	stage := s.coldStageDir(q.Name)
	if err := storage.ValidatePath(stage); err != nil {
		return result, err
	}
	if err := os.MkdirAll(filepath.Dir(stage), 0755); err != nil {
		return result, err
	}
	if err := os.Mkdir(stage, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return result, err
	}
	for _, private := range []bool{false, true} {
		path := stage
		if !private {
			path = filepath.Dir(stage)
		}
		if err := coldPrivateDir(path, private); err != nil {
			return result, err
		}
	}
	stageFS, err := storage.FilesystemUUID(stage)
	if err != nil {
		return result, err
	}
	dataFS, err := storage.FilesystemUUID(s.config.InstancesDir)
	if err != nil {
		return result, err
	}
	if stageFS != dataFS {
		return result, errors.New("cold transfer staging and instances must share one btrfs filesystem")
	}
	// Bind first publication to the disk observed for this incarnation by the
	// control plane, not whichever disk occupies a reused name when we run.
	if q.Action == "offload" && (r == nil || r.Phase == "hot" || r.Phase == "purged") {
		id, err := storage.ValidateDiskSubvolume(ctx, filepath.Join(dir, "disk"))
		if err != nil || q.ExpectedDisk.UUID == "" || id.UUID != q.ExpectedDisk.UUID || id.ID != q.ExpectedDisk.ID {
			return result, errors.New("offload disk no longer matches the requested incarnation")
		}
	}
	var previousBase storage.SubvolumeIdentity
	var previousBaseName string
	if r != nil && q.Action == "offload" && r.Phase == "hot" {
		if err := s.recoverLocalCold(ctx, q, r); err != nil {
			return result, err
		}
		previousBase, previousBaseName = r.Base, r.BaseName
		if err := s.purgeCold(ctx, q, r); err != nil {
			return result, err
		}
		r = nil
	}
	if r != nil && r.Phase == "purged" {
		if q.Action != "delete" {
			id, err := storage.ValidateDiskSubvolume(ctx, filepath.Join(dir, "disk"))
			if err != nil || id != r.Disk {
				return result, errors.New("purged generation has no valid local disk")
			}
		}
		if err := s.finishColdPurge(q); err != nil {
			return result, err
		}
		r = nil
	}
	if r == nil {
		if _, err := os.Lstat(s.coldMarker(q.Name)); !errors.Is(err, os.ErrNotExist) {
			return result, errors.New("cold marker without authoritative runner journal; operator inspection required")
		}
		if q.Action != "offload" {
			if err := os.Remove(stage); err != nil && !errors.Is(err, os.ErrNotExist) {
				return result, err
			}
			return ColdStatus{Phase: "hot"}, nil
		}
		if s.config.ColdStorage.Host == "" {
			return result, errors.New("cold storage is disabled")
		}
		id, err := storage.ValidateDiskSubvolume(ctx, filepath.Join(dir, "disk"))
		if err != nil {
			return result, err
		}
		if err := os.MkdirAll(s.coldDir(q.Name), 0700); err != nil {
			return result, err
		}
		if err := storage.SyncPath(s.receiptDir(q.Name)); err != nil {
			return result, err
		}
		r = &coldDiskRecord{Version: 1, InstanceID: q.InstanceID, Phase: "offloading", Host: s.config.ColdStorage.Host, Disk: id, Object: coldstore.Object{ID: uuid.NewString()}, Base: previousBase, BaseName: previousBaseName}
		if err := s.writeCold(q, r); err != nil {
			return result, err
		}
	}
	switch q.Action {
	case "offload":
		if r.Phase == "cold" {
			return ColdStatus{Phase: r.Phase, Remote: r.Remote, JournalPresent: true}, nil
		}
		if r.Phase != "offloading" {
			return result, errors.New("hydrate and purge previous cold generation before offloading again")
		}
		err = s.offloadCold(ctx, q, r)
		if err != nil {
			// Upload failure must not strand a still-valid local disk. Recovery
			// never needs the remote, even if the upload acknowledgement was lost.
			recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			err = errors.Join(err, s.recoverLocalCold(recoveryCtx, q, r))
			cancel()
		}
	case "hydrate":
		err = s.hydrateCold(ctx, q, r)
	case "purge", "delete":
		if q.Action == "purge" {
			if err := s.recoverLocalCold(ctx, q, r); err != nil {
				return result, errors.New("hydrate the cold disk before purging its remote copy")
			}
		}
		err = s.purgeCold(ctx, q, r)
		if err == nil {
			return ColdStatus{Phase: "hot"}, nil
		}
	}
	return ColdStatus{Phase: r.Phase, Remote: r.Remote, JournalPresent: true}, err
}

var coldPrivateDir = func(path string, private bool) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	mask := os.FileMode(0022)
	if private {
		mask = 0077
	}
	if !st.IsDir() || st.Mode().Perm()&mask != 0 || st.Sys().(*syscall.Stat_t).Uid != 0 {
		return fmt.Errorf("cold staging %s must be a root-owned private directory", path)
	}
	return nil
}

var coldNoLoop = func(ctx context.Context, path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "losetup", "-j", path, "--output", "NAME", "--noheadings").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("cold storage requires an unattached disk (loop inspection: %v)", err)
	}
	return nil
}

func (s *Server) coldRemote(r *coldDiskRecord) (coldRemote, error) {
	if s.config.ColdStorage.Host == "" || r.Host != s.config.ColdStorage.Host {
		return nil, errors.New("restore the original cold-storage destination configuration before accessing this generation")
	}
	return newColdRemote(s.config.ColdStorage), nil
}

func coldObject(ctx context.Context, id, path string) (coldstore.Object, error) {
	identity, err := storage.ValidateDiskSubvolume(ctx, path)
	if err != nil {
		return coldstore.Object{}, err
	}
	if err := storage.RequireReadonly(ctx, path); err != nil {
		return coldstore.Object{}, err
	}
	f, err := os.Open(filepath.Join(path, "rootfs.img"))
	if err != nil {
		return coldstore.Object{}, err
	}
	defer f.Close()
	h := sha256.New()
	var size int64
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return coldstore.Object{}, err
		}
		n, err := f.Read(buf)
		_, _ = h.Write(buf[:n])
		size += int64(n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return coldstore.Object{}, err
		}
	}
	return coldstore.Object{ID: id, UUID: identity.UUID, SHA256: fmt.Sprintf("%x", h.Sum(nil)), Size: size}, nil
}

func (s *Server) findColdBase(ctx context.Context, parentUUID string) (string, storage.SubvolumeIdentity, error) {
	root := filepath.Join(filepath.Dir(s.config.InstancesDir), "image-bases")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return "", storage.SubvolumeIdentity{}, nil
	}
	if err != nil {
		return "", storage.SubvolumeIdentity{}, err
	}
	for _, entry := range entries {
		if len(entry.Name()) != 64 || strings.Trim(entry.Name(), "0123456789abcdef") != "" {
			continue
		}
		path := filepath.Join(root, entry.Name(), "disk")
		id, err := storage.ValidateDiskSubvolume(ctx, path)
		if err != nil {
			if ctx.Err() != nil {
				return "", storage.SubvolumeIdentity{}, ctx.Err()
			}
			continue // Incremental bases are optional; use a full send if unavailable.
		}
		if id.UUID != parentUUID {
			continue
		}
		if err := storage.RequireReadonly(ctx, path); err != nil {
			continue
		}
		return entry.Name(), id, nil
	}
	return "", storage.SubvolumeIdentity{}, nil
}

func (s *Server) basePath(r *coldDiskRecord) string {
	return filepath.Join(filepath.Dir(s.config.InstancesDir), "image-bases", r.BaseName, "disk")
}

func (s *Server) offloadCold(ctx context.Context, q ColdRequest, r *coldDiskRecord) error {
	remote, err := s.coldRemote(r)
	if err != nil {
		return err
	}
	disk := filepath.Join(s.config.InstancesDir, q.Name, "disk")
	actual, err := storage.ValidateDiskSubvolume(ctx, disk)
	if err != nil || actual != r.Disk {
		return errors.New("local disk identity changed before offload")
	}
	send := filepath.Join(s.coldStageDir(q.Name), "send")
	// A retry cannot reuse a prior snapshot after the disk might have changed.
	// Require hydration/purge if a transfer could already have reached remote.
	if r.Remote {
		return errors.New("previous upload outcome requires hydrate and purge before retry")
	}
	if err := storage.RemoveTree(ctx, send); err != nil {
		return err
	}
	if err := storage.SnapshotSubvolume(ctx, disk, send, true); err != nil {
		return err
	}
	r.Object, err = coldObject(ctx, r.Object.ID, send)
	if err != nil {
		return err
	}
	if r.BaseName != "" {
		id, err := storage.ValidateDiskSubvolume(ctx, s.basePath(r))
		if err != nil || id != r.Base || storage.RequireReadonly(ctx, s.basePath(r)) != nil {
			r.BaseName, r.Base = "", storage.SubvolumeIdentity{}
		}
	}
	if r.BaseName == "" {
		r.BaseName, r.Base, err = s.findColdBase(ctx, r.Disk.ParentUUID)
		if err != nil {
			return err
		}
	}
	parent := ""
	if r.BaseName != "" {
		parent = s.basePath(r)
		base, err := coldObject(ctx, r.Base.UUID, parent)
		if err != nil {
			return err
		}
		if err := remote.Put(ctx, base, parent, ""); err != nil {
			return err
		}
		r.Object.Parent = base.ID
	}
	// Conservatively track the object before making the remote call: a lost
	// reply is never evidence that a remotely published generation is absent.
	r.Remote = true
	if err := s.writeCold(q, r); err != nil {
		return err
	}
	if err := remote.Put(ctx, r.Object, send, parent); err != nil {
		return err
	}
	r.Phase = "evicting"
	if err := s.writeCold(q, r); err != nil {
		return err
	}
	if err := storage.DeleteSubvolume(ctx, disk, r.Disk); err != nil {
		return err
	}
	if err := storage.RemoveTree(ctx, send); err != nil {
		return err
	}
	r.Phase = "cold"
	return s.writeCold(q, r)
}

func (s *Server) cleanColdStages(ctx context.Context, q ColdRequest) error {
	for _, name := range []string{"send", "receive", "candidate"} {
		if err := storage.RemoveTree(ctx, filepath.Join(s.coldStageDir(q.Name), name)); err != nil {
			return err
		}
	}
	return storage.SyncPath(s.coldStageDir(q.Name))
}

func (s *Server) recoverLocalCold(ctx context.Context, q ColdRequest, r *coldDiskRecord) error {
	path := filepath.Join(s.config.InstancesDir, q.Name, "disk")
	id, err := storage.ValidateDiskSubvolume(ctx, path)
	if err != nil {
		return err
	}
	if id != r.Disk && (r.Candidate.UUID == "" || id != r.Candidate) {
		return errors.New("unrecognized local disk during cold recovery")
	}
	// Publish local authority before cleaning any stage. A hot generation may
	// never fall back to the remote copy after the guest has modified it.
	r.Disk, r.Phase = id, "hot"
	if err := s.writeCold(q, r); err != nil {
		return err
	}
	return s.cleanColdStages(ctx, q)
}

func (s *Server) hydrateCold(ctx context.Context, q ColdRequest, r *coldDiskRecord) error {
	disk := filepath.Join(s.config.InstancesDir, q.Name, "disk")
	if _, err := os.Lstat(disk); err == nil {
		if r.Phase == "cold" {
			return errors.New("unexpected disk alongside cold generation")
		}
		return s.recoverLocalCold(ctx, q, r)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if r.Phase == "hot" || r.Phase == "offloading" || !r.Remote {
		return errors.New("authoritative local disk is missing; refusing stale remote fallback")
	}
	remote, err := s.coldRemote(r)
	if err != nil {
		return err
	}
	candidate := filepath.Join(s.coldStageDir(q.Name), "candidate")
	if r.Candidate.UUID != "" {
		id, err := storage.ValidateDiskSubvolume(ctx, candidate)
		if err != nil || id != r.Candidate {
			return errors.New("cold restore candidate identity mismatch")
		}
	} else {
		if err := storage.CheckDiskSpace(s.coldStageDir(q.Name), r.Object.Size); err != nil {
			return err
		}
		r.Phase = "restoring"
		if err := s.writeCold(q, r); err != nil {
			return err
		}
		if err := s.cleanColdStages(ctx, q); err != nil {
			return err
		}
		receive := filepath.Join(s.coldStageDir(q.Name), "receive")
		if err := os.Mkdir(receive, 0700); err != nil {
			return err
		}
		parentID := ""
		if r.BaseName != "" {
			id, err := storage.ValidateDiskSubvolume(ctx, s.basePath(r))
			if err == nil && id == r.Base && storage.RequireReadonly(ctx, s.basePath(r)) == nil {
				parentID = r.Object.Parent
			}
		}
		received, err := remote.Get(ctx, r.Object, receive, parentID)
		if err != nil {
			return err
		}
		if filepath.Dir(received) != filepath.Join(receive, r.Object.ID) {
			return errors.New("received disk escaped private staging directory")
		}
		if err := storage.SnapshotSubvolume(ctx, received, candidate, false); err != nil {
			return err
		}
		r.Candidate, err = storage.ValidateDiskSubvolume(ctx, candidate)
		if err != nil {
			return err
		}
		if err := s.writeCold(q, r); err != nil {
			return err
		}
	}
	if err := unix.Renameat2(unix.AT_FDCWD, candidate, unix.AT_FDCWD, disk, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if err := storage.SyncPath(filepath.Dir(disk)); err != nil {
		return err
	}
	return s.recoverLocalCold(ctx, q, r)
}

func (s *Server) purgeCold(ctx context.Context, q ColdRequest, r *coldDiskRecord) error {
	if r.Remote {
		remote, err := s.coldRemote(r)
		if err != nil {
			return err
		}
		if err := remote.Delete(ctx, r.Object.ID); err != nil {
			return err
		}
		r.Remote = false
		if err := s.writeCold(q, r); err != nil {
			return err
		}
	}
	if err := s.cleanColdStages(ctx, q); err != nil {
		return err
	}
	r.Phase = "purged"
	if err := s.writeCold(q, r); err != nil {
		return err
	}
	return s.finishColdPurge(q)
}

func (s *Server) finishColdPurge(q ColdRequest) error {
	// The durable purged phase permits retry after either unlink. No remote
	// object or temporary disk survives this phase.
	if err := os.Remove(s.coldStageDir(q.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := storage.SyncPath(filepath.Dir(s.coldStageDir(q.Name))); err != nil {
		return err
	}
	if err := os.Remove(s.coldMarker(q.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := storage.SyncPath(filepath.Join(s.config.InstancesDir, q.Name)); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.coldDir(q.Name), "state")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return storage.SyncPath(s.coldDir(q.Name))
}

// Called with the same flock held by transfer operations, including on late
// start requests whose service-side lifecycle lock has already been released.
func (s *Server) coldStartFence(q ColdRequest) error {
	if err := s.coldDeletionFence(q); err != nil {
		return err
	}
	r, err := s.readCold(q)
	if err != nil {
		return err
	}
	if r == nil {
		return nil
	}
	if r.Phase != "hot" && r.Phase != "purged" {
		return errors.New("disk requires hydration before start")
	}
	id, err := storage.ValidateDiskSubvolume(context.Background(), filepath.Join(s.config.InstancesDir, q.Name, "disk"))
	if err != nil || id != r.Disk {
		return errors.New("authoritative hot disk is missing or changed")
	}
	return nil
}

func (s *Server) coldDeletedPath(q ColdRequest) string {
	return filepath.Join(s.receiptDir(q.Name), "cold-deleted-"+receiptKey(q.InstanceID))
}

func (s *Server) coldDeletionFence(q ColdRequest) error {
	_, err := os.Lstat(s.coldDeletedPath(q))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return errors.New("disk incarnation has deletion intent; retry delete, not offload or start")
}
