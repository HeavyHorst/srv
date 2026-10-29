package coldstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
	"srv/internal/storage"
)

// BTRFS_IOC_SUBVOL_GETFLAGS reads the actual kernel read-only flag, without
// parsing locale-dependent CLI output.
func readonly(path string) error {
	f, err := storage.OpenNoSymlinks(path, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer f.Close()
	var flags uint64
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), 0x80089419, uintptr(unsafe.Pointer(&flags)))
	if errno != 0 {
		return errno
	}
	if flags&2 == 0 {
		return errors.New("disk subvolume is not read-only")
	}
	return nil
}

type contextReader struct {
	context.Context
	io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(b)
}

// Identity checks avoid scanning immutable disk bytes already verified on
// receive. Downloads and new receives still use verifyDisk before publication.
func verifyDiskIdentity(ctx context.Context, path string, o Object, received bool) error {
	id, err := diskStructure(ctx, path)
	if err != nil {
		return err
	}
	if err := readonly(path); err != nil {
		return err
	}
	want := id.UUID
	if received {
		// Kernel send_subvol_begin emits received_uuid when it is nonempty,
		// including when the remote sends an already received disk again.
		// Thus the downloaded ReceivedUUID remains the original Object.UUID,
		// not the remote filesystem's newly allocated local UUID.
		want = id.ReceivedUUID
	}
	if want != o.UUID {
		return errors.New("disk subvolume UUID mismatch")
	}
	return nil
}

// Empty/truncated data is still safe to dispose of. Content size and checksum
// are enforced separately at the receive/publication boundary.
func diskStructure(ctx context.Context, path string) (storage.SubvolumeIdentity, error) {
	id, err := storage.InspectSubvolume(ctx, path)
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
	if !st.Mode().IsRegular() {
		return id, errors.New("disk rootfs must be a regular file")
	}
	return id, nil
}

func verifyDisk(ctx context.Context, path string, o Object, received bool) error {
	if err := verifyDiskIdentity(ctx, path, o, received); err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(path, "rootfs.img"))
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() != o.Size {
		return errors.New("rootfs size mismatch")
	}
	h := sha256.New()
	n, err := io.Copy(h, contextReader{ctx, f})
	if err != nil {
		return err
	}
	if n != o.Size || fmt.Sprintf("%x", h.Sum(nil)) != o.SHA256 {
		return errors.New("rootfs checksum mismatch")
	}
	return nil
}

func receivedDisk(ctx context.Context, stage string, o Object) (string, error) {
	entries, err := os.ReadDir(stage)
	if err != nil {
		return "", err
	}
	if len(entries) != 1 {
		return "", errors.New("receive must contain exactly one disk subvolume")
	}
	p := filepath.Join(stage, entries[0].Name())
	if err := verifyDisk(ctx, p, o, true); err != nil {
		return "", err
	}
	return p, nil
}

func cleanup(stage string) {
	// Never use a cancelled transfer context for cleanup. Failed cleanup leaves
	// only a private stage, never a published object, for the next invocation.
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_ = storage.RemoveTree(ctx, stage)
}
