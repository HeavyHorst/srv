package storage

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type inheritedLocksKey struct{}

// WithInheritedLock adds a held flock to storage subprocesses launched with ctx.
// The caller owns f and must keep it open until those subprocesses have started.
// Release by closing, never LOCK_UN: children share the open file description.
func WithInheritedLock(ctx context.Context, f *os.File) context.Context {
	if f == nil {
		return ctx
	}
	old, _ := ctx.Value(inheritedLocksKey{}).([]*os.File)
	locks := append([]*os.File(nil), old...)
	for _, held := range locks {
		if held == f {
			return ctx
		}
	}
	return context.WithValue(ctx, inheritedLocksKey{}, append(locks, f))
}

// InheritLocks pins held locks in cmd until the last inheriting child exits.
// Only storage commands should opt in; VM processes must not inherit these fds.
func InheritLocks(ctx context.Context, cmd *exec.Cmd) {
	locks, _ := ctx.Value(inheritedLocksKey{}).([]*os.File)
	for _, f := range locks {
		found := false
		for _, existing := range cmd.ExtraFiles {
			if existing == f {
				found = true
				break
			}
		}
		if !found {
			cmd.ExtraFiles = append(cmd.ExtraFiles, f)
		}
	}
}

// AcquirePayload serializes host payload transfers. The installer must precreate
// dataDir/.host-locks/payload.lock as root:<service-group> 0660; runtime never
// creates it. The kernel enforces membership in the administrator-chosen group.
// The variable is a test seam, like the other storage/kernel operations.
var AcquirePayload = func(ctx context.Context, dataDir string) (context.Context, func(), error) {
	return acquirePayload(ctx, dataDir, -1)
}

// Tests may additionally require a specific group without host account changes.
// Ownership remains strictly root-only in this helper and the public entry point.
func acquirePayload(ctx context.Context, dataDir string, gid int) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	if !filepath.IsAbs(dataDir) || filepath.Clean(dataDir) != dataDir {
		return ctx, nil, errors.New("payload data directory must be absolute and clean")
	}
	// Pin each component and validate the whole trust chain, not just the leaf:
	// a writable ancestor could otherwise replace the namespace with a new lock.
	dir, err := OpenNoSymlinks("/", unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return ctx, nil, err
	}
	defer func() { _ = dir.Close() }()
	parts := []string{}
	for p := dataDir; p != "/"; p = filepath.Dir(p) {
		parts = append(parts, filepath.Base(p))
	}
	for i := len(parts); i >= 0; i-- {
		if i < len(parts) {
			fd, err := unix.Openat(int(dir.Fd()), parts[i], unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return ctx, nil, err
			}
			_ = dir.Close()
			dir = os.NewFile(uintptr(fd), parts[i])
		}
		if err := payloadDirectory(dir); err != nil {
			return ctx, nil, err
		}
	}
	fd, err := unix.Openat(int(dir.Fd()), ".host-locks", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ctx, nil, err
	}
	locks := os.NewFile(uintptr(fd), ".host-locks")
	defer locks.Close()
	if err := payloadDirectory(locks); err != nil {
		return ctx, nil, err
	}
	fd, err = unix.Openat(fd, "payload.lock", unix.O_RDWR|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ctx, nil, err
	}
	f := os.NewFile(uintptr(fd), "payload.lock")
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = f.Close()
		return ctx, nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Uid != 0 || (gid >= 0 && int(st.Gid) != gid) || st.Mode&07777 != 0660 {
		_ = f.Close()
		return ctx, nil, errors.New("payload lock must be a single-link regular file owned by root with mode 0660 and the configured service group")
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return ctx, nil, err
		}
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return WithInheritedLock(ctx, f), func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = f.Close()
			return ctx, nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return ctx, nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func payloadDirectory(f *os.File) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != 0 || st.Mode&0022 != 0 {
		return errors.New("payload lock ancestors must be root-owned directories not writable by group or others")
	}
	return nil
}
