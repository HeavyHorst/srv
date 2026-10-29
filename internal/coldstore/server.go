package coldstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"srv/internal/storage"
)

func secureDir(path string) error {
	if err := storage.ValidatePath(path); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Sys().(*syscall.Stat_t).Uid != 0 || st.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("%s must be a root-owned directory not writable by group or others", path)
	}
	return nil
}

// A trusted leaf is insufficient when an ancestor can rename or replace it.
func trustedRoot(path string, lstat func(string) (os.FileInfo, error)) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("storage root must be absolute and clean")
	}
	for p := path; ; p = filepath.Dir(p) {
		st, err := lstat(p)
		if err != nil {
			return err
		}
		raw, ok := st.Sys().(*syscall.Stat_t)
		if !ok || raw.Uid != 0 || !st.IsDir() || st.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("storage ancestor %s must be a root-owned nonsymlink directory not writable by group or others", p)
		}
		if p == "/" {
			return nil
		}
	}
}

func ordinaryDir(path string) error {
	if err := secureDir(path); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Sys().(*syscall.Stat_t).Ino == 256 {
		return errors.New("object container must be an ordinary directory")
	}
	return nil
}

func lock(ctx context.Context, path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Sys().(*syscall.Stat_t).Uid != 0 || st.Mode().Perm()&0022 != 0 {
		f.Close()
		return nil, errors.New("unsafe lock file")
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Serve executes one forced command. root is configuration supplied by the
// administrator, never by the peer. The entire operation holds an exclusive
// lock, including streaming, so sends and incremental receives pin parents.
func Serve(ctx context.Context, root, command string, in io.Reader, out io.Writer) error {
	r, err := parseRequest(command)
	if err != nil {
		return err
	}
	return withServer(ctx, root, func(s server) error {
		switch r.Operation {
		case "stat":
			o, err := s.stat(ctx, r.Object.ID)
			if err != nil {
				return err
			}
			return json.NewEncoder(out).Encode(o)
		case "put":
			return s.put(ctx, r.Object, in)
		case "get":
			return s.get(ctx, r.Object.ID, r.Parent, out)
		case "delete":
			return s.delete(ctx, r.Object.ID)
		}
		return errors.New("invalid operation")
	})
}

func withServer(ctx context.Context, root string, fn func(server) error) error {
	if err := ValidateRoot(root); err != nil {
		return err
	}
	l, err := lock(ctx, filepath.Join(root, ".lock"))
	if err != nil {
		return err
	}
	defer l.Close()
	objects := filepath.Join(root, "objects")
	if err := os.Mkdir(objects, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := ordinaryDir(objects); err != nil {
		return err
	}
	s := server{objects: objects, root: root, namespaceLock: l}
	if err := s.recoverStages(ctx); err != nil {
		return err
	}
	return fn(s)
}

type server struct {
	objects, root string
	namespaceLock *os.File
	// Optional test hook for simulating publication failures such as ENOSPC.
	syncDeletionPath func(string) error
}

func (s server) path(id string) string { return filepath.Join(s.objects, id) }

var errObjectDeleted = errors.New("object ID is permanently deleted")

// deleted records are permanent anti-resurrection fences, not cleanup stages.
// An empty file is the complete record: its UUID filename is all the metadata.
// O_EXCL creation is atomic, and retry fsyncs an existing record as well, so a
// crash or ENOSPC between create and sync cannot produce a false Delete ack.
// All callers hold the namespace flock.
func (s server) deletionRecord(id string, create bool) (bool, error) {
	dir := filepath.Join(s.root, "deleted")
	if create {
		if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return false, err
		}
	}
	if err := ordinaryDir(dir); err != nil {
		if !create && errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	path := filepath.Join(dir, id)
	if create {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return false, err
		}
		if err == nil {
			if err := f.Close(); err != nil {
				return false, err
			}
		}
	}
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && !create {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Size() != 0 || st.Sys().(*syscall.Stat_t).Uid != 0 || st.Sys().(*syscall.Stat_t).Nlink != 1 {
		return false, errors.New("invalid permanent deletion record")
	}
	if create {
		syncPath := s.syncDeletionPath
		if syncPath == nil {
			syncPath = storage.SyncPath
		}
		for _, p := range []string{path, dir, s.root} {
			if err := syncPath(p); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// Caller holds the namespace lock. Validate all stages before removing any.
// Sync both rename parents before destructive cleanup, including recovery from
// a crash between the tombstone rename and its original directory fsyncs.
func (s server) recoverStages(ctx context.Context) error {
	ctx = storage.WithInheritedLock(ctx, s.namespaceLock)
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	var stages []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".stage-") {
			continue
		}
		id := strings.TrimPrefix(e.Name(), ".stage-put-")
		if id == e.Name() {
			id = strings.TrimPrefix(e.Name(), ".stage-delete-")
		}
		if !validID(id) {
			return errors.New("foreign staging namespace entry")
		}
		p := filepath.Join(s.root, e.Name())
		if err := ordinaryDir(p); err != nil {
			return err
		}
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if st.Mode().Perm() != 0700 {
			return errors.New("stage must be private mode 0700")
		}
		stages = append(stages, p)
	}
	if err := errors.Join(storage.SyncPath(s.objects), storage.SyncPath(s.root)); err != nil {
		return err
	}
	for _, p := range stages {
		if id, ok := strings.CutPrefix(filepath.Base(p), ".stage-delete-"); ok {
			if _, err := s.deletionRecord(id, true); err != nil {
				return err
			}
		}
		if err := storage.RemoveTree(ctx, p); err != nil {
			return fmt.Errorf("clean interrupted operation: %w", err)
		}
	}
	return storage.SyncPath(s.root)
}

func (s server) manifest(id string) (Object, error) {
	var o Object
	p := s.path(id)
	if err := ordinaryDir(p); err != nil {
		return o, err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return o, err
	}
	if len(entries) != 2 || entries[0].Name() != "disk" || entries[1].Name() != "manifest.json" {
		return o, errors.New("invalid object layout")
	}
	name := filepath.Join(p, "manifest.json")
	st, err := os.Lstat(name)
	if err != nil {
		return o, err
	}
	if !st.Mode().IsRegular() || st.Sys().(*syscall.Stat_t).Uid != 0 || st.Mode().Perm()&0022 != 0 {
		return o, errors.New("unsafe manifest")
	}
	f, err := os.Open(name)
	if err != nil {
		return o, err
	}
	defer f.Close()
	if err := decode(f, &o); err != nil {
		return o, err
	}
	if err := o.validate(); err != nil {
		return o, err
	}
	if o.ID != id {
		return o, errors.New("manifest ID mismatch")
	}
	return o, nil
}

func (s server) stat(ctx context.Context, id string) (Object, error) {
	ctx = storage.WithInheritedLock(ctx, s.namespaceLock)
	if deleted, err := s.deletionRecord(id, false); err != nil {
		return Object{}, err
	} else if deleted {
		return Object{}, errObjectDeleted
	}
	o, err := s.manifest(id)
	if err != nil {
		return o, err
	}
	return o, verifyDiskIdentity(ctx, filepath.Join(s.path(id), "disk"), o, true)
}

func (s server) put(ctx context.Context, o Object, in io.Reader) error {
	ctx = storage.WithInheritedLock(ctx, s.namespaceLock)
	if deleted, err := s.deletionRecord(o.ID, false); err != nil {
		return err
	} else if deleted {
		return errObjectDeleted
	}
	if _, err := os.Lstat(s.path(o.ID)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("object exists or cannot be inspected; refusing overwrite")
	}
	if o.Parent != "" {
		if _, err := s.stat(ctx, o.Parent); err != nil {
			return fmt.Errorf("parent: %w", err)
		}
	}
	stage := filepath.Join(s.root, ".stage-put-"+uuid.NewString())
	if err := os.Mkdir(stage, 0700); err != nil {
		return err
	}
	defer func() {
		// Cleanup survives transfer cancellation but retains the held locks.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_ = storage.RemoveTree(cleanupCtx, stage)
	}()
	if err := run(ctx, "btrfs", []string{"receive", stage}, in, io.Discard); err != nil {
		return err
	}
	disk, err := receivedDisk(ctx, stage, o)
	if err != nil {
		return err
	}
	if disk != filepath.Join(stage, "disk") {
		if err := os.Rename(disk, filepath.Join(stage, "disk")); err != nil {
			return err
		}
	}
	if err := storage.SyncPath(filepath.Join(stage, "disk", "rootfs.img")); err != nil {
		return err
	}
	if err := storage.SyncPath(filepath.Join(stage, "disk")); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(stage, "manifest.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(o)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := storage.SyncPath(stage); err != nil {
		return err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, s.path(o.ID), unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	// Publication is the point of no return. Any following sync/SSH failure is
	// ambiguous to the caller and must never roll back the immutable object.
	return errors.Join(storage.SyncPath(s.objects), storage.SyncPath(s.root))
}

func (s server) get(ctx context.Context, id, parent string, out io.Writer) error {
	ctx = storage.WithInheritedLock(ctx, s.namespaceLock)
	if _, err := s.stat(ctx, id); err != nil {
		return err
	}
	parentPath := ""
	if parent != "" {
		if _, err := s.stat(ctx, parent); err == nil {
			parentPath = filepath.Join(s.path(parent), "disk")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return run(ctx, "btrfs", sendArgs(filepath.Join(s.path(id), "disk"), parentPath), nil, out)
}

func (s server) delete(ctx context.Context, id string) error {
	ctx = storage.WithInheritedLock(ctx, s.namespaceLock)
	if err := s.recoverStages(ctx); err != nil {
		return err
	}
	if _, err := os.Lstat(s.path(id)); errors.Is(err, os.ErrNotExist) {
		_, err := s.deletionRecord(id, true)
		return err
	} else if err != nil {
		return err
	}
	// A previous attempt may have fenced the ID before it renamed the disk.
	// Validate the physical object directly rather than rejecting our own fence.
	o, err := s.manifest(id)
	if err != nil {
		return err
	}
	if err := verifyDiskIdentity(ctx, filepath.Join(s.path(id), "disk"), o, true); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.objects)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !validID(e.Name()) {
			return errors.New("unexpected object namespace entry")
		}
		o, err := s.manifest(e.Name())
		if err != nil {
			return err
		}
		if o.Parent == id {
			return errors.New("object is referenced by a child")
		}
	}
	stage := filepath.Join(s.root, ".stage-delete-"+id)
	st, err := os.Lstat(s.path(id))
	if err != nil {
		return err
	}
	if st.Mode().Perm() != 0700 {
		return errors.New("object container must be private mode 0700")
	}
	if _, err := s.deletionRecord(id, true); err != nil {
		return err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, s.path(id), unix.AT_FDCWD, stage, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	// No removal before the rename is durable. Failures leave a recoverable
	// tombstone, never a partially destroyed published object.
	return s.recoverStages(ctx)
}
