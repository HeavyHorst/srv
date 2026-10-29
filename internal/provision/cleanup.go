package provision

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"srv/internal/model"
	"srv/internal/nethelper"
	"srv/internal/storage"
	"srv/internal/store"
	"srv/internal/vmrunner"
)

func absentPath(path string) error {
	_, err := os.Lstat(path)
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cleanup path present or unreadable %s: %v", path, err)
	}
	return nil
}

// SetCleanup configures service-owned gateway teardown before serving commands.
func (p *Provisioner) SetCleanup(backendID string, closeGateways func(context.Context, model.Instance) error) {
	p.backendID, p.closeGateways = backendID, closeGateways
}

// Delete selects the current instance once. All subsequent work, including
// background retries, uses the same durable request and incarnation binding.
func (p *Provisioner) Delete(ctx context.Context, name string) (model.Instance, error) {
	inst, err := p.store.GetInstance(ctx, name)
	if err != nil {
		return inst, err
	}
	if inst.State == model.StateDeleted {
		return inst, fmt.Errorf("instance %q is already deleted", name)
	}
	request, err := p.store.CleanupRequest(ctx, inst.ID)
	if errors.Is(err, sql.ErrNoRows) {
		// Stable across preflight failures, before a cleanup fence exists.
		id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("srv-delete:"+inst.ID)).String()
		args, _ := json.Marshal([]string{inst.ID, inst.Name, inst.CreatedAt.Format(time.RFC3339Nano), p.backendID, inst.ImageName()})
		request = store.LifecycleRequest{ID: id, Actor: inst.CreatedByUser, Command: "delete", Name: inst.Name, Arguments: string(args)}
		_, err = p.store.AdmitLifecycleRequest(ctx, request)
	}
	if err != nil {
		return inst, err
	}
	var identity []string
	if err := json.Unmarshal([]byte(request.Arguments), &identity); err != nil {
		return inst, err
	}
	if request.Command == "delete-proof" {
		if len(identity) != 7 || identity[0] != request.Command || identity[1] != request.ID || identity[2] != inst.Name {
			return inst, errors.New("cleanup request binding changed")
		}
		identity = []string{identity[3], identity[2], identity[4], identity[5], identity[6]}
	}
	if len(identity) != 5 || request.Name != inst.Name || identity[0] != inst.ID || identity[1] != inst.Name || identity[3] != p.backendID || identity[4] != inst.ImageName() {
		return inst, errors.New("cleanup incarnation or backend binding changed")
	}
	created, err := time.Parse(time.RFC3339Nano, identity[2])
	if err != nil || !created.Equal(inst.CreatedAt) {
		return inst, errors.New("cleanup creation binding changed")
	}
	_, err = p.DeleteWithProof(ctx, inst, request.ID, request.Actor, p.backendID, p.closeGateways)
	current, readErr := p.store.GetInstance(ctx, name)
	if readErr == nil {
		inst = current
		if err != nil && inst.State == model.StateDeleting {
			inst.LastError = "delete failed: " + err.Error()
			inst.UpdatedAt = time.Now().UTC()
			err = errors.Join(err, p.store.UpdateInstance(ctx, inst))
		}
	}
	return inst, errors.Join(err, readErr)
}

// DeleteWithProof is the single deletion engine, under the service instance lock.
// A retry retains the original request and incarnation fence, and repeats only
// idempotent cleanup.
// Shared snapshots and project templates are outside this deletion's scope.
func (p *Provisioner) DeleteWithProof(ctx context.Context, inst model.Instance, requestID, actor, backendID string, closeGateways func(context.Context, model.Instance) error) (model.CleanupReceipt, error) {
	var r model.CleanupReceipt
	if closeGateways == nil {
		return r, errors.New("gateway quiescence proof unavailable")
	}
	if (inst.KindName() != model.KindOrb && inst.KindName() != model.KindVM) || backendID == "" {
		return r, errors.New("cleanup requires a supported instance and backend identity")
	}
	migration, err := p.store.GetMigration(ctx, inst.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return r, err
	}
	moved := err == nil && migration.Phase == "completed"
	if err == nil && !moved && migration.Phase != "aborted" {
		return r, errors.New("active migration excludes deletion")
	}
	importReceipt, err := p.store.GetImportReceipt(ctx, inst.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return r, err
	}
	partialImport := err == nil && importReceipt.Phase == "importing"
	if inst.KindName() == model.KindOrb {
		if _, err := p.store.EnrollmentKey(ctx, inst.ID); !errors.Is(err, sql.ErrNoRows) || inst.TailnetNodeID != "" {
			return r, errors.New("native orb enrollment history is not empty")
		}
	}
	network, ok := p.networkHelper.(interface {
		FinalizeInstanceNetwork(context.Context, nethelper.FinalizeRequest) error
	})
	if !ok {
		return r, errors.New("network finalization helper unavailable")
	}
	runtimeProof, ok := p.vmRunner.(interface {
		VerifyInstanceAbsent(context.Context, vmrunner.StartResultRequest) error
		ColdDisk(context.Context, vmrunner.ColdRequest) (vmrunner.ColdStatus, error)
	})
	if !ok {
		return r, errors.New("runtime proof helper unavailable")
	}
	backups, err := p.instanceBackupsDir(inst.Name)
	if err != nil {
		return r, err
	}
	dir, err := p.instanceDir(inst.Name)
	if err != nil {
		return r, err
	}
	if err := p.store.BeginCleanup(ctx, requestID, actor, inst); err != nil {
		return r, err
	}
	if err := closeGateways(ctx, inst); err != nil {
		return r, err
	}
	if inst.StartRequestID != "" {
		// Resolve a persisted but never-submitted start into a closed attempt.
		// Unlike ordinary reconciliation, do not run unfenced network cleanup.
		result, err := p.startResult(ctx, inst)
		if err != nil {
			return r, err
		}
		if result.Outcome != vmrunner.StartAbsent && result.Outcome != vmrunner.StartRunning {
			return r, fmt.Errorf("runtime outcome remains uncertain: %s", result.Reason)
		}
	}
	if err := p.stopFirecracker(ctx, inst); err != nil {
		return r, err
	}
	// This removes remote generations without hydration, and fences delayed
	// starts/offloads even when there is no local rootfs or start receipt.
	if err := p.cleanupColdForRemoval(ctx, inst); err != nil {
		return r, err
	}
	if err := runtimeProof.VerifyInstanceAbsent(ctx, vmrunner.StartResultRequest{Name: inst.Name, InstanceID: inst.ID, RequestID: inst.StartRequestID, HostBootID: inst.StartBootID}); err != nil {
		return r, err
	}
	// No runner admission while holding payload: hydration/drain can otherwise
	// form a lock cycle. Match import's payload -> admission order. Surviving
	// Btrfs children retain it until their destructive work ends.
	ctx, release, err := storage.AcquirePayload(ctx, p.cfg.DataDirAbs())
	if err != nil {
		return r, err
	}
	defer release()
	p.admissionMu.Lock()
	defer p.admissionMu.Unlock()
	// Only an initial hint: retries use nethelper's durable original binding,
	// even when the default route has changed or can no longer be queried.
	outbound, _ := p.outboundInterface(ctx)
	if err := network.FinalizeInstanceNetwork(ctx, nethelper.FinalizeRequest{RequestID: requestID, CleanupRequest: nethelper.CleanupRequest{InstanceID: inst.ID, TapDevice: inst.TapDevice, NetworkCIDR: inst.NetworkCIDR, OutboundInterface: outbound}}); err != nil {
		return r, err
	}
	tailnetDisposition := "absent"
	if inst.TailnetNodeID != "" {
		tailnetDisposition = "retained"
		if !moved && !partialImport {
			if p.tsClient == nil {
				return r, errors.New("tailnet API required to delete this instance's identity")
			}
			devices, err := listTailnetDevices(ctx, p.tsClient)
			if err != nil {
				return r, err
			}
			for _, device := range devices {
				if device.NodeID == inst.TailnetNodeID {
					if err := p.tsClient.Devices().Delete(ctx, deviceIdentifier(device)); err != nil {
						return r, err
					}
				}
			}
			tailnetDisposition = "removed"
		}
	}
	if err := removeTree(ctx, dir); err != nil {
		return r, fmt.Errorf("remove instance directory for %q (%s): %w", inst.Name, dir, err)
	}
	parent := filepath.Dir(dir)
	if _, err := os.Lstat(parent); errors.Is(err, os.ErrNotExist) {
		parent = filepath.Dir(parent)
	}
	// Also sync on retry after a crash between unlink and directory fsync.
	if err := syncTemplatePath(parent); err != nil {
		return r, err
	}
	if err := absentPath(dir); err != nil {
		return r, err
	}
	if err := absentPath(inst.RootFSPath); err != nil {
		return r, err
	}
	authDisposition := "not-asserted"
	if inst.KindName() == model.KindOrb {
		authDisposition = "never-minted"
	}
	r = model.CleanupReceipt{Version: 5, BackendID: backendID, Kind: inst.KindName(), Image: inst.ImageName(), RequestID: requestID, Actor: actor, InstanceID: inst.ID, Name: inst.Name, CreatedAt: inst.CreatedAt, CreatedByUser: inst.CreatedByUser, CreatedByNode: inst.CreatedByNode, AuthKeyDisposition: authDisposition, TailnetNodeID: inst.TailnetNodeID, TailnetDisposition: tailnetDisposition, IngressClosed: true, GatewaysAbsent: true, RuntimeAbsent: true, JailerAbsent: true, CgroupAbsent: true, NetworkAbsent: true, TapDevice: inst.TapDevice, NetworkCIDR: inst.NetworkCIDR, InstanceDir: dir, RootFSPath: inst.RootFSPath, BackupsDir: backups, StorageAbsent: true, DeletedAt: time.Now().UTC()}
	if err := p.store.CommitCleanup(ctx, r); err != nil {
		return model.CleanupReceipt{}, err
	}
	return r, nil
}
