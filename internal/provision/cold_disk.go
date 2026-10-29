package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"srv/internal/model"
	"srv/internal/storage"
	"srv/internal/vmrunner"
)

// The public marker is only a fence; the runner owns the authoritative journal.
type coldDiskMarker struct {
	InstanceID string `json:"instance_id"`
	Phase      string `json:"phase"`
	Remote     bool   `json:"remote"`
}

func (p *Provisioner) coldMarker(inst model.Instance) (*coldDiskMarker, error) {
	dir, err := p.instanceDir(inst.Name)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "cold-disk.json")
	if err := storage.ValidatePath(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("cold disk marker is not a regular file")
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var marker coldDiskMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return nil, fmt.Errorf("invalid cold disk marker: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if string(fields["remote"]) != "true" && string(fields["remote"]) != "false" {
		return nil, errors.New("invalid cold disk marker remote flag")
	}
	if marker.InstanceID == "" || marker.InstanceID != inst.ID || marker.Phase == "" {
		return nil, errors.New("invalid cold disk marker identity or phase")
	}
	return &marker, nil
}

func (p *Provisioner) coldDiskFence(ctx context.Context, inst model.Instance, anyMarker bool) error {
	m, err := p.coldMarker(inst)
	if err != nil {
		return err
	}
	if m != nil && (anyMarker || m.Phase != "hot") {
		return fmt.Errorf("instance %q has cold disk recovery/remote state (%s); run disk hydrate %s (then disk purge %s to remove retained remote state)", inst.Name, m.Phase, inst.Name, inst.Name)
	}
	status, err := p.runColdDisk(ctx, inst, "status")
	if err != nil {
		return err
	}
	if status.Phase != "hot" || (anyMarker && (status.Remote || status.JournalPresent)) {
		return fmt.Errorf("instance %q has cold disk recovery/remote state (%s); run disk hydrate %s (then disk purge %s to remove retained remote state)", inst.Name, status.Phase, inst.Name, inst.Name)
	}
	return nil
}

func (p *Provisioner) runColdDisk(ctx context.Context, inst model.Instance, action string) (vmrunner.ColdStatus, error) {
	runner, ok := p.vmRunner.(interface {
		ColdDisk(context.Context, vmrunner.ColdRequest) (vmrunner.ColdStatus, error)
	})
	if !ok {
		// Compatibility for in-process runners/mocks without cold support.
		// This exception never applies to the production Client: an unavailable
		// endpoint, an old runner's 404, or any other RPC error is propagated.
		marker, err := p.coldMarker(inst)
		if err != nil {
			return vmrunner.ColdStatus{}, err
		}
		if action != "offload" && marker == nil && p.cfg.ColdStorageHost == "" && coldDiskRootFSPresent(inst) == nil {
			return vmrunner.ColdStatus{Phase: "hot"}, nil
		}
		return vmrunner.ColdStatus{}, errors.New("cold disk runner unavailable")
	}
	q := vmrunner.ColdRequest{Name: inst.Name, InstanceID: inst.ID, Action: action}
	if action == "offload" {
		// A missing (or stale hot) public marker must not hide a generation
		// whose journal was committed before marker publication.
		status, err := p.runColdDisk(ctx, inst, "status")
		if err != nil {
			return vmrunner.ColdStatus{}, err
		}
		if status.Phase == "hot" || status.Phase == "purged" {
			q.ExpectedDisk, err = storage.ValidateDiskSubvolume(ctx, filepath.Join(p.cfg.InstancesDir(), inst.Name, "disk"))
			if err != nil {
				return vmrunner.ColdStatus{}, err
			}
		}
	}
	status, err := runner.ColdDisk(ctx, q)
	if err == nil && status.Error != "" {
		err = errors.New(status.Error)
	}
	if err == nil {
		switch status.Phase {
		case "hot", "offloading", "evicting", "cold", "restoring", "purged":
		default:
			err = fmt.Errorf("unknown cold disk runner phase %q", status.Phase)
		}
	}
	return status, err
}

// Runner status reports journal state, not the existence of a usable rootfs.
// In particular, no journal is reported as hot even after local disk loss.
func coldDiskRootFSPresent(inst model.Instance) error {
	if err := storage.ValidatePath(inst.RootFSPath); err != nil {
		return err
	}
	info, err := os.Lstat(inst.RootFSPath)
	if err != nil {
		return fmt.Errorf("authoritative local rootfs unavailable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("authoritative local rootfs is not a regular file")
	}
	return nil
}

func (p *Provisioner) hydrateColdDisk(ctx context.Context, inst model.Instance) error {
	m, err := p.coldMarker(inst)
	if err != nil {
		return err
	}
	status, err := p.runColdDisk(ctx, inst, "status")
	if err != nil {
		return fmt.Errorf("inspect cold disk: %w", err)
	}
	// Remote retention does not require rehydrating an authoritative hot disk.
	if status.Phase == "hot" && (m == nil || m.Phase == "hot") {
		return coldDiskRootFSPresent(inst)
	}
	status, err = p.runColdDisk(ctx, inst, "hydrate")
	if err != nil {
		return fmt.Errorf("hydrate cold disk: %w", err)
	}
	if status.Phase != "hot" {
		return errors.New("cold disk hydration did not produce a hot disk")
	}
	if err := coldDiskRootFSPresent(inst); err != nil {
		return err
	}
	return p.coldDiskFence(ctx, inst, false)
}

// cleanupColdForRemoval requires the caller's lifecycle lock and a confirmed
// stopped runtime. Call before removing an old incarnation's directory, even
// without a public marker or configured cold host. The runner owns journal
// cleanup and the durable tombstone against delayed operations. No local
// rootfs is required: a cold or partially-created instance may have none.
func (p *Provisioner) cleanupColdForRemoval(ctx context.Context, inst model.Instance) error {
	if _, err := p.coldMarker(inst); err != nil {
		return err
	}
	status, err := p.runColdDisk(ctx, inst, "delete")
	if err != nil {
		return fmt.Errorf("remove cold disk state: %w", err)
	}
	if status.Phase != "hot" || status.Remote || status.JournalPresent {
		return errors.New("cold disk deletion did not clear retained state")
	}
	marker, err := p.coldMarker(inst)
	if err != nil {
		return err
	}
	if marker != nil {
		return errors.New("cold disk marker remains after deletion")
	}
	return nil
}

// ColdDisk requires the caller's instance lifecycle lock.
func (p *Provisioner) ColdDisk(ctx context.Context, name, action string) (vmrunner.ColdStatus, error) {
	inst, err := p.store.GetInstance(ctx, name)
	if err != nil {
		return vmrunner.ColdStatus{}, err
	}
	if _, err := p.coldMarker(inst); err != nil {
		return vmrunner.ColdStatus{}, err
	}
	if action == "status" {
		status, err := p.runColdDisk(ctx, inst, action)
		if err == nil && status.Phase == "hot" {
			err = coldDiskRootFSPresent(inst)
		}
		return status, err
	}
	if action != "offload" && action != "hydrate" && action != "purge" {
		return vmrunner.ColdStatus{}, errors.New("invalid cold disk action")
	}
	if err := p.migrationFence(ctx, name); err != nil {
		return vmrunner.ColdStatus{}, err
	}
	if err := p.resolveRuntime(ctx, &inst); err != nil {
		return vmrunner.ColdStatus{}, err
	}
	if inst.State != model.StateStopped || inst.FirecrackerPID != 0 {
		return vmrunner.ColdStatus{}, errors.New("cold disk operation requires a stopped instance")
	}
	if action == "offload" {
		if p.cfg.ColdStorageHost == "" {
			return vmrunner.ColdStatus{}, errors.New("cold storage is disabled")
		}
		status, err := p.runColdDisk(ctx, inst, "status")
		if err != nil {
			return vmrunner.ColdStatus{}, err
		}
		if status.Phase == "hot" || status.Phase == "purged" {
			p.admissionMu.Lock()
			err = p.ensureDiskLayout(ctx, &inst)
			p.admissionMu.Unlock()
			if err != nil {
				return vmrunner.ColdStatus{}, err
			}
		}
	}
	status, err := p.runColdDisk(ctx, inst, action)
	if err == nil && status.Phase == "hot" {
		err = coldDiskRootFSPresent(inst)
	}
	return status, err
}
