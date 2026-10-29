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
	"strings"
	"time"

	"srv/internal/model"
	"srv/internal/nethelper"
	"srv/internal/storage"
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
func (p *Provisioner) recoveryCopiesAbsent(ctx context.Context, inst model.Instance) (string, error) {
	if err := p.coldDiskFence(ctx, inst, true); err != nil {
		return "", err
	}
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
		root := filepath.Join(p.cfg.SnapshotsDir(), entry.Name())
		if err := storage.ValidatePath(root); err != nil {
			return "", err
		}
		data, manifestErr := os.ReadFile(filepath.Join(root, "manifest.json"))
		if manifestErr == nil {
			var m struct {
				Version    int    `json:"version"`
				ID         string `json:"id"`
				Components []struct {
					Source string `json:"source"`
					Path   string `json:"path"`
				} `json:"components"`
			}
			if err := json.Unmarshal(data, &m); err != nil {
				return "", err
			}
			if m.Version != 1 || m.ID != entry.Name() || len(m.Components) == 0 || m.Components[0].Source != "." || m.Components[0].Path != "root" {
				return "", errors.New("incomplete or invalid snapshot bundle; explicit disposition required")
			}
			contents, err := os.ReadDir(root)
			if err != nil {
				return "", err
			}
			if len(contents) != len(m.Components)+1 {
				return "", errors.New("snapshot bundle has unmanifested or missing components")
			}
			seen := make(map[string]bool)
			for n, component := range m.Components {
				if n == 0 {
					continue
				}
				if filepath.IsAbs(component.Source) || filepath.Clean(component.Source) != component.Source || strings.HasPrefix(component.Source, "../") || component.Path != fmt.Sprintf("disk-%06d", n-1) {
					return "", errors.New("invalid snapshot component")
				}
				parts := strings.Split(component.Source, "/")
				valid := len(parts) == 3 && (parts[0] == "instances" || parts[0] == "image-bases") && parts[2] == "disk"
				valid = valid || (len(parts) == 4 && (parts[0] == "templates" || parts[0] == "backups") && parts[3] == "disk")
				if !valid || seen[component.Source] {
					return "", errors.New("unknown or duplicate snapshot component source")
				}
				seen[component.Source] = true
				if strings.HasPrefix(component.Source, "instances/"+inst.Name+"/") || strings.HasPrefix(component.Source, "backups/"+inst.Name+"/") {
					return "", errors.New("snapshot bundle retains instance disk; explicit disposition required")
				}
				if info, err := os.Lstat(filepath.Join(root, component.Path)); err != nil || !info.IsDir() {
					return "", errors.New("snapshot disk component missing or invalid")
				}
			}
			root = filepath.Join(root, "root")
			if info, err := os.Lstat(root); err != nil || !info.IsDir() {
				return "", errors.New("snapshot bundle root missing or invalid")
			}
		} else {
			return "", errors.New("legacy or unresolved snapshot archive; explicit disposition required")
		}
		for _, sub := range []string{"instances", "backups"} {
			if err := absentPath(filepath.Join(root, sub, inst.Name)); err != nil {
				return "", err
			}
		}
		copies, err := retainedTemplates(filepath.Join(root, "templates"), inst)
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
func (p *Provisioner) DeleteWithProof(ctx context.Context, inst model.Instance, requestID, actor, backendID string, closeGateways func(context.Context, model.Instance) error) (model.CleanupReceipt, error) {
	var r model.CleanupReceipt
	if closeGateways == nil {
		return r, errors.New("gateway quiescence proof unavailable")
	}
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
		ColdDisk(context.Context, vmrunner.ColdRequest) (vmrunner.ColdStatus, error)
	})
	if !ok {
		return r, errors.New("runtime proof helper unavailable")
	}
	backups, err := p.recoveryCopiesAbsent(ctx, inst)
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
	// Service-owned listeners must drain after admission, never during preflight.
	// A timeout leaves the durable cleanup fence in place, without a receipt.
	if err := closeGateways(ctx, inst); err != nil {
		return r, err
	}
	if _, err := p.recoveryCopiesAbsent(ctx, inst); err != nil {
		return r, err
	}
	if err := p.stopFirecracker(ctx, inst); err != nil {
		return r, err
	}
	// A momentary absence proof is insufficient: delayed offloads must remain
	// fenced after the runner releases its lock and we remove the local disk.
	cold, err := runtimeProof.ColdDisk(ctx, vmrunner.ColdRequest{Name: inst.Name, InstanceID: inst.ID, Action: "fence-delete"})
	if err != nil {
		return r, err
	}
	if cold.Phase != "hot" || cold.Remote || cold.Error != "" {
		return r, errors.New("cold storage deletion fence was not established")
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
	if _, err := p.recoveryCopiesAbsent(ctx, inst); err != nil {
		return r, err
	}
	if err := p.removeInstanceDir(inst.Name); err != nil {
		return r, err
	}
	// Publish no successful cleanup receipt ahead of durable directory removal.
	if err := syncTemplatePath(p.cfg.InstancesDir()); err != nil {
		return r, err
	}
	dir := filepath.Join(p.cfg.InstancesDir(), inst.Name)
	for _, path := range []string{dir, inst.RootFSPath} {
		if err := absentPath(path); err != nil {
			return r, err
		}
	}
	r = model.CleanupReceipt{Version: 3, BackendID: backendID, Kind: inst.KindName(), Image: inst.ImageName(), RetainedTemplates: templates, RequestID: requestID, Actor: actor, InstanceID: inst.ID, Name: inst.Name, CreatedAt: inst.CreatedAt, CreatedByUser: inst.CreatedByUser, CreatedByNode: inst.CreatedByNode, AuthKeyDisposition: "never-minted", IngressClosed: true, GatewaysAbsent: true, RuntimeAbsent: true, JailerAbsent: true, CgroupAbsent: true, NetworkAbsent: true, TapDevice: inst.TapDevice, NetworkCIDR: inst.NetworkCIDR, InstanceDir: dir, RootFSPath: inst.RootFSPath, BackupsDir: backups, StorageAbsent: true, BackupsAbsent: true, SnapshotsAbsent: true, DeletedAt: time.Now().UTC()}
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
			if (m.Version != 1 && m.Version != 2) || m.SourceID == "" || m.ID != entry.Name() || m.Owner == "" || m.Image == "" {
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
