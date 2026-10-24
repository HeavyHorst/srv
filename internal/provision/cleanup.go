package provision

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"srv/internal/model"
	"srv/internal/nethelper"
	"srv/internal/vmrunner"
)

func absentPath(path string) error {
	_, err := os.Lstat(path)
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cleanup path present or unreadable %s: %v", path, err)
	}
	return nil
}

// recoveryCopiesAbsent never removes backup/snapshot data. The service's name
// lock and snapshot lease exclude concurrent creation throughout deletion.
func (p *Provisioner) recoveryCopiesAbsent(inst model.Instance) (string, error) {
	backups, err := p.instanceBackupsDir(inst.Name)
	if err != nil {
		return "", err
	}
	if err := absentPath(backups); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(p.cfg.SnapshotsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			return "", errors.New("unrecognized snapshot entry; operator inspection required")
		}
		for _, sub := range []string{"instances", "backups"} {
			if err := absentPath(filepath.Join(p.cfg.SnapshotsDir(), entry.Name(), sub, inst.Name)); err != nil {
				return "", err
			}
		}
		copies, err := retainedTemplates(filepath.Join(p.cfg.SnapshotsDir(), entry.Name(), "templates"), inst)
		if err != nil {
			return "", err
		}
		if len(copies) != 0 {
			return "", errors.New("snapshot retains source templates; explicit snapshot disposition required")
		}
	}
	return backups, nil
}

// DeleteWithProof is only called under the service lifecycle lock. Failure
// after admission leaves both the request and instance fenced; it is never
// retried automatically. Only a stored committed receipt is replayable.
func (p *Provisioner) DeleteWithProof(ctx context.Context, inst model.Instance, requestID, actor, backendID string) (model.CleanupReceipt, error) {
	var r model.CleanupReceipt
	if inst.KindName() != model.KindOrb || inst.TailnetNodeID != "" || backendID == "" {
		return r, errors.New("cleanup proof requires a native orb and backend identity")
	}
	p.admissionMu.Lock()
	defer p.admissionMu.Unlock()
	if err := p.store.CheckCleanupFence(ctx, inst.ID); err != nil {
		return r, err
	}
	if migration, err := p.store.GetMigration(ctx, inst.ID); err == nil && migration.Phase != "aborted" {
		return r, errors.New("migration excludes automatic cleanup")
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return r, err
	}
	if _, err := p.store.GetImportReceipt(ctx, inst.ID); err == nil {
		return r, errors.New("import excludes automatic cleanup")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return r, err
	}
	if _, err := p.store.EnrollmentKey(ctx, inst.ID); !errors.Is(err, sql.ErrNoRows) {
		return r, errors.New("native orb enrollment history is not empty")
	}
	network, ok := p.networkHelper.(interface {
		VerifyInstanceNetworkAbsent(context.Context, nethelper.CleanupRequest) error
	})
	if !ok {
		return r, errors.New("network proof helper unavailable")
	}
	runtimeProof, ok := p.vmRunner.(interface {
		VerifyInstanceAbsent(context.Context, vmrunner.StartResultRequest) error
	})
	if !ok {
		return r, errors.New("runtime proof helper unavailable")
	}
	backups, err := p.recoveryCopiesAbsent(inst)
	if err != nil {
		return r, err
	}
	templates, err := retainedTemplates(filepath.Join(p.cfg.DataDirAbs(), "templates"), inst)
	if err != nil {
		return r, err
	}
	if err := p.store.BeginCleanup(ctx, requestID, actor, inst); err != nil {
		return r, err
	}
	if _, err := p.recoveryCopiesAbsent(inst); err != nil {
		return r, err
	}
	if err := p.stopFirecracker(ctx, inst); err != nil {
		return r, err
	}
	if err := runtimeProof.VerifyInstanceAbsent(ctx, vmrunner.StartResultRequest{Name: inst.Name, InstanceID: inst.ID, RequestID: inst.StartRequestID, HostBootID: inst.StartBootID}); err != nil {
		return r, err
	}
	if err := p.cleanupNetworking(inst); err != nil {
		return r, err
	}
	if err := network.VerifyInstanceNetworkAbsent(ctx, nethelper.CleanupRequest{TapDevice: inst.TapDevice, NetworkCIDR: inst.NetworkCIDR}); err != nil {
		return r, err
	}
	if _, err := p.recoveryCopiesAbsent(inst); err != nil {
		return r, err
	}
	if err := p.removeInstanceDir(inst.Name); err != nil {
		return r, err
	}
	// Publish no durable tombstone ahead of durable directory removal.
	if err := syncTemplatePath(p.cfg.InstancesDir()); err != nil {
		return r, err
	}
	dir := filepath.Join(p.cfg.InstancesDir(), inst.Name)
	for _, path := range []string{dir, inst.RootFSPath} {
		if err := absentPath(path); err != nil {
			return r, err
		}
	}
	r = model.CleanupReceipt{Version: 2, BackendID: backendID, Kind: inst.KindName(), Image: inst.ImageName(), RetainedTemplates: templates, RequestID: requestID, Actor: actor, InstanceID: inst.ID, Name: inst.Name, CreatedAt: inst.CreatedAt, CreatedByUser: inst.CreatedByUser, CreatedByNode: inst.CreatedByNode, AuthKeyDisposition: "never-minted", IngressClosed: true, RuntimeAbsent: true, JailerAbsent: true, CgroupAbsent: true, NetworkAbsent: true, TapDevice: inst.TapDevice, NetworkCIDR: inst.NetworkCIDR, InstanceDir: dir, RootFSPath: inst.RootFSPath, BackupsDir: backups, StorageAbsent: true, BackupsAbsent: true, SnapshotsAbsent: true, DeletedAt: time.Now().UTC()}
	if err := p.store.CommitCleanup(ctx, r); err != nil {
		return model.CleanupReceipt{}, err
	}
	return r, nil
}

// Templates intentionally outlive the source. Enumerate all owner namespaces,
// including incomplete publications; an unreadable manifest is not absence.
// The admission lock excludes template publication and deletion.
func retainedTemplates(root string, inst model.Instance) ([]string, error) {
	owners, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, owner := range owners {
		if !owner.IsDir() {
			return nil, errors.New("invalid template owner directory")
		}
		entries, err := os.ReadDir(filepath.Join(root, owner.Name()))
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				return nil, errors.New("invalid template directory")
			}
			data, err := os.ReadFile(filepath.Join(root, owner.Name(), entry.Name(), "manifest.json"))
			if err != nil {
				return nil, err
			}
			var m templateManifest
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, err
			}
			if m.Version != 1 || m.SourceID == "" || m.ID != entry.Name() || m.Owner == "" || m.Image == "" {
				return nil, errors.New("invalid retained template manifest")
			}
			if m.SourceID == inst.ID {
				ids = append(ids, owner.Name()+"/"+m.ID)
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}
