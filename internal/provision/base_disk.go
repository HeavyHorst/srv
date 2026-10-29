package provision

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"srv/internal/storage"
)

func diskDigest(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buf)
		_, _ = h.Write(buf[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// cloneInstanceDisk preserves btrfs snapshot ancestry. Plain distribution
// images are imported once into immutable, content-addressed bases; arbitrary
// reflink files in unrelated subvolumes would not provide that ancestry.
func (p *Provisioner) cloneInstanceDisk(ctx context.Context, source, dest string, templateSource bool) error {
	p.admissionMu.Lock()
	defer p.admissionMu.Unlock()
	if filepath.Base(filepath.Dir(source)) == "disk" {
		if _, err := storage.ValidateDiskSubvolume(ctx, filepath.Dir(source)); err != nil {
			return err
		}
		if err := storage.RequireReadonly(ctx, filepath.Dir(source)); err != nil {
			return err
		}
		if err := storage.SnapshotSubvolume(ctx, filepath.Dir(source), filepath.Dir(dest), false); err != nil {
			return err
		}
		return os.Chmod(dest, 0o660)
	}
	// Legacy file-only templates remain readable, but must never enter the
	// global distribution-image cache (which intentionally outlives instances).
	if templateSource {
		if err := storage.CreateSubvolume(ctx, filepath.Dir(dest)); err != nil {
			return err
		}
		return p.cloneRootFS(ctx, source, dest)
	}
	digest, err := diskDigest(ctx, source)
	if err != nil {
		return err
	}
	parent := filepath.Join(p.cfg.DataDirAbs(), "image-bases")
	if err := os.MkdirAll(parent, 0o770); err != nil {
		return err
	}
	base := filepath.Join(parent, digest)
	disk := filepath.Join(base, "disk")
	if _, err := os.Lstat(base); errors.Is(err, os.ErrNotExist) {
		stage, err := os.MkdirTemp(parent, ".base-")
		if err != nil {
			return err
		}
		defer removePathAll(stage)
		writable := filepath.Join(stage, "writable")
		if err := storage.CreateSubvolume(ctx, writable); err != nil {
			return err
		}
		image := filepath.Join(writable, "rootfs.img")
		if err := p.cloneRootFS(ctx, source, image); err != nil {
			return err
		}
		actual, err := diskDigest(ctx, image)
		if err != nil {
			return err
		}
		if actual != digest {
			return errors.New("base image changed during import")
		}
		if err := storage.SyncPath(image); err != nil {
			return err
		}
		if err := storage.SnapshotSubvolume(ctx, writable, filepath.Join(stage, "disk"), true); err != nil {
			return err
		}
		if err := removePathAll(writable); err != nil {
			return err
		}
		if err := os.Rename(stage, base); err != nil {
			return err
		}
		if err := storage.SyncPath(parent); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := storage.ValidateDiskSubvolume(ctx, disk); err != nil {
		return err
	}
	if err := storage.RequireReadonly(ctx, disk); err != nil {
		return err
	}
	if err := storage.SnapshotSubvolume(ctx, disk, filepath.Dir(dest), false); err != nil {
		return err
	}
	return os.Chmod(dest, 0o660)
}
