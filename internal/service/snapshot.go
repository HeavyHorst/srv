package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
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
		"snapshot-created: %s\npath: %s\ncreated-at: %s\nconsistency: control-plane consistent; stopped guests fully safe; running guests crash-consistent\n",
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
	if err := os.MkdirAll(a.cfg.SnapshotsDir(), 0o755); err != nil {
		return dataSnapshotInfo{}, fmt.Errorf("create snapshots directory %s: %w", a.cfg.SnapshotsDir(), err)
	}
	if err := a.store.Checkpoint(ctx); err != nil {
		return dataSnapshotInfo{}, fmt.Errorf("checkpoint sqlite before snapshot: %w", err)
	}
	if err := syncPathFilesystem(dataDir); err != nil {
		return dataSnapshotInfo{}, fmt.Errorf("sync filesystem for %s: %w", dataDir, err)
	}

	createdAt := snapshotNow()
	id := createdAt.Format("20060102T150405.000000000Z")
	path := filepath.Join(a.cfg.SnapshotsDir(), id)
	if _, err := os.Stat(path); err == nil {
		return dataSnapshotInfo{}, fmt.Errorf("snapshot destination %s already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return dataSnapshotInfo{}, fmt.Errorf("stat snapshot destination %s: %w", path, err)
	}
	if err := createReadonlyBtrfsSnapshot(ctx, dataDir, path); err != nil {
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
