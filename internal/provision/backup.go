package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"srv/internal/model"
)

const (
	backupManifestVersion = 1
	backupManifestName    = "manifest.json"
	backupRootFSName      = "rootfs.img"
	backupSerialLogName   = "serial.log"
	backupFirecrackerName = "firecracker.log"
)

type BackupInfo struct {
	ID                string
	Name              string
	CreatedAt         time.Time
	Path              string
	RootFSSizeBytes   int64
	VCPUCount         int64
	MemoryMiB         int64
	HasSerialLog      bool
	HasFirecrackerLog bool
}

type backupManifest struct {
	Version   int            `json:"version"`
	ID        string         `json:"id"`
	CreatedAt time.Time      `json:"created_at"`
	Instance  model.Instance `json:"instance"`
	Files     backupFiles    `json:"files"`
}

type backupFiles struct {
	RootFS         string `json:"rootfs"`
	SerialLog      string `json:"serial_log,omitempty"`
	FirecrackerLog string `json:"firecracker_log,omitempty"`
}

type stagedRestoreFile struct {
	dest string
	tmp  string
}

type replacedRestoreFile struct {
	dest     string
	original string
	hadFile  bool
}

func (p *Provisioner) CreateBackup(ctx context.Context, name string) (BackupInfo, error) {
	inst, err := p.requireStoppedInstance(ctx, name)
	if err != nil {
		return BackupInfo{}, err
	}

	createdAt := time.Now().UTC()
	backupID := createdAt.Format("20060102T150405.000000000Z")
	backupDir, err := p.backupDir(name, backupID)
	if err != nil {
		return BackupInfo{}, err
	}
	if err := os.MkdirAll(filepath.Dir(backupDir), 0o770); err != nil {
		return BackupInfo{}, fmt.Errorf("create backups directory: %w", err)
	}
	if err := os.Mkdir(backupDir, 0o770); err != nil {
		return BackupInfo{}, fmt.Errorf("create backup directory %s: %w", backupDir, err)
	}

	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(backupDir)
		}
	}()

	manifest := backupManifest{
		Version:   backupManifestVersion,
		ID:        backupID,
		CreatedAt: createdAt,
		Instance:  inst,
		Files: backupFiles{
			RootFS: backupRootFSName,
		},
	}
	if err := reflinkCloneFile(ctx, inst.RootFSPath, filepath.Join(backupDir, manifest.Files.RootFS)); err != nil {
		return BackupInfo{}, fmt.Errorf("backup rootfs for %q: %w", name, err)
	}
	if err := os.Chmod(filepath.Join(backupDir, manifest.Files.RootFS), 0o660); err != nil {
		return BackupInfo{}, fmt.Errorf("set backup rootfs permissions: %w", err)
	}

	serialCopied, err := cloneBackupFileIfPresent(ctx, inst.SerialLogPath, filepath.Join(backupDir, backupSerialLogName))
	if err != nil {
		return BackupInfo{}, fmt.Errorf("backup serial log for %q: %w", name, err)
	}
	if serialCopied {
		manifest.Files.SerialLog = backupSerialLogName
	}
	fcCopied, err := cloneBackupFileIfPresent(ctx, inst.LogPath, filepath.Join(backupDir, backupFirecrackerName))
	if err != nil {
		return BackupInfo{}, fmt.Errorf("backup firecracker log for %q: %w", name, err)
	}
	if fcCopied {
		manifest.Files.FirecrackerLog = backupFirecrackerName
	}

	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return BackupInfo{}, fmt.Errorf("marshal backup manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, backupManifestName), payload, 0o640); err != nil {
		return BackupInfo{}, fmt.Errorf("write backup manifest: %w", err)
	}

	cleanup = false
	info := backupInfoFromManifest(backupDir, manifest)
	p.recordEvent(inst.ID, "backup", "instance backup created", map[string]any{"backup_id": info.ID, "path": info.Path})
	return info, nil
}

func (p *Provisioner) ListBackups(_ context.Context, name string) ([]BackupInfo, error) {
	instanceBackupsDir, err := p.instanceBackupsDir(name)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(instanceBackupsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list backups for %q: %w", name, err)
	}

	backups := make([]BackupInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifest, backupDir, err := p.readBackupManifest(name, entry.Name())
		if err != nil {
			if p.log != nil {
				p.log.Warn("skip unreadable backup manifest", "name", name, "backup_id", entry.Name(), "err", err)
			}
			continue
		}
		backups = append(backups, backupInfoFromManifest(backupDir, manifest))
	}

	sort.Slice(backups, func(i, j int) bool {
		if backups[i].CreatedAt.Equal(backups[j].CreatedAt) {
			return backups[i].ID > backups[j].ID
		}
		return backups[i].CreatedAt.After(backups[j].CreatedAt)
	})
	return backups, nil
}

func (p *Provisioner) RestoreBackup(ctx context.Context, name, backupID string) (model.Instance, BackupInfo, error) {
	inst, err := p.requireStoppedInstance(ctx, name)
	if err != nil {
		return model.Instance{}, BackupInfo{}, err
	}

	manifest, backupDir, err := p.readBackupManifest(name, backupID)
	if err != nil {
		return model.Instance{}, BackupInfo{}, err
	}
	if manifest.Instance.Name != name {
		return model.Instance{}, BackupInfo{}, fmt.Errorf("backup %q belongs to instance %q, not %q", backupID, manifest.Instance.Name, name)
	}
	if manifest.Instance.ID != inst.ID {
		return model.Instance{}, BackupInfo{}, fmt.Errorf("backup %q belongs to a different instance record for %q; restoring onto a recreated VM is not supported", backupID, name)
	}

	staged := make([]stagedRestoreFile, 0, 3)
	cleanupStaged := func() {
		for _, file := range staged {
			_ = os.Remove(file.tmp)
		}
	}
	defer cleanupStaged()

	for _, target := range []struct {
		backupPath string
		dest       string
		label      string
	}{
		{backupPath: manifest.Files.SerialLog, dest: inst.SerialLogPath, label: "serial log"},
		{backupPath: manifest.Files.FirecrackerLog, dest: inst.LogPath, label: "firecracker log"},
		{backupPath: manifest.Files.RootFS, dest: inst.RootFSPath, label: "rootfs"},
	} {
		if target.backupPath == "" {
			continue
		}
		stagedFile, err := prepareStagedRestoreFile(ctx, filepath.Join(backupDir, target.backupPath), target.dest, 0o660)
		if err != nil {
			return model.Instance{}, BackupInfo{}, fmt.Errorf("restore %s for %q from backup %q: %w", target.label, name, backupID, err)
		}
		staged = append(staged, stagedFile)
	}

	restored := inst
	restored.VCPUCount = manifest.Instance.VCPUCount
	restored.MemoryMiB = manifest.Instance.MemoryMiB
	restored.RootFSSizeBytes = manifest.Instance.RootFSSizeBytes
	restored.TailscaleName = manifest.Instance.TailscaleName
	restored.TailscaleIP = manifest.Instance.TailscaleIP
	restored.State = model.StateStopped
	restored.FirecrackerPID = 0
	restored.LastError = ""
	restored.DeletedAt = nil
	restored.UpdatedAt = time.Now().UTC()
	replaced, err := commitStagedRestoreFiles(staged)
	if err != nil {
		return model.Instance{}, BackupInfo{}, fmt.Errorf("restore files for %q from backup %q: %w", name, backupID, err)
	}
	if err := p.store.UpdateInstance(ctx, restored); err != nil {
		rollbackErr := rollbackCommittedRestoreFiles(replaced)
		cleanupErr := cleanupReplacedRestoreFiles(replaced)
		if rollbackErr != nil || cleanupErr != nil {
			return model.Instance{}, BackupInfo{}, fmt.Errorf("update instance after restore: %w (file rollback failed: %v)", err, errors.Join(rollbackErr, cleanupErr))
		}
		return model.Instance{}, BackupInfo{}, fmt.Errorf("update instance after restore: %w", err)
	}
	if cleanupErr := cleanupReplacedRestoreFiles(replaced); cleanupErr != nil && p.log != nil {
		p.log.Warn("cleanup restore rollback files", "name", name, "backup_id", backupID, "err", cleanupErr)
	}

	info := backupInfoFromManifest(backupDir, manifest)
	p.recordEvent(restored.ID, "backup", "instance restored from backup", map[string]any{"backup_id": info.ID, "path": info.Path})
	return restored, info, nil
}

func (p *Provisioner) requireStoppedInstance(ctx context.Context, name string) (model.Instance, error) {
	inst, err := p.store.GetInstance(ctx, name)
	if err != nil {
		return model.Instance{}, err
	}
	if inst.State == model.StateDeleted {
		return model.Instance{}, fmt.Errorf("instance %q is deleted", name)
	}
	if inst.FirecrackerPID > 0 && processExists(inst.FirecrackerPID) {
		return model.Instance{}, fmt.Errorf("instance %q must be stopped before backup or restore", name)
	}
	if inst.FirecrackerPID != 0 {
		inst.FirecrackerPID = 0
		inst.UpdatedAt = time.Now().UTC()
		if err := p.store.UpdateInstance(ctx, inst); err != nil {
			return model.Instance{}, fmt.Errorf("clear stale firecracker pid for %q: %w", name, err)
		}
	}
	if inst.State != model.StateStopped {
		return model.Instance{}, fmt.Errorf("instance %q must be stopped before backup or restore (current state: %s)", name, inst.State)
	}
	if inst.RootFSPath == "" {
		return model.Instance{}, fmt.Errorf("instance %q is missing a rootfs path", name)
	}
	if _, err := os.Stat(inst.RootFSPath); err != nil {
		return model.Instance{}, fmt.Errorf("stat rootfs %s: %w", inst.RootFSPath, err)
	}
	return inst, nil
}

func (p *Provisioner) instanceBackupsDir(name string) (string, error) {
	instanceBackupsDir, err := directChildPath(p.cfg.BackupsDir(), name)
	if err != nil {
		return "", fmt.Errorf("resolve backups directory for %q: %w", name, err)
	}
	return instanceBackupsDir, nil
}

func (p *Provisioner) backupDir(name, backupID string) (string, error) {
	instanceBackupsDir, err := p.instanceBackupsDir(name)
	if err != nil {
		return "", err
	}
	backupDir, err := directChildPath(instanceBackupsDir, backupID)
	if err != nil {
		return "", fmt.Errorf("resolve backup directory for %q backup %q: %w", name, backupID, err)
	}
	return backupDir, nil
}

func (p *Provisioner) readBackupManifest(name, backupID string) (backupManifest, string, error) {
	backupDir, err := p.backupDir(name, backupID)
	if err != nil {
		return backupManifest{}, "", err
	}
	payload, err := os.ReadFile(filepath.Join(backupDir, backupManifestName))
	if err != nil {
		return backupManifest{}, "", fmt.Errorf("read backup manifest for %q backup %q: %w", name, backupID, err)
	}
	var manifest backupManifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		return backupManifest{}, "", fmt.Errorf("parse backup manifest for %q backup %q: %w", name, backupID, err)
	}
	if manifest.Version != backupManifestVersion {
		return backupManifest{}, "", fmt.Errorf("backup %q uses unsupported manifest version %d", backupID, manifest.Version)
	}
	if manifest.Files.RootFS == "" {
		return backupManifest{}, "", fmt.Errorf("backup %q is missing a rootfs entry", backupID)
	}
	return manifest, backupDir, nil
}

func backupInfoFromManifest(path string, manifest backupManifest) BackupInfo {
	return BackupInfo{
		ID:                manifest.ID,
		Name:              manifest.Instance.Name,
		CreatedAt:         manifest.CreatedAt,
		Path:              path,
		RootFSSizeBytes:   manifest.Instance.RootFSSizeBytes,
		VCPUCount:         manifest.Instance.VCPUCount,
		MemoryMiB:         manifest.Instance.MemoryMiB,
		HasSerialLog:      manifest.Files.SerialLog != "",
		HasFirecrackerLog: manifest.Files.FirecrackerLog != "",
	}
}

func cloneBackupFileIfPresent(ctx context.Context, src, dest string) (bool, error) {
	info, err := os.Stat(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if info.IsDir() {
		return false, fmt.Errorf("source %s is a directory", src)
	}
	if err := reflinkCloneFile(ctx, src, dest); err != nil {
		return false, err
	}
	return true, nil
}

func prepareStagedRestoreFile(ctx context.Context, src, dest string, mode os.FileMode) (stagedRestoreFile, error) {
	if _, err := os.Stat(src); err != nil {
		return stagedRestoreFile{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o770); err != nil {
		return stagedRestoreFile{}, fmt.Errorf("create destination directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".srv-restore-*")
	if err != nil {
		return stagedRestoreFile{}, fmt.Errorf("create temporary restore file: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return stagedRestoreFile{}, fmt.Errorf("close temporary restore file: %w", err)
	}
	if err := reflinkCloneFile(ctx, src, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return stagedRestoreFile{}, err
	}
	if mode != 0 {
		if err := os.Chmod(tmpPath, mode); err != nil {
			_ = os.Remove(tmpPath)
			return stagedRestoreFile{}, fmt.Errorf("set restored file permissions: %w", err)
		}
	}
	return stagedRestoreFile{dest: dest, tmp: tmpPath}, nil
}

func commitStagedRestoreFiles(files []stagedRestoreFile) ([]replacedRestoreFile, error) {
	replaced := make([]replacedRestoreFile, 0, len(files))

	for _, file := range files {
		replacedFile, err := moveAsideRestoreDestination(file.dest)
		if err != nil {
			return nil, errors.Join(err, rollbackCommittedRestoreFiles(replaced), cleanupReplacedRestoreFiles(replaced))
		}
		if err := os.Rename(file.tmp, file.dest); err != nil {
			return nil, errors.Join(
				fmt.Errorf("replace %s: %w", file.dest, err),
				cleanupReplacedRestoreFiles([]replacedRestoreFile{replacedFile}),
				rollbackCommittedRestoreFiles(replaced),
				cleanupReplacedRestoreFiles(replaced),
			)
		}
		replaced = append(replaced, replacedFile)
	}
	return replaced, nil
}

func rollbackCommittedRestoreFiles(files []replacedRestoreFile) error {
	var errs []error
	for i := len(files) - 1; i >= 0; i-- {
		file := files[i]
		if err := os.Remove(file.dest); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove restored destination %s: %w", file.dest, err))
			continue
		}
		if file.hadFile {
			if err := os.Rename(file.original, file.dest); err != nil {
				errs = append(errs, fmt.Errorf("restore original destination %s: %w", file.dest, err))
			}
		}
	}
	return errors.Join(errs...)
}

func cleanupReplacedRestoreFiles(files []replacedRestoreFile) error {
	var errs []error
	for _, file := range files {
		if !file.hadFile {
			continue
		}
		if err := os.Remove(file.original); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove rollback copy %s: %w", file.original, err))
		}
	}
	return errors.Join(errs...)
}

func moveAsideRestoreDestination(dest string) (replacedRestoreFile, error) {
	info, err := os.Stat(dest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return replacedRestoreFile{dest: dest}, nil
		}
		return replacedRestoreFile{}, fmt.Errorf("stat existing destination %s: %w", dest, err)
	}
	if info.IsDir() {
		return replacedRestoreFile{}, fmt.Errorf("existing destination %s is a directory", dest)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".srv-restore-old-*")
	if err != nil {
		return replacedRestoreFile{}, fmt.Errorf("reserve rollback path for %s: %w", dest, err)
	}
	original := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(original)
		return replacedRestoreFile{}, fmt.Errorf("close rollback path for %s: %w", dest, err)
	}
	if err := os.Remove(original); err != nil && !errors.Is(err, os.ErrNotExist) {
		return replacedRestoreFile{}, fmt.Errorf("prepare rollback path for %s: %w", dest, err)
	}
	if err := os.Link(dest, original); err != nil {
		return replacedRestoreFile{}, fmt.Errorf("link existing destination %s for rollback: %w", dest, err)
	}
	return replacedRestoreFile{dest: dest, original: original, hadFile: true}, nil
}
