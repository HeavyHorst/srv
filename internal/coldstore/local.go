package coldstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"srv/internal/storage"
)

// ValidateRoot checks trust and filesystem without taking the receive lock.
// A sender must not wait for an unrelated incoming stream before producing
// bytes: opposite-direction migrations could otherwise deadlock. Operations
// on the object namespace still lock and recover stages through withServer.
func ValidateRoot(root string) error {
	if os.Geteuid() != 0 {
		return errors.New("cold store must run as root")
	}
	if err := trustedRoot(root, os.Lstat); err != nil {
		return err
	}
	return storage.RequireBtrfs(root)
}

// Receive publishes a trusted btrfs stream using the same validation, locking,
// and durable immutable publication as the SSH server. root is an existing
// administrator-chosen private btrfs directory, never a peer-supplied path.
// An exact existing object is retained and the redundant stream is consumed,
// allowing concurrent same-base transfers and lost-ack retries to succeed.
func Receive(ctx context.Context, root string, o Object, in io.Reader) error {
	if err := o.validate(); err != nil {
		return err
	}
	ctx, cancel := bounded(ctx, 0)
	defer cancel()
	return withServer(ctx, root, func(s server) error {
		if actual, err := s.stat(ctx, o.ID); err == nil {
			if actual != o {
				return errors.New("immutable object already exists with different metadata")
			}
			_, err = io.Copy(io.Discard, contextReader{ctx, in})
			return err
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return s.put(ctx, o, in)
	})
}

// LocalStat checks a published object's manifest and immutable disk identity.
// Like remote Stat, it does not rescan already-verified disk contents.
func LocalStat(ctx context.Context, root, id string) (Object, error) {
	if !validID(id) {
		return Object{}, errors.New("invalid object ID")
	}
	ctx, cancel := bounded(ctx, 0)
	defer cancel()
	var o Object
	err := withServer(ctx, root, func(s server) error {
		var err error
		o, err = s.stat(ctx, id)
		return err
	})
	return o, err
}

// Clone verifies the exact stored object, including its contents, then creates
// a new writable snapshot at dest. dest is a runner-chosen absent path on the
// same btrfs filesystem, not a peer-supplied path. The stored object is retained.
// A failure after snapshot creation may leave dest published; never remove the
// stored object on failure. The caller owns destination recovery/publication.
func Clone(ctx context.Context, root string, o Object, dest string) error {
	if err := o.validate(); err != nil {
		return err
	}
	ctx, cancel := bounded(ctx, 0)
	defer cancel()
	return withServer(ctx, root, func(s server) error {
		ctx := storage.WithInheritedLock(ctx, s.namespaceLock)
		actual, err := s.stat(ctx, o.ID)
		if err != nil {
			return err
		}
		if actual != o {
			return errors.New("stored object metadata mismatch")
		}
		disk := filepath.Join(s.path(o.ID), "disk")
		if err := verifyDisk(ctx, disk, o, true); err != nil {
			return err
		}
		return storage.SnapshotSubvolume(ctx, disk, dest, false)
	})
}

// Remove explicitly deletes an object, retaining all other objects. Referenced
// parents cannot be removed. Like remote Delete, retries are safe and the ID
// is permanently fenced against late receives, even when it was absent.
func Remove(ctx context.Context, root, id string) error {
	if !validID(id) {
		return errors.New("invalid object ID")
	}
	ctx, cancel := bounded(ctx, 0)
	defer cancel()
	return withServer(ctx, root, func(s server) error { return s.delete(ctx, id) })
}

// Send streams an original or previously received read-only snapshot without SSH.
// The parent, if supplied, must also be a read-only disk snapshot. Object.Parent
// names the receiving store's dependency, not a local path or subvolume UUID.
func Send(ctx context.Context, o Object, snapshotPath, parentPath string, out io.Writer) error {
	if err := o.validate(); err != nil {
		return err
	}
	if (parentPath == "") != (o.Parent == "") {
		return errors.New("parent path and object parent must be supplied together")
	}
	ctx, cancel := bounded(ctx, 0)
	defer cancel()
	id, err := storage.InspectSubvolume(ctx, snapshotPath)
	if err != nil {
		return err
	}
	// btrfs send emits received_uuid when present, otherwise the local UUID.
	if err := verifyDiskIdentity(ctx, snapshotPath, o, id.ReceivedUUID != ""); err != nil {
		return err
	}
	if parentPath != "" {
		if _, err := diskStructure(ctx, parentPath); err != nil {
			return err
		}
		if err := readonly(parentPath); err != nil {
			return err
		}
	}
	return run(ctx, "btrfs", sendArgs(snapshotPath, parentPath), nil, out)
}

func sendArgs(snapshotPath, parentPath string) []string {
	args := []string{"send"}
	if parentPath != "" {
		args = append(args, "-p", parentPath)
	}
	return append(args, snapshotPath)
}
