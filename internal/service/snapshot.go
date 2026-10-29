package service

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"srv/internal/storage"
)

const btrfsSubvolumeRootInode = 256

var (
	errSnapshotInProgress = errors.New("snapshot in progress; try again once the current snapshot completes")
	snapshotNow           = func() time.Time { return time.Now().UTC() }
)

type commandGateLease struct {
	app      *App
	snapshot bool
	released bool
}

type dataSnapshotInfo struct {
	ID        string
	Path      string
	CreatedAt time.Time
}

func (a *App) beginCommand() (*commandGateLease, error) {
	a.commandOnce.Do(func() {
		a.commandCond = sync.NewCond(&a.commandMu)
	})

	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if a.snapshotOn {
		return nil, errSnapshotInProgress
	}
	a.inFlight++
	return &commandGateLease{app: a}, nil
}

func (l *commandGateLease) PromoteToSnapshot(ctx context.Context) error {
	if l == nil || l.app == nil {
		return errors.New("snapshot gate is unavailable")
	}
	a := l.app
	a.commandOnce.Do(func() {
		a.commandCond = sync.NewCond(&a.commandMu)
	})

	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if l.released {
		return errors.New("command gate lease already released")
	}
	if l.snapshot {
		return nil
	}
	if a.snapshotOn {
		return errSnapshotInProgress
	}

	a.snapshotOn = true
	a.inFlight--
	notifyStop := context.AfterFunc(ctx, func() {
		a.commandMu.Lock()
		defer a.commandMu.Unlock()
		a.commandCond.Broadcast()
	})
	defer notifyStop()

	for a.inFlight > 0 {
		if err := ctx.Err(); err != nil {
			a.snapshotOn = false
			a.inFlight++
			a.commandCond.Broadcast()
			return err
		}
		a.commandCond.Wait()
	}

	l.snapshot = true
	return nil
}

func (l *commandGateLease) Release() {
	if l == nil || l.app == nil {
		return
	}
	a := l.app
	a.commandOnce.Do(func() {
		a.commandCond = sync.NewCond(&a.commandMu)
	})

	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if l.released {
		return
	}
	l.released = true
	if l.snapshot {
		a.snapshotOn = false
		a.commandCond.Broadcast()
		return
	}
	a.inFlight--
	if a.inFlight == 0 {
		a.commandCond.Broadcast()
	}
}

func (a *App) waitForSnapshotBarrierToLift() {
	a.commandOnce.Do(func() {
		a.commandCond = sync.NewCond(&a.commandMu)
	})

	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	for a.snapshotOn {
		a.commandCond.Wait()
	}
}

func (a *App) cmdSnapshot(ctx context.Context, args []string, lease *commandGateLease) (commandResult, error) {
	action, err := parseSnapshotArgs(args)
	if err != nil {
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	switch action {
	case "create":
		return a.cmdSnapshotCreate(ctx, lease)
	default:
		return commandResult{stderr: snapshotUsage() + "\n", exitCode: 2}, errors.New(snapshotUsage())
	}
}

func (a *App) cmdSnapshotCreate(ctx context.Context, lease *commandGateLease) (commandResult, error) {
	if err := lease.PromoteToSnapshot(ctx); err != nil {
		return commandResult{stderr: fmt.Sprintf("snapshot create: %v\n", err), exitCode: 1}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	info, err := a.createDataSnapshot(ctx)
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("snapshot create: %v\n", err), exitCode: 1}, err
	}
	if a.log != nil {
		a.log.Info("created local data snapshot", "snapshot_id", info.ID, "path", info.Path)
	}

	stdout := fmt.Sprintf(
		"snapshot-created: %s\npath: %s\ncreated-at: %s\nconsistency: control-plane consistent; stopped guests fully safe; running guests individually crash-consistent (not a global instant)\n",
		info.ID,
		info.Path,
		info.CreatedAt.Format(time.RFC3339),
	)
	return commandResult{stdout: stdout, exitCode: 0}, nil
}

func (a *App) createDataSnapshot(ctx context.Context) (dataSnapshotInfo, error) {
	dataDir := a.cfg.DataDirAbs()
	if err := ensureBtrfsSubvolume(dataDir); err != nil {
		return dataSnapshotInfo{}, err
	}
	// A nested archive subvolume is essential: otherwise each root snapshot
	// would capture the manifests and directories of all preceding bundles.
	archive := a.cfg.SnapshotsDir()
	if _, err := os.Lstat(archive); errors.Is(err, os.ErrNotExist) {
		output, err := exec.CommandContext(ctx, "btrfs", "subvolume", "create", archive).CombinedOutput()
		if err != nil {
			return dataSnapshotInfo{}, fmt.Errorf("create snapshot archive: %w: %s", err, output)
		}
	}
	if err := ensureBtrfsSubvolume(archive); err != nil {
		return dataSnapshotInfo{}, fmt.Errorf("snapshot archive must be a dedicated subvolume (migrate any legacy archive first): %w", err)
	}
	paths, err := inventorySnapshotDisks(dataDir, snapshotSubvolume)
	if err != nil {
		return dataSnapshotInfo{}, err
	}
	instances, err := a.store.ListInstances(ctx, false)
	if err != nil {
		return dataSnapshotInfo{}, fmt.Errorf("snapshot instance inventory: %w", err)
	}
	inventory := make(map[string]bool, len(paths))
	for _, path := range paths {
		inventory[filepath.Join(dataDir, path, "rootfs.img")] = true
	}
	for _, inst := range instances {
		dir := filepath.Join(dataDir, "instances", inst.Name)
		expected, err := snapshotDiskImage(dir, true, snapshotSubvolume)
		if err != nil || inst.RootFSPath != expected || (expected == filepath.Join(dir, "disk", "rootfs.img") && !inventory[expected]) {
			return dataSnapshotInfo{}, fmt.Errorf("snapshot inventory: instance %q disk is missing or noncanonical", inst.Name)
		}
	}
	if err := a.store.Checkpoint(ctx); err != nil {
		return dataSnapshotInfo{}, fmt.Errorf("checkpoint sqlite before snapshot: %w", err)
	}
	return createSnapshotBundle(ctx, dataDir, archive, paths, snapshotNow(), createReadonlyBtrfsSnapshot, syncPathFilesystem, snapshotUUID)
}

type snapshotComponent struct {
	Source       string `json:"source"` // relative to SRV_DATA_DIR; "." is the root
	Path         string `json:"path"`   // relative to the bundle directory
	SourceUUID   string `json:"source_uuid"`
	SnapshotUUID string `json:"snapshot_uuid"`
}

func snapshotUUID(ctx context.Context, path string) (string, error) {
	id, err := storage.InspectSubvolume(ctx, path)
	return id.UUID, err
}

type snapshotManifest struct {
	Version    int                 `json:"version"`
	ID         string              `json:"id"`
	CreatedAt  time.Time           `json:"created_at"`
	Components []snapshotComponent `json:"components"`
}

func snapshotSubvolume(path string) (bool, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return false, err
	}
	return st.Ino == btrfsSubvolumeRootInode && st.Mode&unix.S_IFMT == unix.S_IFDIR, nil
}

// Legacy rootfs files remain in the root component until lazy migration. Never
// choose between two layouts or capture an unresolved migration as complete.
func snapshotDiskImage(dir string, allowLegacy bool, isSubvolume func(string) (bool, error)) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Name() == "cold-disk.json" {
			return "", fmt.Errorf("snapshot inventory: cold disk marker at %s; hydrate and purge remote state first", dir)
		}
		if entry.Name() == "disk-migration.json" || strings.HasPrefix(entry.Name(), ".disk-migration-") || strings.HasPrefix(entry.Name(), ".disk-journal-") {
			return "", fmt.Errorf("snapshot inventory: pending disk migration at %s", dir)
		}
	}
	disk := filepath.Join(dir, "disk")
	legacy := filepath.Join(dir, "rootfs.img")
	_, diskErr := os.Lstat(disk)
	_, legacyErr := os.Lstat(legacy)
	if diskErr != nil && !errors.Is(diskErr, os.ErrNotExist) {
		return "", diskErr
	}
	if legacyErr != nil && !errors.Is(legacyErr, os.ErrNotExist) {
		return "", legacyErr
	}
	var image string
	switch {
	case diskErr == nil && legacyErr == nil:
		return "", fmt.Errorf("snapshot inventory: ambiguous dual disk layout at %s", dir)
	case diskErr == nil:
		ok, err := isSubvolume(disk)
		if err != nil || !ok {
			return "", fmt.Errorf("snapshot inventory: %s requires a disk subvolume (probe: %v)", dir, err)
		}
		image = filepath.Join(disk, "rootfs.img")
	case legacyErr == nil && allowLegacy:
		image = legacy
	default:
		return "", fmt.Errorf("snapshot inventory: missing disk at %s", dir)
	}
	info, err := os.Lstat(image)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return "", fmt.Errorf("snapshot inventory: missing or invalid rootfs at %s", image)
	}
	return image, nil
}

// Inventory the live tree, never the archive. Unknown subvolumes, mounts,
// symlinked managed paths and missing disks fail closed rather than silently
// producing an incomplete disaster-recovery bundle.
func inventorySnapshotDisks(root string, isSubvolume func(string) (bool, error)) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		parts := strings.Split(rel, string(filepath.Separator))
		managed := parts[0] == "instances" || parts[0] == "templates" || parts[0] == "backups" || parts[0] == "image-bases"
		depth := 3
		if parts[0] == "instances" || parts[0] == "image-bases" {
			depth = 2
		}
		storagePath := managed && (len(parts) <= depth || parts[depth] == "disk" || parts[depth] == "rootfs.img")
		if entry.Type()&os.ModeSymlink != 0 && (storagePath || rel == ".snapshots") {
			return fmt.Errorf("snapshot inventory: symlink at %s", rel)
		}
		if !entry.IsDir() {
			return nil
		}
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return err
		}
		subvol, err := isSubvolume(path)
		if err != nil {
			return err
		}
		var parent unix.Stat_t
		if err := unix.Stat(filepath.Dir(path), &parent); err != nil {
			return err
		}
		// Btrfs assigns a distinct st_dev to each subvolume. A non-subvolume
		// device boundary, however, is an unsupported mounted directory.
		if !subvol && st.Dev != parent.Dev {
			return fmt.Errorf("snapshot inventory: foreign filesystem at %s", rel)
		}
		if rel == ".snapshots" {
			if !subvol {
				return errors.New("snapshot archive is not a subvolume")
			}
			return filepath.SkipDir
		}
		if managed && len(parts) == depth {
			if parts[0] == "image-bases" {
				digest, err := hex.DecodeString(parts[1])
				if err != nil || len(digest) != 32 || parts[1] != strings.ToLower(parts[1]) {
					return fmt.Errorf("snapshot inventory: unsupported image base %s", rel)
				}
			}
			if _, err := snapshotDiskImage(path, parts[0] != "image-bases", isSubvolume); err != nil {
				return err
			}
		}
		if subvol {
			if !managed || len(parts) != depth+1 || parts[depth] != "disk" {
				return fmt.Errorf("snapshot inventory: unsupported nested subvolume %s", rel)
			}
			paths = append(paths, rel)
		}
		return nil // also inspect disk interiors for unsupported nested subvolumes
	})
	return paths, err
}

func createSnapshotBundle(ctx context.Context, dataDir, archive string, disks []string, createdAt time.Time,
	snapshot func(context.Context, string, string) error, syncFS func(string) error,
	inspectUUID func(context.Context, string) (string, error)) (dataSnapshotInfo, error) {
	id := createdAt.Format("20060102T150405.000000000Z")
	path := filepath.Join(archive, id)
	if _, err := os.Lstat(path); err == nil {
		return dataSnapshotInfo{}, fmt.Errorf("snapshot destination %s already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return dataSnapshotInfo{}, fmt.Errorf("stat snapshot destination %s: %w", path, err)
	}
	stage, err := os.MkdirTemp(archive, ".incomplete-"+id+"-")
	if err != nil {
		return dataSnapshotInfo{}, err
	}
	// Leave failures identifiable for operator cleanup; RemoveAll cannot safely
	// remove readonly subvolumes, and must never be used for archive retention.
	m := snapshotManifest{Version: 1, ID: id, CreatedAt: createdAt,
		Components: []snapshotComponent{{Source: ".", Path: "root"}}}
	for i, disk := range disks {
		m.Components = append(m.Components, snapshotComponent{Source: disk, Path: fmt.Sprintf("disk-%06d", i)})
	}
	payload, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return dataSnapshotInfo{}, err
	}
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), payload, 0o600); err != nil {
		return dataSnapshotInfo{}, err
	}
	if err := syncFS(dataDir); err != nil {
		return dataSnapshotInfo{}, err
	}
	for i := range m.Components {
		component := &m.Components[i]
		if err := ctx.Err(); err != nil {
			return dataSnapshotInfo{}, err
		}
		source, target := filepath.Join(dataDir, component.Source), filepath.Join(stage, component.Path)
		component.SourceUUID, err = inspectUUID(ctx, source)
		if err != nil || component.SourceUUID == "" {
			return dataSnapshotInfo{}, fmt.Errorf("inspect source identity %s: %v", source, err)
		}
		if err := snapshot(ctx, source, target); err != nil {
			return dataSnapshotInfo{}, fmt.Errorf("incomplete bundle %s: %w", stage, err)
		}
		component.SnapshotUUID, err = inspectUUID(ctx, target)
		if err != nil || component.SnapshotUUID == "" || component.SnapshotUUID == component.SourceUUID {
			return dataSnapshotInfo{}, fmt.Errorf("inspect snapshot identity %s: %v", target, err)
		}
		after, err := inspectUUID(ctx, source)
		if err != nil || after != component.SourceUUID {
			return dataSnapshotInfo{}, fmt.Errorf("source identity changed during snapshot: %s", source)
		}
	}
	payload, err = json.MarshalIndent(m, "", "  ")
	if err != nil {
		return dataSnapshotInfo{}, err
	}
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), payload, 0o600); err != nil {
		return dataSnapshotInfo{}, err
	}
	if err := syncFS(stage); err != nil {
		return dataSnapshotInfo{}, err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, path, unix.RENAME_NOREPLACE); err != nil {
		return dataSnapshotInfo{}, err
	}
	if err := syncFS(archive); err != nil {
		return dataSnapshotInfo{}, fmt.Errorf("bundle %s published but durability unconfirmed: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return dataSnapshotInfo{}, err
	}
	return dataSnapshotInfo{ID: id, Path: path, CreatedAt: createdAt}, nil
}

func parseSnapshotArgs(args []string) (string, error) {
	if len(args) != 2 {
		return "", errors.New(snapshotUsage())
	}
	switch args[1] {
	case "create":
		return args[1], nil
	default:
		return "", fmt.Errorf("unknown snapshot action %q\n%s", args[1], snapshotUsage())
	}
}

func snapshotUsage() string {
	return "usage: snapshot create"
}

func ensureBtrfsSubvolume(path string) error {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return fmt.Errorf("statfs %s: %w", path, err)
	}
	if fs.Type != unix.BTRFS_SUPER_MAGIC {
		return fmt.Errorf("SRV_DATA_DIR %s must be on btrfs; snapshot create requires a btrfs subvolume", path)
	}
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if st.Ino != btrfsSubvolumeRootInode {
		return fmt.Errorf("SRV_DATA_DIR %s must be a btrfs subvolume root; plain directories are not supported", path)
	}
	return nil
}

func syncPathFilesystem(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := unix.Syncfs(int(file.Fd())); err != nil {
		return err
	}
	return nil
}

func createReadonlyBtrfsSnapshot(ctx context.Context, source, target string) error {
	cmd := exec.CommandContext(ctx, "btrfs", "subvolume", "snapshot", "-r", source, target)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return fmt.Errorf("create readonly btrfs snapshot %s -> %s: %w", source, target, err)
	}
	return fmt.Errorf("create readonly btrfs snapshot %s -> %s: %w: %s", source, target, err, trimmed)
}
