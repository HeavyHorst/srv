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
	"path/filepath"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"golang.org/x/sys/unix"
	"srv/internal/storage"
)

// A suspend image belongs to one incarnation and one stopped attempt. It is
// never a template or a backup: the disk must remain exactly as it was paused.
type ramState struct {
	Attempt StartResultRequest
	Config  string
	Disk    fileStamp
	Binary  string
}

type fileStamp struct {
	Inode uint64
	Size  int64
	Mtime unix.Timespec
}

func stampFile(path string) (fileStamp, error) {
	f, err := storage.OpenNoSymlinks(path, unix.O_RDONLY)
	if err != nil {
		return fileStamp{}, err
	}
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return fileStamp{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fileStamp{}, fmt.Errorf("not a regular file: %s", path)
	}
	return fileStamp{st.Ino, st.Size, st.Mtim}, nil
}

func resumeConfig(req StartRequest) string {
	req.RequestID, req.HostBootID = "", ""
	req.Bootstrap = Bootstrap{}
	req.DiscardState = false
	b, _ := json.Marshal(req)
	return receiptKey(string(b))
}

func (s *Server) ramStateDir(name string) string {
	return filepath.Join(s.config.InstancesDir, name, "ram-state")
}

func (s *Server) readRAMState(name string) (*ramState, error) {
	dir := s.ramStateDir(name)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := storage.ValidatePath(filepath.Join(dir, "state.json")); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, fmt.Errorf("incomplete RAM state: %w", err)
	}
	var state ramState
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, err
	}
	if state.Attempt.Name != name || state.Attempt.Validate() != nil || state.Config == "" {
		return nil, errors.New("invalid RAM state identity")
	}
	return &state, nil
}

func (s *Server) discardRAMState(name string) error {
	dir := s.ramStateDir(name)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := storage.ValidatePath(dir); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(dir))
}

// Probe the actual rename boundary, including distinct mounts of one device
// and Btrfs subvolumes. Reject unsupported layouts before recording save intent.
func (s *Server) checkRAMStateLayout(name string) error {
	jail, err := resolveJailerRuntimePaths(s.config.JailerBaseDir, s.config.FirecrackerBinary, name)
	if err != nil {
		return err
	}
	source, err := os.CreateTemp(jail.RootDir, ".ram-layout-")
	if err != nil {
		return err
	}
	defer os.Remove(source.Name())
	if err := source.Close(); err != nil {
		return err
	}
	target, err := os.CreateTemp(filepath.Join(s.config.InstancesDir, name), ".ram-layout-")
	if err != nil {
		return err
	}
	defer os.Remove(target.Name())
	if err := target.Close(); err != nil {
		return err
	}
	if err := os.Rename(source.Name(), target.Name()); err != nil {
		return fmt.Errorf("RAM state requires jailer and instance directories on the same rename-compatible filesystem/subvolume: %w", err)
	}
	return nil
}

// Takes ownership of an already opened file. /proc/PID/exe deliberately follows
// the kernel's link to the verified VMM, not the possibly replaced installation.
func ramBinaryDigest(f *os.File) (string, error) {
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// Called while holding the lifecycle lock and a verified pidfd. Persist the
// complete image before killing the paused VM; retries never resume the source.
func (s *Server) saveRAMState(ctx context.Context, r startReceipt) error {
	// Keep the lifecycle lock across HTTP disconnects while the VMM writes RAM.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	if r.ResumeConfig == "" {
		return errors.New("VM predates RAM-state support; cold-start it before suspending")
	}
	state, err := s.readRAMState(r.Name)
	if err != nil {
		return err
	}
	if state != nil {
		if state.Attempt != r.StartResultRequest {
			return errors.New("RAM state belongs to another attempt")
		}
		// Retry a failed directory sync before acknowledging the saved image.
		return syncDirectory(filepath.Dir(s.ramStateDir(r.Name)))
	}
	jail, err := resolveJailerRuntimePaths(s.config.JailerBaseDir, s.config.FirecrackerBinary, r.Name)
	if err != nil {
		return err
	}
	stage := filepath.Join(jail.RootDir, "ram-save")
	socket := filepath.Join(s.config.InstancesDir, r.Name, "firecracker.sock")
	var cfg struct {
		MemoryMiB int64 `json:"mem_size_mib"`
	}
	// Firecracker's API is synchronous. Wait for a timed-out snapshot/create
	// to finish before reclaiming its unpublished output files on retry.
	if err := s.doFirecrackerAPIRequest(ctx, socket, http.MethodGet, "/machine-config", nil, http.StatusOK, &cfg); err != nil {
		return err
	}
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := os.Mkdir(stage, 0700); err != nil {
		return err
	}
	if err := os.Chown(stage, s.config.JailerUID, s.config.JailerGID); err != nil {
		return err
	}
	var space unix.Statfs_t
	if err := unix.Statfs(stage, &space); err != nil {
		return err
	}
	if cfg.MemoryMiB <= 0 || uint64(cfg.MemoryMiB+256)*uint64(miBBytes) > space.Bavail*uint64(space.Bsize) {
		return errors.New("insufficient disk space for full RAM state plus 256 MiB headroom")
	}
	if err := s.doFirecrackerAPIRequest(ctx, socket, http.MethodPatch, "/vm", map[string]string{"state": "Paused"}, http.StatusNoContent, nil); err != nil {
		return err
	}
	if err := s.doFirecrackerAPIRequest(ctx, socket, http.MethodPut, "/snapshot/create", map[string]string{
		"snapshot_type": "Full", "snapshot_path": "ram-save/vmstate", "mem_file_path": "ram-save/memory",
	}, http.StatusNoContent, nil); err != nil {
		return fmt.Errorf("VM remains paused; retry stop to finish saving RAM: %w", err)
	}
	state = &ramState{Attempt: r.StartResultRequest, Config: r.ResumeConfig}
	state.Disk, err = stampFile(filepath.Join(s.config.InstancesDir, r.Name, "disk", "rootfs.img"))
	if err != nil {
		return err
	}
	runningBinary, err := os.Open(fmt.Sprintf("/proc/%d/exe", r.PID))
	if err != nil {
		return err
	}
	state.Binary, err = ramBinaryDigest(runningBinary)
	if err != nil {
		return err
	}
	for _, name := range []string{"vmstate", "memory"} {
		path := filepath.Join(stage, name)
		f, err := storage.OpenNoSymlinks(path, unix.O_RDONLY)
		if err != nil {
			return err
		}
		err = errors.Join(f.Chmod(0600), f.Sync(), f.Close())
		if err != nil {
			return err
		}
	}
	if err := atomicColdJSON(filepath.Join(stage, "state.json"), state, 0600); err != nil {
		return err
	}
	if err := os.Rename(stage, s.ramStateDir(r.Name)); err != nil {
		return err
	}
	return errors.Join(syncDirectory(jail.RootDir), syncDirectory(filepath.Join(s.config.InstancesDir, r.Name)))
}

func (s *Server) checkRAMState(req StartRequest) (*ramState, error) {
	if req.DiscardState {
		return nil, s.discardRAMState(req.Name)
	}
	state, err := s.readRAMState(req.Name)
	if err != nil || state == nil {
		return state, err
	}
	if state.Attempt.InstanceID != req.InstanceID || state.Config != resumeConfig(req) {
		return nil, errors.New("RAM state does not match VM configuration; use start --discard-state")
	}
	r, err := s.readReceipt(state.Attempt)
	if err != nil {
		return nil, err
	}
	if r == nil || r.State != "closed" || !r.SaveState {
		return nil, errors.New("RAM state source has not finished stopping")
	}
	disk, err := stampFile(filepath.Join(s.config.InstancesDir, req.Name, "disk", "rootfs.img"))
	if err != nil {
		return nil, err
	}
	f, err := storage.OpenNoSymlinks(s.config.FirecrackerBinary, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	binary, err := ramBinaryDigest(f)
	if err != nil {
		return nil, err
	}
	if disk != state.Disk || binary != state.Binary {
		return nil, errors.New("disk or Firecracker changed since suspend; use start --discard-state")
	}
	return state, nil
}

// Consume before any guest instruction can run. The accepted start receipt
// fences uncertain launches; a failed restore must never replay the same RAM.
func (s *Server) configureRAMRestore(m *firecracker.Machine, req StartRequest, jail jailerRuntimePaths) error {
	if err := os.MkdirAll(jail.RootDir, 0755); err != nil {
		return err
	}
	stage := filepath.Join(jail.RootDir, "ram-resume")
	if err := os.Rename(s.ramStateDir(req.Name), stage); err != nil {
		return err
	}
	if err := errors.Join(syncDirectory(jail.RootDir), syncDirectory(filepath.Join(s.config.InstancesDir, req.Name))); err != nil {
		return err
	}
	// Suppress InstanceStart: snapshot/load creates the device backends instead.
	m.Cfg.Snapshot.SnapshotPath = "ram-resume/vmstate"
	m.Handlers.FcInit = m.Handlers.FcInit.Remove(firecracker.CreateMachineHandlerName).
		Remove(firecracker.CreateBootSourceHandlerName).Remove(firecracker.AttachDrivesHandlerName).
		Remove(firecracker.CreateNetworkInterfacesHandlerName).Remove(firecracker.AddVsocksHandlerName).
		Remove(firecracker.ConfigMmdsHandlerName)
	m.Handlers.FcInit = m.Handlers.FcInit.Append(firecracker.Handler{Name: "srv.LoadRAMState", Fn: func(ctx context.Context, _ *firecracker.Machine) error {
		socket := filepath.Join(s.config.InstancesDir, req.Name, "firecracker.sock")
		if err := s.doFirecrackerAPIRequest(ctx, socket, http.MethodPut, "/snapshot/load", map[string]any{
			"snapshot_path": "ram-resume/vmstate", "mem_backend": map[string]string{"backend_type": "File", "backend_path": "ram-resume/memory"}, "resume_vm": false,
		}, http.StatusNoContent, nil); err != nil {
			return err
		}
		// Unlink, never truncate: Firecracker retains a private mmap of memory.
		if err := os.RemoveAll(stage); err != nil {
			return err
		}
		return syncDirectory(jail.RootDir)
	}})
	return nil
}
