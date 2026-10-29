package provision

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"srv/internal/model"
	"srv/internal/storage"
)

func (r cleanupRunner) VerifyInstanceIdle(context.Context, string) error { return r.verify() }

// Unit tests use real directories and durable journal/SQLite writes, with only
// btrfs kernel operations substituted. Kernel snapshot semantics need a btrfs
// integration host; these seams are not evidence of kernel-level correctness.
func useTestDiskStorage(t *testing.T) {
	t.Helper()
	create, inspect, snapshot, require, readonly := storage.CreateSubvolume, storage.InspectSubvolume, storage.SnapshotSubvolume, storage.RequireBtrfs, storage.RequireReadonly
	containing, loops := storage.ContainingSubvolume, loopDevicesForPath
	payload := storage.AcquirePayload
	t.Cleanup(func() {
		storage.CreateSubvolume, storage.InspectSubvolume, storage.SnapshotSubvolume, storage.RequireBtrfs, storage.RequireReadonly = create, inspect, snapshot, require, readonly
		storage.ContainingSubvolume, loopDevicesForPath = containing, loops
		storage.AcquirePayload = payload
	})
	// Exercise real flock serialization; only installer/root ownership is
	// substituted, like Btrfs below. Strict trust is tested in storage/hosts.
	storage.AcquirePayload = func(ctx context.Context, dataDir string) (context.Context, func(), error) {
		f, err := os.OpenFile(filepath.Join(dataDir, ".test-payload-lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return ctx, nil, err
		}
		for {
			if err := ctx.Err(); err != nil {
				f.Close()
				return ctx, nil, err
			}
			if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
				return storage.WithInheritedLock(ctx, f), func() { f.Close() }, nil
			} else if !errors.Is(err, unix.EWOULDBLOCK) {
				f.Close()
				return ctx, nil, err
			}
			time.Sleep(time.Millisecond)
		}
	}
	storage.ContainingSubvolume = func(ctx context.Context, path string) (storage.SubvolumeIdentity, error) {
		if err := storage.ValidatePath(path); err != nil {
			return storage.SubvolumeIdentity{}, err
		}
		return storage.SubvolumeIdentity{UUID: "persistent-data-root", ID: "257"}, nil
	}
	loopDevicesForPath = func(string) (string, error) { return "", nil }
	storage.RequireBtrfs = func(path string) error { return storage.ValidatePath(path) }
	storage.CreateSubvolume = func(ctx context.Context, path string) error { return os.Mkdir(path, 0o770) }
	storage.InspectSubvolume = func(ctx context.Context, path string) (storage.SubvolumeIdentity, error) {
		if err := storage.ValidatePath(path); err != nil {
			return storage.SubvolumeIdentity{}, err
		}
		st, err := os.Stat(path)
		if err != nil {
			return storage.SubvolumeIdentity{}, err
		}
		if !st.IsDir() {
			return storage.SubvolumeIdentity{}, errors.New("not a directory")
		}
		id := fmt.Sprint(st.Sys().(*syscall.Stat_t).Ino)
		return storage.SubvolumeIdentity{ID: id, UUID: id}, nil
	}
	storage.SnapshotSubvolume = func(ctx context.Context, source, dest string, ro bool) error {
		if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
			return errors.New("destination exists")
		}
		return exec.CommandContext(ctx, "cp", "-a", source, dest).Run()
	}
	storage.RequireReadonly = func(context.Context, string) error { return nil }
}

func TestDiskMigrationRecovery(t *testing.T) {
	for _, phase := range []string{"new", "intent", "empty-created", "identified", "copied", "published", "committed", "legacy-cleaned", "wrong-instance", "wrong-subvolume", "unidentified-data", "uncertain-runtime", "changed-legacy-subvolume", "changed-legacy-inode", "device-number-ignored", "loop-attached", "loop-probe-error"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			cfg := loadProvisionTestConfig(t, nil)
			st := newProvisionTestStore(t, cfg)
			inst := provisionTestInstance(cfg, "legacy", model.StateStopped, time.Now())
			dir := filepath.Join(cfg.InstancesDir(), inst.Name)
			legacy := filepath.Join(dir, "rootfs.img")
			canonical := filepath.Join(dir, "disk", "rootfs.img")
			inst.RootFSPath = legacy
			if err := os.MkdirAll(dir, 0o770); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(legacy, []byte("original disk"), 0o660); err != nil {
				t.Fatal(err)
			}
			info, _ := os.Stat(legacy)
			raw := info.Sys().(*syscall.Stat_t)
			containing, err := storage.ContainingSubvolume(ctx, legacy)
			if err != nil {
				t.Fatal(err)
			}
			j := diskMigration{Version: 1, Operation: uuid.NewString(), InstanceID: inst.ID, LegacySubvolume: containing, LegacyInode: raw.Ino}
			stage := filepath.Join(dir, ".disk-migration-"+j.Operation)
			journal := filepath.Join(dir, "disk-migration.json")
			if phase != "new" && phase != "intent" && phase != "uncertain-runtime" {
				if err := storage.CreateSubvolume(ctx, stage); err != nil {
					t.Fatal(err)
				}
				if phase != "empty-created" && phase != "unidentified-data" {
					j.Subvolume, _ = storage.InspectSubvolume(ctx, stage)
				}
				if phase != "empty-created" && phase != "identified" {
					if err := os.WriteFile(filepath.Join(stage, "rootfs.img"), []byte("original disk"), 0o660); err != nil {
						t.Fatal(err)
					}
					j.Ready = true
				}
			}
			if phase == "published" || phase == "committed" || phase == "legacy-cleaned" {
				if err := os.Rename(stage, filepath.Dir(canonical)); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "committed" || phase == "legacy-cleaned" {
				inst.RootFSPath = canonical
			}
			if phase == "legacy-cleaned" {
				if err := os.Remove(legacy); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "wrong-instance" {
				j.InstanceID = "different-incarnation"
			}
			if phase == "wrong-subvolume" {
				j.Subvolume.UUID = "different-subvolume"
			}
			if phase == "changed-legacy-subvolume" {
				j.LegacySubvolume.UUID = "different-containing-subvolume"
			}
			if phase == "changed-legacy-inode" {
				j.LegacyInode++
			}
			if phase == "loop-attached" {
				loopDevicesForPath = func(string) (string, error) { return "/dev/loop7", nil }
			}
			if phase == "loop-probe-error" {
				loopDevicesForPath = func(string) (string, error) { return "", errors.New("loop inspection failed") }
			}
			if phase != "new" && phase != "uncertain-runtime" {
				if err := writeDiskJournal(journal, j); err != nil {
					t.Fatal(err)
				}
				payload, err := os.ReadFile(journal)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(payload), "legacy_device") {
					t.Fatal("journal persists ephemeral st_dev")
				}
				if phase == "device-number-ignored" {
					// Simulate a journal carrying a pre-remount device number.
					// Recovery must use only the stable subvolume identity/inode.
					payload = append([]byte(fmt.Sprintf(`{"legacy_device":%d,`, uint64(raw.Dev)+1)), payload[1:]...)
					if err := os.WriteFile(journal, payload, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			residue := filepath.Join(dir, ".disk-journal-interrupted")
			if err := os.WriteFile(residue, []byte(`{"instance_id":"foreign-uncommitted"`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateInstance(ctx, inst); err != nil {
				t.Fatal(err)
			}
			p := &Provisioner{cfg: cfg, store: st, vmRunner: cleanupRunner{verify: func() error {
				if phase == "uncertain-runtime" {
					return errors.New("unknown")
				}
				return nil
			}}}
			old := reflinkCloneFile
			t.Cleanup(func() { reflinkCloneFile = old })
			reflinkCloneFile = func(ctx context.Context, src, dest string) error {
				return exec.CommandContext(ctx, "cp", src, dest).Run()
			}
			err = p.ensureDiskLayout(ctx, &inst)
			bad := phase == "wrong-instance" || phase == "wrong-subvolume" || phase == "unidentified-data" || phase == "uncertain-runtime" || phase == "changed-legacy-subvolume" || phase == "changed-legacy-inode" || phase == "loop-attached" || phase == "loop-probe-error"
			if bad {
				if err == nil {
					t.Fatal("ambiguous migration accepted")
				}
				if data, err := os.ReadFile(legacy); err != nil || string(data) != "original disk" {
					t.Fatalf("legacy changed: %q %v", data, err)
				}
				if _, err := os.Stat(residue); err != nil {
					t.Fatal("residue removed before authoritative recovery completed", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			stored, err := st.GetInstance(ctx, inst.Name)
			if err != nil || stored.RootFSPath != canonical {
				t.Fatalf("database: %s %v", stored.RootFSPath, err)
			}
			for _, path := range []string{legacy, journal, stage, residue} {
				if err := absentPath(path); err != nil {
					t.Fatal(err)
				}
			}
			if data, err := os.ReadFile(canonical); err != nil || string(data) != "original disk" {
				t.Fatalf("disk: %q %v", data, err)
			}
			if err := os.WriteFile(residue, []byte(`{"instance_id":"foreign"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := p.ensureDiskLayout(ctx, &inst); err != nil {
				t.Fatalf("idempotence: %v", err)
			}
			if err := absentPath(residue); err != nil {
				t.Fatal("canonical retry retained temporary journal", err)
			}
		})
	}
}

func TestDiskJournalTempCleanupRejectsNonregularResidue(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			cfg := loadProvisionTestConfig(t, nil)
			inst := provisionTestInstance(cfg, "canonical", model.StateStopped, time.Now())
			if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o770); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(inst.RootFSPath, []byte("canonical disk"), 0o660); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(cfg.InstancesDir(), inst.Name)
			residue := filepath.Join(dir, ".disk-journal-nonregular")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(inst.RootFSPath, residue)
			case "directory":
				err = os.Mkdir(residue, 0o700)
			case "fifo":
				err = syscall.Mkfifo(residue, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			regular := filepath.Join(dir, ".disk-journal-a-regular")
			if err := os.WriteFile(regular, []byte("partial"), 0o600); err != nil {
				t.Fatal(err)
			}
			p := &Provisioner{cfg: cfg}
			if err := p.ensureDiskLayout(context.Background(), &inst); err == nil {
				t.Fatal("nonregular residue allowed boot")
			}
			for _, path := range []string{residue, regular} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("removed residue before validation: %v", err)
				}
			}
			if data, err := os.ReadFile(inst.RootFSPath); err != nil || string(data) != "canonical disk" {
				t.Fatalf("disk changed: %q %v", data, err)
			}
		})
	}
}

func TestBaseDiskSnapshotLineageAndIsolation(t *testing.T) {
	cfg := loadProvisionTestConfig(t, nil)
	p := &Provisioner{cfg: cfg}
	ctx := context.Background()
	source := filepath.Join(cfg.DataDirAbs(), "distribution.img")
	if err := os.WriteFile(source, []byte("pristine distribution"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := reflinkCloneFile
	t.Cleanup(func() { reflinkCloneFile = old })
	clones := 0
	reflinkCloneFile = func(ctx context.Context, src, dest string) error {
		clones++
		return exec.CommandContext(ctx, "cp", src, dest).Run()
	}
	snapshot := storage.SnapshotSubvolume
	var parents []string
	readonly := 0
	storage.SnapshotSubvolume = func(ctx context.Context, src, dest string, ro bool) error {
		if ro {
			readonly++
		} else {
			parents = append(parents, src)
		}
		return snapshot(ctx, src, dest, ro)
	}
	for _, name := range []string{"one", "two"} {
		dir := filepath.Join(cfg.InstancesDir(), name)
		if err := os.MkdirAll(dir, 0o770); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(dir, "disk", "rootfs.img")
		if err := p.cloneInstanceDisk(ctx, source, dest, false); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, []byte("guest mutation"), 0o660); err != nil {
			t.Fatal(err)
		}
	}
	if clones != 1 || readonly != 1 || len(parents) != 2 || parents[0] != parents[1] {
		t.Fatalf("no shared immutable base: clones=%d readonly=%d parents=%v", clones, readonly, parents)
	}
	if data, err := os.ReadFile(filepath.Join(parents[0], "rootfs.img")); err != nil || string(data) != "pristine distribution" {
		t.Fatalf("base changed: %q %v", data, err)
	}
}

func TestDiskMigrationDatabaseFailureRetainsLegacy(t *testing.T) {
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	inst := provisionTestInstance(cfg, "legacy", model.StateStopped, time.Now())
	dir := filepath.Join(cfg.InstancesDir(), inst.Name)
	inst.RootFSPath = filepath.Join(dir, "rootfs.img")
	if err := os.MkdirAll(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.RootFSPath, []byte("original"), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInstance(context.Background(), inst); err != nil {
		t.Fatal(err)
	}
	p := &Provisioner{cfg: cfg, store: st, vmRunner: cleanupRunner{verify: func() error { return nil }}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := reflinkCloneFile
	t.Cleanup(func() { reflinkCloneFile = old })
	reflinkCloneFile = func(ctx context.Context, source, dest string) error {
		if err := exec.Command("cp", source, dest).Run(); err != nil {
			return err
		}
		cancel() // durable filesystem publication completes; DB commit fails.
		return nil
	}
	if err := p.ensureDiskLayout(ctx, &inst); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected DB cancellation, got %v", err)
	}
	if data, err := os.ReadFile(inst.RootFSPath); err != nil || string(data) != "original" {
		t.Fatalf("legacy lost before commit: %q %v", data, err)
	}
	if err := p.diskMutationFence(context.Background(), inst); err == nil {
		t.Fatal("offline mutation allowed during migration")
	}
	if err := p.ensureDiskLayout(context.Background(), &inst); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if err := absentPath(filepath.Join(dir, "rootfs.img")); err != nil {
		t.Fatal(err)
	}
}
