package coldstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// DiscardObjects is a LOCAL administrator-only, irreversible recovery operation,
// deliberately absent from the SSH protocol. The administrator explicitly
// abandons every supplied ID, including its unknown manifest dependencies.
// Every object outside this set must have a valid manifest and must not depend
// on the set. Thus corrupt manifests never cause guessed dependencies: include
// all corrupt objects and their dependents, or restore metadata from backup.
// Disk bytes and manifest contents need not be intact, but namespace boundaries,
// regular files and read-only disk subvolume structure must still be valid.
// A failure may leave some IDs permanently fenced or removed; retry the SAME
// complete set. There is no rollback and fences must never be removed.
func DiscardObjects(ctx context.Context, root string, ids []string) error {
	if len(ids) == 0 {
		return errors.New("explicit nonempty discard set required")
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !validID(id) || set[id] {
			return errors.New("discard IDs must be unique canonical UUIDs")
		}
		set[id] = true
	}
	return withServer(ctx, root, func(s server) error {
		entries, err := os.ReadDir(s.objects)
		if err != nil {
			return err
		}
		var present []string
		for _, e := range entries {
			id := e.Name()
			if !validID(id) {
				return errors.New("unexpected object namespace entry")
			}
			if !set[id] {
				o, err := s.manifest(id)
				if err != nil {
					return fmt.Errorf("object %s must be restored or explicitly included in discard set: %w", id, err)
				}
				if set[o.Parent] {
					return fmt.Errorf("retained object %s depends on discard set", id)
				}
				continue
			}
			p := s.path(id)
			if err := ordinaryDir(p); err != nil {
				return err
			}
			st, err := os.Lstat(p)
			if err != nil || st.Mode().Perm() != 0700 {
				return errors.New("discard container must be private mode 0700")
			}
			children, err := os.ReadDir(p)
			if err != nil || len(children) != 2 || children[0].Name() != "disk" || children[1].Name() != "manifest.json" {
				return errors.New("invalid discard object layout")
			}
			st, err = os.Lstat(filepath.Join(p, "manifest.json"))
			if err != nil {
				return err
			}
			if !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || st.Sys().(*syscall.Stat_t).Uid != 0 || st.Sys().(*syscall.Stat_t).Nlink != 1 {
				return errors.New("unsafe discard manifest")
			}
			disk := filepath.Join(p, "disk")
			if _, err := diskStructure(ctx, disk); err != nil {
				return err
			}
			if err := readonly(disk); err != nil {
				return err
			}
			present = append(present, id)
		}
		// Validate the entire disposition before any irreversible action. Fence
		// the entire set before renaming even its first member.
		for _, id := range ids {
			if _, err := s.deletionRecord(id, true); err != nil {
				return err
			}
		}
		for _, id := range present {
			stage := filepath.Join(root, ".stage-delete-"+id)
			if err := unix.Renameat2(unix.AT_FDCWD, s.path(id), unix.AT_FDCWD, stage, unix.RENAME_NOREPLACE); err != nil {
				return err
			}
		}
		return s.recoverStages(ctx)
	})
}
