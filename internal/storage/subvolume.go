package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// SubvolumeIdentity is stable across renames, but not across snapshots.
type SubvolumeIdentity struct {
	UUID         string `json:"uuid"`
	ID           string `json:"id"`
	ParentUUID   string `json:"parent_uuid,omitempty"`
	ReceivedUUID string `json:"received_uuid,omitempty"`
}

// Command is a test seam. Production operations never invoke sudo or acquire
// CAP_SYS_ADMIN; the storage mount must permit owner subvolume operations.
var Command = func(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "btrfs", args...)
	InheritLocks(ctx, cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("btrfs %v: %w: %s", args, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func ValidatePath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("storage path must be absolute and clean: %q", path)
	}
	for p := path; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("storage path contains symlink: %s", p)
		}
		if p == "/" {
			break
		}
	}
	return nil
}

var RequireBtrfs = func(path string) error {
	if err := ValidatePath(path); err != nil {
		return err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return err
	}
	if uint64(st.Type) != 0x9123683e {
		return fmt.Errorf("%s must be on btrfs", path)
	}
	return nil
}

// Layout from linux/btrfs.h; GET_SUBVOL_INFO is an unprivileged inode ioctl.
// `btrfs subvolume show` additionally performs a privileged TREE_SEARCH and
// therefore cannot be used by the srv service for identity checks.
type subvolumeInfo struct {
	TreeID       uint64
	Name         [256]byte
	ParentID     uint64
	DirID        uint64
	Generation   uint64
	Flags        uint64
	UUID         [16]byte
	ParentUUID   [16]byte
	ReceivedUUID [16]byte
	Transactions [4]uint64
	Times        [4]struct {
		Seconds uint64
		Nanos   uint32
		Padding uint32
	}
	Reserved [8]uint64
}

const getSubvolumeInfo = uintptr(0x80000000 | (unsafe.Sizeof(subvolumeInfo{}) << 16) | (0x94 << 8) | 60)

// OpenNoSymlinks opens a clean absolute path, pinning each directory
// before opening its child. Unlike openat2, openat works with systemd's
// RestrictSUIDSGID filter; O_NOFOLLOW still rejects symlinks at every component.
func OpenNoSymlinks(path string, flags int) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("storage path must be absolute and clean: %q", path)
	}
	if path == "/" {
		fd, err := unix.Open(path, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), path), nil
	}
	fd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(path[1:], "/")
	for i, part := range parts {
		openFlags := unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i == len(parts)-1 {
			openFlags = flags | unix.O_NOFOLLOW | unix.O_CLOEXEC
		}
		next, err := unix.Openat(fd, part, openFlags, 0)
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	// O_PATH|O_NOFOLLOW opens the symlink inode itself instead of rejecting it.
	if flags&unix.O_PATH != 0 {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			unix.Close(fd)
			return nil, err
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			unix.Close(fd)
			return nil, unix.ELOOP
		}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// FS_INFO's first 32 bytes identify the filesystem, independent of subvolume
// and mount aliases. The remaining ioctl fields are unused and zero-initialized.
type filesystemInfo struct {
	MaxID, NumDevices uint64
	UUID              [16]byte
	_                 [992]byte
}

const getFilesystemInfo = uintptr(0x80000000 | (unsafe.Sizeof(filesystemInfo{}) << 16) | (0x94 << 8) | 31)

var FilesystemUUID = func(path string) (string, error) {
	if err := RequireBtrfs(path); err != nil {
		return "", err
	}
	f, err := OpenNoSymlinks(path, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var info filesystemInfo
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), getFilesystemInfo, uintptr(unsafe.Pointer(&info))); errno != 0 {
		return "", fmt.Errorf("inspect btrfs filesystem containing %s: %w", path, errno)
	}
	return uuid.UUID(info.UUID).String(), nil
}

var ContainingSubvolume = func(ctx context.Context, path string) (SubvolumeIdentity, error) {
	if err := ctx.Err(); err != nil {
		return SubvolumeIdentity{}, err
	}
	if err := RequireBtrfs(path); err != nil {
		return SubvolumeIdentity{}, err
	}
	f, err := OpenNoSymlinks(path, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		return SubvolumeIdentity{}, err
	}
	defer f.Close()
	var info subvolumeInfo
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), getSubvolumeInfo, uintptr(unsafe.Pointer(&info))); errno != 0 {
		return SubvolumeIdentity{}, fmt.Errorf("inspect btrfs subvolume containing %s: %w", path, errno)
	}
	if info.TreeID == 0 || info.TreeID == 5 || info.UUID == [16]byte{} {
		return SubvolumeIdentity{}, errors.New("data must be in a dedicated btrfs subvolume with a persistent UUID, not the filesystem top-level tree")
	}
	id := SubvolumeIdentity{UUID: uuid.UUID(info.UUID).String(), ID: strconv.FormatUint(info.TreeID, 10)}
	if info.ParentUUID != [16]byte{} {
		id.ParentUUID = uuid.UUID(info.ParentUUID).String()
	}
	if info.ReceivedUUID != [16]byte{} {
		id.ReceivedUUID = uuid.UUID(info.ReceivedUUID).String()
	}
	return id, nil
}

var InspectSubvolume = func(ctx context.Context, path string) (SubvolumeIdentity, error) {
	if err := ValidatePath(path); err != nil {
		return SubvolumeIdentity{}, err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return SubvolumeIdentity{}, err
	}
	if !st.IsDir() || st.Sys().(*syscall.Stat_t).Ino != 256 {
		return SubvolumeIdentity{}, fmt.Errorf("%s is not a subvolume root", path)
	}
	return ContainingSubvolume(ctx, path)
}

var CreateSubvolume = func(ctx context.Context, path string) error {
	if err := ValidatePath(path); err != nil {
		return err
	}
	if err := RequireBtrfs(filepath.Dir(path)); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("subvolume destination exists or cannot be inspected: %s", path)
	}
	if _, err := Command(ctx, "subvolume", "create", path); err != nil {
		return err
	}
	return SyncPath(filepath.Dir(path))
}

var SnapshotSubvolume = func(ctx context.Context, source, dest string, readonly bool) error {
	if _, err := InspectSubvolume(ctx, source); err != nil {
		return err
	}
	if err := ValidatePath(dest); err != nil {
		return err
	}
	if err := RequireBtrfs(filepath.Dir(dest)); err != nil {
		return err
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("snapshot destination exists or cannot be inspected: %s", dest)
	}
	args := []string{"subvolume", "snapshot"}
	if readonly {
		args = append(args, "-r")
	}
	args = append(args, source, dest)
	if _, err := Command(ctx, args...); err != nil {
		return err
	}
	return SyncPath(filepath.Dir(dest))
}

// DeleteSubvolume requires an independently recorded identity. It does not
// recursively delete nested subvolumes, nor fall back to ordinary file removal.
var DeleteSubvolume = func(ctx context.Context, path string, expected SubvolumeIdentity) (retErr error) {
	actual, err := InspectSubvolume(ctx, path)
	if err != nil {
		return err
	}
	if expected.UUID == "" || actual.UUID != expected.UUID || actual.ID != expected.ID {
		return errors.New("subvolume identity changed; refusing deletion")
	}
	// user_subvol_rm_allowed still requires write access to the subvolume
	// itself. Only thaw a local snapshot already authorized for deletion;
	// received subvolumes must retain their send/receive identity.
	restoreReadonly := false
	defer func() {
		if restoreReadonly {
			// A failed command may already have changed the flag. Restore it
			// even if the caller canceled before receiving the command result.
			restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, restoreErr := Command(restoreCtx, "property", "set", "-ts", path, "ro", "true")
			retErr = errors.Join(retErr, restoreErr)
		}
	}()
	if os.Geteuid() != 0 && actual.ReceivedUUID == "" {
		out, err := Command(ctx, "property", "get", "-ts", path, "ro")
		if err != nil {
			return err
		}
		switch strings.TrimSpace(string(out)) {
		case "ro=true":
			restoreReadonly = true
			if _, err := Command(ctx, "property", "set", "-ts", path, "ro", "false"); err != nil {
				return err
			}
		case "ro=false":
		default:
			return fmt.Errorf("unexpected subvolume read-only property: %q", out)
		}
	}
	if _, err := Command(ctx, "subvolume", "delete", path); err != nil {
		return err
	}
	restoreReadonly = false
	return SyncPath(filepath.Dir(path))
}

func SyncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func ValidateDiskSubvolume(ctx context.Context, path string) (SubvolumeIdentity, error) {
	id, err := InspectSubvolume(ctx, path)
	if err != nil {
		return id, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return id, err
	}
	if len(entries) != 1 || entries[0].Name() != "rootfs.img" {
		return id, errors.New("disk subvolume must contain only rootfs.img")
	}
	st, err := os.Lstat(filepath.Join(path, "rootfs.img"))
	if err != nil {
		return id, err
	}
	if !st.Mode().IsRegular() || st.Size() <= 0 {
		return id, errors.New("disk rootfs must be a nonempty regular file")
	}
	return id, nil
}

var RequireReadonly = func(ctx context.Context, path string) error {
	if _, err := InspectSubvolume(ctx, path); err != nil {
		return err
	}
	out, err := Command(ctx, "property", "get", "-ts", path, "ro")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "ro=true" {
		return errors.New("immutable disk subvolume is not read-only")
	}
	return nil
}

// RemoveTree removes a caller-authorized tree including read-only subvolumes.
// Symlinks are unlinked, never traversed. Errors are never treated as absence.
func RemoveTree(ctx context.Context, path string) error {
	if err := ValidatePath(filepath.Dir(path)); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() {
		if err := os.Remove(path); err != nil {
			return err
		}
		return SyncPath(filepath.Dir(path))
	}
	if err := ValidatePath(path); err != nil {
		return err
	}
	// btrfs subvolume roots have inode 256. Ordinary directories must not be
	// mistaken for subvolumes when the command is unavailable or denied.
	if raw, ok := st.Sys().(*syscall.Stat_t); ok && raw.Ino == 256 {
		if err := RequireBtrfs(path); err != nil {
			return err
		}
		id, err := InspectSubvolume(ctx, path)
		if err != nil {
			return err
		}
		return DeleteSubvolume(ctx, path, id)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := RemoveTree(ctx, filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return SyncPath(filepath.Dir(path))
}
