package provision

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"srv/internal/model"
	"srv/internal/storage"
	"srv/internal/vmrunner"
)

func (p *Provisioner) migrationDiskRPC(ctx context.Context, q vmrunner.MigrationDiskRequest, in io.Reader, out io.Writer) (vmrunner.MigrationDiskResult, error) {
	runner, ok := p.vmRunner.(interface {
		MigrationDisk(context.Context, vmrunner.MigrationDiskRequest, io.Reader, io.Writer) (vmrunner.MigrationDiskResult, error)
	})
	if !ok {
		return vmrunner.MigrationDiskResult{}, errors.New("runner does not support native migration")
	}
	return runner.MigrationDisk(ctx, q, in, out)
}

// TransferMigrationDisk is called under the service's instance lock, only for
// native commands from an authenticated trusted frontend. Source operations
// additionally require the durable prepared migration fence and its token.
func (p *Provisioner) TransferMigrationDisk(ctx context.Context, q vmrunner.MigrationDiskRequest, in io.Reader, out io.Writer) (vmrunner.MigrationDiskResult, error) {
	switch q.Action {
	case "prepare", "send-base", "send-disk", "cleanup":
		inst, err := p.requireStoppedInstance(ctx, q.Name)
		if err != nil {
			return vmrunner.MigrationDiskResult{}, err
		}
		m, err := p.store.GetMigration(ctx, inst.ID)
		if err != nil {
			return vmrunner.MigrationDiskResult{}, err
		}
		if m.Token != q.Token || (m.Phase != "prepared" && !(q.Action == "cleanup" && m.Phase == "completed")) {
			return vmrunner.MigrationDiskResult{}, errors.New("native transfer requires the matching prepared migration")
		}
		q.InstanceID = inst.ID
		if q.Action == "prepare" {
			q.ExpectedDisk, err = storage.ValidateDiskSubvolume(ctx, filepath.Dir(inst.RootFSPath))
			if err != nil {
				return vmrunner.MigrationDiskResult{}, err
			}
		}
	case "check", "base-present", "receive-base", "receive-disk":
	default:
		return vmrunner.MigrationDiskResult{}, errors.New("unsupported native transfer action")
	}
	return p.migrationDiskRPC(ctx, q, in, out)
}

func (p *Provisioner) MigrationStatus(ctx context.Context, name string) (model.Migration, error) {
	inst, err := p.store.GetInstance(ctx, name)
	if err != nil {
		return model.Migration{}, err
	}
	return p.store.GetMigration(ctx, inst.ID)
}

func (p *Provisioner) migrationFence(ctx context.Context, name string) error {
	inst, err := p.store.GetInstance(ctx, name)
	if err != nil {
		return err
	}
	if err := p.store.CheckCleanupFence(ctx, inst.ID); err != nil {
		return err
	}
	r, err := p.store.GetImportReceipt(ctx, inst.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && r.Phase != "imported" {
		return fmt.Errorf("instance %q is fenced by incomplete import", name)
	}
	m, err := p.MigrationStatus(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if m.Phase != "aborted" {
		return fmt.Errorf("instance %q is fenced by migration (%s)", name, m.Phase)
	}
	return nil
}

// PrepareMigration requires the caller's instance lock. Admission serializes the
// durable fence with runtime admission; a failed/cancelled stop leaves it fenced.
func (p *Provisioner) PrepareMigration(ctx context.Context, name, target, token string, options ...ImportOptions) (model.Migration, error) {
	opts := ImportOptions{}
	if len(options) > 0 {
		opts = options[0]
	}
	p.admissionMu.Lock()
	defer p.admissionMu.Unlock()
	inst, err := p.store.GetInstance(ctx, name)
	if err != nil {
		return model.Migration{}, err
	}
	if err := p.coldDiskFence(ctx, inst, false); err != nil {
		return model.Migration{}, err
	}
	if target == "" || token == "" || inst.State == model.StateDeleted || inst.State == model.StateDeleting {
		return model.Migration{}, fmt.Errorf("invalid migration source, target or token")
	}
	if err := opts.Validate(inst.UsesMemoryPool()); err != nil {
		return model.Migration{}, err
	}
	m, err := p.store.GetMigration(ctx, inst.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return m, err
	}
	if err == nil && m.Phase == "aborted" {
		if m.Token == token {
			return m, fmt.Errorf("recovered migration requires a fresh token")
		}
		err = sql.ErrNoRows
	}
	if err == nil && (m.Token != token || m.Target != target || m.MemoryPoolName != opts.MemoryPoolName || m.FixedMemory != opts.FixedMemory) {
		return m, fmt.Errorf("migration already exists with different token, target or recovery outcome")
	}
	if err == nil && (m.Phase == "prepared" || m.Phase == "completed") {
		return m, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		if err := p.migrationFence(ctx, name); err != nil {
			return m, err
		}
		bindings, err := p.store.ListInstanceIntegrations(ctx, inst.ID)
		if err != nil {
			return m, err
		}
		if len(bindings) != 0 {
			return m, fmt.Errorf("migration of integrations is not supported")
		}
		m = model.Migration{InstanceID: inst.ID, Token: token, Target: target, WasRunning: inst.State == model.StateReady || inst.State == model.StateAwaitingTailnet || instanceHasActiveRuntime(inst), Phase: "preparing"}
		m.MemoryPoolName, m.FixedMemory = opts.MemoryPoolName, opts.FixedMemory
		if err := p.store.PutMigration(ctx, m); err != nil {
			return m, err
		}
	}
	if err := p.resolveRuntime(ctx, &inst); err != nil {
		return m, err
	}
	if inst.State != model.StateStopped || inst.FirecrackerPID != 0 {
		if _, err := p.Stop(ctx, name); err != nil {
			return m, err
		}
	}
	m.Phase = "prepared"
	return m, p.store.PutMigration(ctx, m)
}

// FinishMigration never starts a runtime. Recovery confirmation is checked by
// the service; callers must independently establish the destination is gone.
func (p *Provisioner) FinishMigration(ctx context.Context, name, token string, recoverSource bool) (model.Migration, error) {
	p.admissionMu.Lock()
	defer p.admissionMu.Unlock()
	m, err := p.MigrationStatus(ctx, name)
	if err != nil {
		return m, err
	}
	if m.Token != token {
		return m, fmt.Errorf("migration token mismatch")
	}
	if recoverSource {
		if m.Phase == "completed" {
			return m, fmt.Errorf("completed migration cannot be aborted; delete the source instead")
		}
		m.Phase = "aborted"
	} else {
		if m.Phase != "prepared" && m.Phase != "completed" {
			return m, fmt.Errorf("migration is not prepared")
		}
		m.Phase = "completed"
	}
	return m, p.store.PutMigration(ctx, m)
}
