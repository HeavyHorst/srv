package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"srv/internal/model"
	"srv/internal/storage"
	"srv/internal/vmrunner"
)

type diskMigration struct {
	Version         int                       `json:"version"`
	Operation       string                    `json:"operation"`
	InstanceID      string                    `json:"instance_id"`
	Subvolume       storage.SubvolumeIdentity `json:"subvolume"`
	LegacySubvolume storage.SubvolumeIdentity `json:"legacy_subvolume"`
	LegacyInode     uint64                    `json:"legacy_inode"`
	Ready           bool                      `json:"ready"`
}

func writeDiskJournal(path string, j diskMigration) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".disk-journal-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return storage.SyncPath(filepath.Dir(path))
}

// Temporary writes are never authoritative: even partial JSON or a different
// instance ID can be left by a write that never reached publication. Only
// regular files in this reserved namespace may be reclaimed, after recovery.
func cleanupDiskJournalTemps(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var paths []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".disk-journal-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("nonregular disk journal temporary file: %s", path)
		}
		paths = append(paths, path)
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return storage.SyncPath(dir)
}

// ensureDiskLayout runs before path prerequisites and before any boot side
// effect, under admission/lifecycle locks. No legacy data is removed until the
// staged disk is durable, published, and the database points at it.
func (p *Provisioner) ensureDiskLayout(ctx context.Context, inst *model.Instance) error {
	if err := p.coldDiskFence(ctx, *inst, false); err != nil {
		return err
	}
	dir, err := p.instanceDir(inst.Name)
	if err != nil {
		return err
	}
	if err := storage.ValidatePath(dir); err != nil {
		return err
	}
	legacy := filepath.Join(dir, "rootfs.img")
	disk := filepath.Join(dir, "disk")
	canonical := filepath.Join(disk, "rootfs.img")
	journal := filepath.Join(dir, "disk-migration.json")
	if err := storage.ValidatePath(journal); err != nil {
		return err
	}
	b, readErr := os.ReadFile(journal)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if errors.Is(readErr, os.ErrNotExist) && inst.RootFSPath == canonical {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".disk-migration-") {
				return errors.New("unresolved disk migration stage")
			}
		}
		if err := absentPath(legacy); err != nil {
			return err
		}
		_, err = storage.ValidateDiskSubvolume(ctx, disk)
		if err != nil {
			return err
		}
		return cleanupDiskJournalTemps(dir)
	}
	if inst.State != model.StateStopped || inst.FirecrackerPID != 0 {
		return errors.New("disk migration requires a definitely stopped instance")
	}
	if inst.StartRequestID == "" && inst.StartBootID == "" {
		proof, ok := p.vmRunner.(interface {
			VerifyInstanceIdle(context.Context, string) error
		})
		if !ok {
			return errors.New("disk migration requires idle-runtime proof")
		}
		if err := proof.VerifyInstanceIdle(ctx, inst.Name); err != nil {
			return err
		}
	} else {
		if inst.StartRequestID == "" || inst.StartBootID == "" {
			return errors.New("disk migration has partial runtime identity")
		}
		proof, ok := p.vmRunner.(interface {
			VerifyInstanceAbsent(context.Context, vmrunner.StartResultRequest) error
		})
		if !ok {
			return errors.New("disk migration requires runtime absence proof")
		}
		if err := proof.VerifyInstanceAbsent(ctx, vmrunner.StartResultRequest{Name: inst.Name, InstanceID: inst.ID, RequestID: inst.StartRequestID, HostBootID: inst.StartBootID}); err != nil {
			return err
		}
	}
	var j diskMigration
	if readErr == nil {
		if err := json.Unmarshal(b, &j); err != nil {
			return err
		}
		if j.Version != 1 || j.InstanceID != inst.ID {
			return errors.New("disk migration instance identity mismatch")
		}
		if j.LegacySubvolume.UUID == "" || j.LegacySubvolume.ID == "" || j.LegacyInode == 0 {
			return errors.New("disk migration lacks persistent legacy identity; operator inspection required")
		}
		if _, err := uuid.Parse(j.Operation); err != nil {
			return errors.New("invalid disk migration operation")
		}
	} else {
		if inst.RootFSPath != legacy {
			return errors.New("unrecognized instance disk layout")
		}
		if err := absentPath(disk); err != nil {
			return err
		}
		st, err := os.Lstat(legacy)
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return errors.New("legacy rootfs is not a regular file")
		}
		raw := st.Sys().(*syscall.Stat_t)
		containing, err := storage.ContainingSubvolume(ctx, legacy)
		if err != nil {
			return err
		}
		if containing.UUID == "" || containing.ID == "" {
			return errors.New("legacy disk lacks persistent subvolume identity")
		}
		j = diskMigration{Version: 1, Operation: uuid.NewString(), InstanceID: inst.ID, LegacySubvolume: containing, LegacyInode: raw.Ino}
		if err := writeDiskJournal(journal, j); err != nil {
			return err
		}
	}
	stage := filepath.Join(dir, ".disk-migration-"+j.Operation)
	if inst.RootFSPath != legacy && inst.RootFSPath != canonical {
		return errors.New("database disk path conflicts with migration")
	}
	if st, err := os.Lstat(legacy); err == nil {
		raw := st.Sys().(*syscall.Stat_t)
		if !st.Mode().IsRegular() || raw.Ino != j.LegacyInode {
			return errors.New("legacy disk identity changed")
		}
		containing, err := storage.ContainingSubvolume(ctx, legacy)
		if err != nil {
			return err
		}
		if containing.UUID != j.LegacySubvolume.UUID || containing.ID != j.LegacySubvolume.ID {
			return errors.New("legacy disk subvolume identity changed")
		}
		loops, err := loopDevicesForPath(legacy)
		if err != nil {
			return err
		}
		if strings.TrimSpace(loops) != "" {
			return errors.New("legacy disk is attached to a loop device; refusing migration")
		}
	} else if !errors.Is(err, os.ErrNotExist) || inst.RootFSPath != canonical || !j.Ready {
		return fmt.Errorf("legacy disk unavailable before commit: %w", err)
	}
	if j.Subvolume.UUID == "" {
		// The durable journal reserves this unpredictable operation-qualified
		// name. Before identity is committed no data may enter it. Recover the
		// create/record crash window only if that reserved subvolume is empty.
		if inst.RootFSPath != legacy {
			return errors.New("migration identity missing after database commit")
		}
		if err := absentPath(disk); err != nil {
			return err
		}
		if _, err := os.Lstat(stage); errors.Is(err, os.ErrNotExist) {
			if err := storage.CreateSubvolume(ctx, stage); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		entries, err := os.ReadDir(stage)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return errors.New("unidentified migration stage contains data; operator inspection required")
		}
		j.Subvolume, err = storage.InspectSubvolume(ctx, stage)
		if err != nil {
			return err
		}
		if err := writeDiskJournal(journal, j); err != nil {
			return err
		}
	}
	location := stage
	if _, err := os.Lstat(disk); err == nil {
		location = disk
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	id, err := storage.InspectSubvolume(ctx, location)
	if err != nil {
		return err
	}
	if id.UUID != j.Subvolume.UUID || id.ID != j.Subvolume.ID {
		return errors.New("disk migration subvolume identity mismatch")
	}
	if location == disk {
		if !j.Ready {
			return errors.New("disk was published before staging completed")
		}
		if err := absentPath(stage); err != nil {
			return err
		}
	} else {
		if !j.Ready {
			// This is our recorded stage, so an interrupted clone can be retried.
			if err := os.Remove(filepath.Join(stage, "rootfs.img")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := p.cloneRootFS(ctx, legacy, filepath.Join(stage, "rootfs.img")); err != nil {
				return err
			}
			if err := storage.SyncPath(filepath.Join(stage, "rootfs.img")); err != nil {
				return err
			}
			if err := storage.SyncPath(stage); err != nil {
				return err
			}
			j.Ready = true
			if err := writeDiskJournal(journal, j); err != nil {
				return err
			}
		}
		if err := os.Rename(stage, disk); err != nil {
			return err
		}
		if err := storage.SyncPath(dir); err != nil {
			return err
		}
	}
	if _, err := storage.ValidateDiskSubvolume(ctx, disk); err != nil {
		return err
	}
	if inst.RootFSPath != canonical {
		updated := *inst
		updated.RootFSPath, updated.UpdatedAt = canonical, time.Now().UTC()
		if err := p.store.UpdateInstance(ctx, updated); err != nil {
			return err
		}
		*inst = updated
	}
	if err := os.Remove(legacy); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := storage.SyncPath(dir); err != nil {
		return err
	}
	if err := os.Remove(journal); err != nil {
		return err
	}
	return cleanupDiskJournalTemps(dir)
}

// Offline mutations must not change the authoritative legacy file while a
// durable staged copy is awaiting publication/recovery.
func (p *Provisioner) diskMutationFence(ctx context.Context, inst model.Instance) error {
	if err := p.coldDiskFence(ctx, inst, false); err != nil {
		return err
	}
	dir, err := p.instanceDir(inst.Name)
	if err != nil {
		return err
	}
	if err := absentPath(filepath.Join(dir, "disk-migration.json")); err != nil {
		return fmt.Errorf("disk migration recovery required before mutation: %w", err)
	}
	return nil
}
