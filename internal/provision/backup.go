package provision

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"

	"srv/internal/format"
	"srv/internal/model"
)

const (
	backupManifestVersion   = 1
	portableManifestVersion = 1
	portableManifestMaxSize = 1 << 20
	portableLogMaxSize      = 256 << 20
	backupManifestName      = "manifest.json"
	backupRootFSName        = "rootfs.img"
	backupSerialLogName     = "serial.log"
	backupFirecrackerName   = "firecracker.log"
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

type PortableArtifactInfo struct {
	Name              string
	ExportedAt        time.Time
	RootFSSizeBytes   int64
	VCPUCount         int64
	MemoryMiB         int64
	HasSerialLog      bool
	HasFirecrackerLog bool
}

type ImportProgress struct {
	Name           string
	CompletedBytes int64
	TotalBytes     int64
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

type portableManifest struct {
	Version    int              `json:"version"`
	ExportedAt time.Time        `json:"exported_at"`
	Instance   portableInstance `json:"instance"`
	Files      backupFiles      `json:"files"`
}

type portableInstance struct {
	ID              string    `json:"id,omitempty"`
	Name            string    `json:"name"`
	CreatedAt       time.Time `json:"created_at"`
	CreatedByUser   string    `json:"created_by_user,omitempty"`
	CreatedByNode   string    `json:"created_by_node,omitempty"`
	VCPUCount       int64     `json:"vcpu_count"`
	MemoryMiB       int64     `json:"memory_mib"`
	RootFSSizeBytes int64     `json:"rootfs_size_bytes"`
	TailscaleName   string    `json:"tailscale_name,omitempty"`
	TailscaleIP     string    `json:"tailscale_ip,omitempty"`
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

func (p *Provisioner) ExportInstance(ctx context.Context, name string, w io.Writer) (PortableArtifactInfo, error) {
	inst, err := p.requireStoppedInstance(ctx, name)
	if err != nil {
		return PortableArtifactInfo{}, err
	}

	exportedAt := time.Now().UTC()
	manifest := portableManifestFromInstance(exportedAt, inst)
	serialPresent, err := regularFileExists(inst.SerialLogPath)
	if err != nil {
		return PortableArtifactInfo{}, fmt.Errorf("stat serial log for %q: %w", name, err)
	}
	if serialPresent {
		manifest.Files.SerialLog = backupSerialLogName
	}
	fcPresent, err := regularFileExists(inst.LogPath)
	if err != nil {
		return PortableArtifactInfo{}, fmt.Errorf("stat firecracker log for %q: %w", name, err)
	}
	if fcPresent {
		manifest.Files.FirecrackerLog = backupFirecrackerName
	}

	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return PortableArtifactInfo{}, fmt.Errorf("marshal portable artifact manifest: %w", err)
	}

	tw, zw, err := newPortableArtifactTarWriter(w)
	if err != nil {
		return PortableArtifactInfo{}, err
	}
	if err := writeTarBytes(tw, backupManifestName, payload, 0o640, exportedAt); err != nil {
		_ = closePortableArtifactTarWriter(tw, zw)
		return PortableArtifactInfo{}, fmt.Errorf("write portable artifact manifest: %w", err)
	}
	if err := writeTarFile(tw, manifest.Files.RootFS, inst.RootFSPath, 0o660); err != nil {
		_ = closePortableArtifactTarWriter(tw, zw)
		return PortableArtifactInfo{}, fmt.Errorf("write rootfs to portable artifact: %w", err)
	}
	serialTruncated := false
	if manifest.Files.SerialLog != "" {
		serialTruncated, err = writeTarFileTail(tw, manifest.Files.SerialLog, inst.SerialLogPath, 0o660, portableLogMaxSize)
		if err != nil {
			_ = closePortableArtifactTarWriter(tw, zw)
			return PortableArtifactInfo{}, fmt.Errorf("write serial log to portable artifact: %w", err)
		}
	}
	firecrackerTruncated := false
	if manifest.Files.FirecrackerLog != "" {
		firecrackerTruncated, err = writeTarFileTail(tw, manifest.Files.FirecrackerLog, inst.LogPath, 0o660, portableLogMaxSize)
		if err != nil {
			_ = closePortableArtifactTarWriter(tw, zw)
			return PortableArtifactInfo{}, fmt.Errorf("write firecracker log to portable artifact: %w", err)
		}
	}
	if err := closePortableArtifactTarWriter(tw, zw); err != nil {
		return PortableArtifactInfo{}, fmt.Errorf("finalize portable artifact stream: %w", err)
	}

	info := portableArtifactInfoFromManifest(manifest)
	p.recordEvent(inst.ID, "export", "instance exported as portable artifact", map[string]any{
		"exported_at":               info.ExportedAt,
		"has_serial_log":            info.HasSerialLog,
		"has_firecracker_log":       info.HasFirecrackerLog,
		"rootfs_size_bytes":         info.RootFSSizeBytes,
		"serial_log_truncated":      serialTruncated,
		"firecracker_log_truncated": firecrackerTruncated,
	})
	return info, nil
}

func PeekPortableArtifactInfo(r io.Reader) (PortableArtifactInfo, io.Reader, error) {
	var prefix bytes.Buffer
	tr, zr, err := newPortableArtifactTarReader(io.TeeReader(r, &prefix), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return PortableArtifactInfo{}, nil, err
	}
	manifest, err := readPortableManifest(tr)
	if err != nil {
		zr.Close()
		return PortableArtifactInfo{}, nil, err
	}
	zr.Close()
	replayPrefix := append([]byte(nil), prefix.Bytes()...)
	return portableArtifactInfoFromManifest(manifest), io.MultiReader(bytes.NewReader(replayPrefix), r), nil
}

func (p *Provisioner) ImportInstance(ctx context.Context, actor model.Actor, r io.Reader, progressFns ...func(ImportProgress)) (model.Instance, PortableArtifactInfo, error) {
	tr, zr, err := newPortableArtifactTarReader(r)
	if err != nil {
		return model.Instance{}, PortableArtifactInfo{}, err
	}
	defer zr.Close()
	manifest, err := readPortableManifest(tr)
	if err != nil {
		return model.Instance{}, PortableArtifactInfo{}, err
	}
	var progress func(ImportProgress)
	if len(progressFns) > 0 {
		progress = progressFns[0]
	}
	info := portableArtifactInfoFromManifest(manifest)

	targetName := manifest.Instance.Name
	if !validName.MatchString(targetName) {
		return model.Instance{}, PortableArtifactInfo{}, fmt.Errorf("invalid instance name %q", targetName)
	}
	if manifest.Instance.RootFSSizeBytes <= 0 {
		return model.Instance{}, PortableArtifactInfo{}, errors.New("portable artifact manifest is missing rootfs_size_bytes")
	}
	resolvedVCPUCount := p.effectiveVCPUCount(model.Instance{VCPUCount: manifest.Instance.VCPUCount})
	resolvedMemoryMiB := p.effectiveMemoryMiB(model.Instance{MemoryMiB: manifest.Instance.MemoryMiB})
	if err := validateMachineShape(resolvedVCPUCount, resolvedMemoryMiB); err != nil {
		return model.Instance{}, PortableArtifactInfo{}, fmt.Errorf("portable artifact machine shape: %w", err)
	}

	var (
		instanceDir string
		imported    model.Instance
	)
	if err := func() error {
		p.admissionMu.Lock()
		defer p.admissionMu.Unlock()

		if err := p.ensureHostDiskCapacity(ctx, "", manifest.Instance.RootFSSizeBytes); err != nil {
			return err
		}
		preparedDir, err := p.prepareInstanceDir(ctx, targetName)
		if err != nil {
			return err
		}
		instanceDir = preparedDir

		networkCIDR, hostAddr, guestAddr, gateway, err := p.allocateNetwork(ctx)
		if err != nil {
			_ = os.RemoveAll(instanceDir)
			return err
		}

		now := time.Now().UTC()
		imported = model.Instance{
			ID:              firstNonEmpty(manifest.Instance.ID, uuid.NewString()),
			Name:            targetName,
			State:           model.StateProvisioning,
			CreatedAt:       manifest.Instance.CreatedAt,
			UpdatedAt:       now,
			CreatedByUser:   firstNonEmpty(manifest.Instance.CreatedByUser, actor.UserLogin),
			CreatedByNode:   firstNonEmpty(manifest.Instance.CreatedByNode, actor.NodeName),
			VCPUCount:       resolvedVCPUCount,
			MemoryMiB:       resolvedMemoryMiB,
			RootFSSizeBytes: manifest.Instance.RootFSSizeBytes,
			RootFSPath:      filepath.Join(instanceDir, backupRootFSName),
			KernelPath:      p.cfg.BaseKernelPath,
			InitrdPath:      p.cfg.BaseInitrdPath,
			SocketPath:      filepath.Join(instanceDir, "firecracker.sock"),
			LogPath:         filepath.Join(instanceDir, backupFirecrackerName),
			SerialLogPath:   filepath.Join(instanceDir, backupSerialLogName),
			TapDevice:       tapName(targetName),
			GuestMAC:        guestMAC(targetName),
			NetworkCIDR:     networkCIDR,
			HostAddr:        hostAddr,
			GuestAddr:       guestAddr,
			GatewayAddr:     gateway,
			TailscaleName:   manifest.Instance.TailscaleName,
			TailscaleIP:     manifest.Instance.TailscaleIP,
		}
		if err := p.store.CreateInstance(ctx, imported); err != nil {
			_ = os.RemoveAll(instanceDir)
			return fmt.Errorf("create imported instance %q reservation: %w", targetName, err)
		}
		return nil
	}(); err != nil {
		return model.Instance{}, PortableArtifactInfo{}, err
	}

	cleanupReservation := true
	defer func() {
		if !cleanupReservation {
			return
		}
		_ = p.store.DeleteInstance(context.Background(), imported.Name)
		_ = os.RemoveAll(instanceDir)
	}()

	staged := make([]stagedRestoreFile, 0, 3)
	cleanupStaged := func() {
		for _, file := range staged {
			_ = os.Remove(file.tmp)
		}
	}
	defer cleanupStaged()

	targets := map[string]struct {
		dest     string
		label    string
		required bool
	}{
		manifest.Files.RootFS: {dest: imported.RootFSPath, label: "rootfs", required: true},
	}
	if manifest.Files.SerialLog != "" {
		targets[manifest.Files.SerialLog] = struct {
			dest     string
			label    string
			required bool
		}{dest: imported.SerialLogPath, label: "serial log", required: true}
	}
	if manifest.Files.FirecrackerLog != "" {
		targets[manifest.Files.FirecrackerLog] = struct {
			dest     string
			label    string
			required bool
		}{dest: imported.LogPath, label: "firecracker log", required: true}
	}

	seen := make(map[string]bool, len(targets))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return model.Instance{}, PortableArtifactInfo{}, fmt.Errorf("read portable artifact entry: %w", err)
		}
		target, ok := targets[hdr.Name]
		if !ok {
			return model.Instance{}, PortableArtifactInfo{}, fmt.Errorf("portable artifact contains unexpected entry %q", hdr.Name)
		}
		if seen[hdr.Name] {
			return model.Instance{}, PortableArtifactInfo{}, fmt.Errorf("portable artifact entry %q appears more than once", hdr.Name)
		}
		if !isRegularTarEntry(hdr) {
			return model.Instance{}, PortableArtifactInfo{}, fmt.Errorf("portable artifact entry %q is not a regular file", hdr.Name)
		}
		if hdr.Size < 0 {
			return model.Instance{}, PortableArtifactInfo{}, fmt.Errorf("portable artifact entry %q has invalid size %d", hdr.Name, hdr.Size)
		}
		if hdr.Name == manifest.Files.RootFS && hdr.Size != imported.RootFSSizeBytes {
			return model.Instance{}, PortableArtifactInfo{}, fmt.Errorf(
				"portable artifact rootfs size %d does not match manifest size %d",
				hdr.Size,
				imported.RootFSSizeBytes,
			)
		}
		if hdr.Name != manifest.Files.RootFS && hdr.Size > portableLogMaxSize {
			return model.Instance{}, PortableArtifactInfo{}, fmt.Errorf(
				"portable artifact %s is too large: %s exceeds %s",
				target.label,
				format.BinarySize(hdr.Size),
				format.BinarySize(portableLogMaxSize),
			)
		}
		entryReader := io.Reader(tr)
		if progress != nil {
			progress(ImportProgress{Name: hdr.Name, TotalBytes: hdr.Size})
			entryReader = &importProgressReader{
				src:      tr,
				name:     hdr.Name,
				total:    hdr.Size,
				progress: progress,
			}
		}
		stagedFile, err := prepareStagedRestoreFileFromReader(entryReader, target.dest, 0o660)
		if err != nil {
			return model.Instance{}, PortableArtifactInfo{}, fmt.Errorf("stage %s from portable artifact: %w", target.label, err)
		}
		staged = append(staged, stagedFile)
		seen[hdr.Name] = true
	}

	for name, target := range targets {
		if target.required && !seen[name] {
			return model.Instance{}, PortableArtifactInfo{}, fmt.Errorf("portable artifact is missing %s entry %q", target.label, name)
		}
	}

	if _, err := commitStagedRestoreFiles(staged); err != nil {
		return model.Instance{}, PortableArtifactInfo{}, fmt.Errorf("commit portable artifact files for %q: %w", targetName, err)
	}
	imported.State = model.StateStopped
	imported.FirecrackerPID = 0
	imported.LastError = ""
	imported.DeletedAt = nil
	imported.UpdatedAt = time.Now().UTC()
	if err := p.store.UpdateInstance(ctx, imported); err != nil {
		return model.Instance{}, PortableArtifactInfo{}, fmt.Errorf("finalize imported instance %q: %w", targetName, err)
	}

	cleanupReservation = false
	p.recordEvent(imported.ID, "import", "instance imported from portable artifact", map[string]any{
		"source_name":         manifest.Instance.Name,
		"source_instance_id":  manifest.Instance.ID,
		"exported_at":         info.ExportedAt,
		"has_serial_log":      info.HasSerialLog,
		"has_firecracker_log": info.HasFirecrackerLog,
	})
	return imported, info, nil
}

func portableManifestFromInstance(exportedAt time.Time, inst model.Instance) portableManifest {
	return portableManifest{
		Version:    portableManifestVersion,
		ExportedAt: exportedAt,
		Instance: portableInstance{
			ID:              inst.ID,
			Name:            inst.Name,
			CreatedAt:       inst.CreatedAt,
			CreatedByUser:   inst.CreatedByUser,
			CreatedByNode:   inst.CreatedByNode,
			VCPUCount:       inst.VCPUCount,
			MemoryMiB:       inst.MemoryMiB,
			RootFSSizeBytes: inst.RootFSSizeBytes,
			TailscaleName:   inst.TailscaleName,
			TailscaleIP:     inst.TailscaleIP,
		},
		Files: backupFiles{RootFS: backupRootFSName},
	}
}

func portableArtifactInfoFromManifest(manifest portableManifest) PortableArtifactInfo {
	return PortableArtifactInfo{
		Name:              manifest.Instance.Name,
		ExportedAt:        manifest.ExportedAt,
		RootFSSizeBytes:   manifest.Instance.RootFSSizeBytes,
		VCPUCount:         manifest.Instance.VCPUCount,
		MemoryMiB:         manifest.Instance.MemoryMiB,
		HasSerialLog:      manifest.Files.SerialLog != "",
		HasFirecrackerLog: manifest.Files.FirecrackerLog != "",
	}
}

func readPortableManifest(tr *tar.Reader) (portableManifest, error) {
	hdr, err := tr.Next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return portableManifest{}, errors.New("portable artifact stream is empty")
		}
		return portableManifest{}, fmt.Errorf("read portable artifact manifest: %w", err)
	}
	if hdr.Name != backupManifestName {
		return portableManifest{}, fmt.Errorf("portable artifact must begin with %s, found %q", backupManifestName, hdr.Name)
	}
	if !isRegularTarEntry(hdr) {
		return portableManifest{}, errors.New("portable artifact manifest is not a regular file")
	}
	if hdr.Size <= 0 {
		return portableManifest{}, errors.New("portable artifact manifest is empty")
	}
	if hdr.Size > portableManifestMaxSize {
		return portableManifest{}, fmt.Errorf("portable artifact manifest is too large: %d bytes", hdr.Size)
	}
	payload, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
	if err != nil {
		return portableManifest{}, fmt.Errorf("read portable artifact manifest payload: %w", err)
	}
	var manifest portableManifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		return portableManifest{}, fmt.Errorf("parse portable artifact manifest: %w", err)
	}
	if err := manifest.validate(); err != nil {
		return portableManifest{}, err
	}
	return manifest, nil
}

func (m portableManifest) validate() error {
	if m.Version != portableManifestVersion {
		return fmt.Errorf("portable artifact uses unsupported manifest version %d", m.Version)
	}
	if m.ExportedAt.IsZero() {
		return errors.New("portable artifact manifest is missing exported_at")
	}
	if m.Instance.Name == "" {
		return errors.New("portable artifact manifest is missing instance name")
	}
	if m.Instance.CreatedAt.IsZero() {
		return errors.New("portable artifact manifest is missing instance created_at")
	}
	if m.Files.RootFS == "" {
		return errors.New("portable artifact manifest is missing a rootfs entry")
	}
	if m.Files.RootFS != backupRootFSName {
		return fmt.Errorf("portable artifact rootfs entry must be %q, got %q", backupRootFSName, m.Files.RootFS)
	}
	if m.Files.SerialLog != "" && m.Files.SerialLog != backupSerialLogName {
		return fmt.Errorf("portable artifact serial log entry must be %q, got %q", backupSerialLogName, m.Files.SerialLog)
	}
	if m.Files.FirecrackerLog != "" && m.Files.FirecrackerLog != backupFirecrackerName {
		return fmt.Errorf("portable artifact firecracker log entry must be %q, got %q", backupFirecrackerName, m.Files.FirecrackerLog)
	}
	return nil
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if info.IsDir() {
		return false, fmt.Errorf("%s is a directory", path)
	}
	return true, nil
}

func newPortableArtifactTarWriter(w io.Writer) (*tar.Writer, *zstd.Encoder, error) {
	zw, err := zstd.NewWriter(w)
	if err != nil {
		return nil, nil, fmt.Errorf("create portable artifact compressor: %w", err)
	}
	return tar.NewWriter(zw), zw, nil
}

func closePortableArtifactTarWriter(tw *tar.Writer, zw *zstd.Encoder) error {
	if err := tw.Close(); err != nil {
		_ = zw.Close()
		return err
	}
	return zw.Close()
}

func newPortableArtifactTarReader(r io.Reader, opts ...zstd.DOption) (*tar.Reader, *zstd.Decoder, error) {
	zr, err := zstd.NewReader(r, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("open portable artifact stream: %w", err)
	}
	return tar.NewReader(zr), zr, nil
}

func writeTarBytes(tw *tar.Writer, name string, payload []byte, mode os.FileMode, modTime time.Time) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:    name,
		Mode:    int64(mode),
		Size:    int64(len(payload)),
		ModTime: modTime,
	}); err != nil {
		return err
	}
	_, err := tw.Write(payload)
	return err
}

func writeTarFile(tw *tar.Writer, name, path string, mode os.FileMode) error {
	_, err := writeTarFileTail(tw, name, path, mode, 0)
	return err
}

func writeTarFileTail(tw *tar.Writer, name, path string, mode os.FileMode, maxBytes int64) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if info.IsDir() {
		return false, fmt.Errorf("%s is a directory", path)
	}

	size := info.Size()
	offset := int64(0)
	truncated := false
	if maxBytes > 0 && size > maxBytes {
		offset = size - maxBytes
		size = maxBytes
		truncated = true
	}
	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return false, err
		}
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:    name,
		Mode:    int64(mode),
		Size:    size,
		ModTime: info.ModTime(),
	}); err != nil {
		return false, err
	}
	if _, err := io.CopyN(tw, file, size); err != nil {
		return false, err
	}
	return truncated, nil
}

func isRegularTarEntry(hdr *tar.Header) bool {
	return hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA || hdr.Typeflag == 0
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

func prepareStagedRestoreFileFromReader(src io.Reader, dest string, mode os.FileMode) (stagedRestoreFile, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o770); err != nil {
		return stagedRestoreFile{}, fmt.Errorf("create destination directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".srv-restore-*")
	if err != nil {
		return stagedRestoreFile{}, fmt.Errorf("create temporary restore file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return stagedRestoreFile{}, fmt.Errorf("write temporary restore file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return stagedRestoreFile{}, fmt.Errorf("close temporary restore file: %w", err)
	}
	if mode != 0 {
		if err := os.Chmod(tmpPath, mode); err != nil {
			_ = os.Remove(tmpPath)
			return stagedRestoreFile{}, fmt.Errorf("set restored file permissions: %w", err)
		}
	}
	return stagedRestoreFile{dest: dest, tmp: tmpPath}, nil
}

type importProgressReader struct {
	src       io.Reader
	name      string
	total     int64
	completed int64
	progress  func(ImportProgress)
}

func (r *importProgressReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		r.completed += int64(n)
		r.progress(ImportProgress{Name: r.name, CompletedBytes: r.completed, TotalBytes: r.total})
	}
	return n, err
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
