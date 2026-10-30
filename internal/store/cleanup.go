package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"srv/internal/model"
	"time"
)

// RecordEnrollmentKey runs before the one-use secret is handed to the runner.
func (s *Store) RecordEnrollmentKey(ctx context.Context, instanceID, keyID string) error {
	if instanceID == "" || keyID == "" {
		return fmt.Errorf("enrollment identity required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO enrollment_keys(instance_id,key_id) VALUES(?,?)`, instanceID, keyID)
	return err
}

func (s *Store) EnrollmentKey(ctx context.Context, instanceID string) (string, error) {
	var key string
	err := s.db.QueryRowContext(ctx, `SELECT key_id FROM enrollment_keys WHERE instance_id=?`, instanceID).Scan(&key)
	return key, err
}

// CheckCleanupFence protects proof-owned intent independently of request phase,
// including after startup changes executing requests to interrupted.
func (s *Store) CheckCleanupFence(ctx context.Context, instanceID string) error {
	var fenced bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM cleanup_fences WHERE instance_id=?)`, instanceID).Scan(&fenced); err != nil {
		return err
	}
	if fenced {
		return fmt.Errorf("instance has unresolved authoritative cleanup; operator reconciliation required")
	}
	return nil
}

// CleanupRequest returns the existing owner, including after service restart.
func (s *Store) CleanupRequest(ctx context.Context, instanceID string) (LifecycleRequest, error) {
	var id, actor string
	err := s.db.QueryRowContext(ctx, `SELECT r.id,r.actor FROM cleanup_fences f JOIN lifecycle_requests r ON r.id=f.request_id WHERE f.instance_id=?`, instanceID).Scan(&id, &actor)
	if err != nil {
		return LifecycleRequest{}, err
	}
	return s.GetLifecycleRequest(ctx, id, actor)
}

// BeginCleanup admits or resumes deletion of this exact incarnation. Only the
// owning request may resume a fence; non-delete lifecycle rules are unchanged.
// The caller holds the service lifecycle lock and snapshot command lease.
func (s *Store) BeginCleanup(ctx context.Context, requestID, actor string, inst model.Instance) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner string
	err = tx.QueryRowContext(ctx, `SELECT request_id FROM cleanup_fences WHERE instance_id=?`, inst.ID).Scan(&owner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	resume := err == nil
	if resume && owner != requestID {
		return fmt.Errorf("cleanup fence belongs to another request")
	}
	phases, states := "('admitted','failed','cancelled')", "('ready','stopped','failed','provisioning','awaiting-tailnet','awaiting-guest','stopping')"
	// Only a new ordinary-delete intent may adopt legacy deleting rows. A
	// bound interrupted request with a missing fence must still fail closed.
	var command string
	if err := tx.QueryRowContext(ctx, `SELECT command FROM lifecycle_requests WHERE id=? AND actor=?`, requestID, actor).Scan(&command); err != nil {
		return err
	}
	if command == "delete" {
		states = "('ready','stopped','failed','provisioning','awaiting-tailnet','awaiting-guest','stopping','deleting')"
		if !resume {
			var unresolved bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM lifecycle_requests WHERE name=? AND command IN ('delete','delete-proof') AND phase IN ('executing','interrupted'))`, inst.Name).Scan(&unresolved); err != nil {
				return err
			}
			if unresolved {
				return fmt.Errorf("cleanup owner fence is missing; operator reconciliation required")
			}
		}
	}
	if resume {
		phases, states = "('executing','interrupted')", "('deleting')"
	}
	result, err := tx.ExecContext(ctx, `UPDATE lifecycle_requests SET phase='executing',exit_code=0 WHERE id=? AND actor=? AND command IN ('delete','delete-proof') AND name=? AND phase IN `+phases, requestID, actor, inst.Name)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("cleanup request cannot begin or resume: %v", err)
	}
	result, err = tx.ExecContext(ctx, `UPDATE instances SET state='deleting',updated_at=? WHERE id=? AND name=? AND created_at=? AND created_by_user=? AND created_by_node=? AND tailnet_node_id=? AND state IN `+states, timeString(time.Now().UTC()), inst.ID, inst.Name, timeString(inst.CreatedAt), inst.CreatedByUser, inst.CreatedByNode, inst.TailnetNodeID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("cleanup incarnation changed: %v", err)
	}
	if !resume {
		if _, err := tx.ExecContext(ctx, `INSERT INTO cleanup_fences(instance_id,request_id) VALUES(?,?)`, inst.ID, requestID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CommitCleanup never writes a tombstone without the corresponding receipt.
// A failed or interrupted request cannot be completed by a late worker.
func (s *Store) CommitCleanup(ctx context.Context, r model.CleanupReceipt) error {
	if !r.ValidCopyScope() || !r.GatewaysAbsent || (r.Kind != model.KindOrb && r.Kind != model.KindVM) || r.BackendID == "" || r.Image == "" || r.RequestID == "" || r.Actor == "" || r.InstanceID == "" || r.Name == "" || r.CreatedAt.IsZero() || r.CreatedByUser == "" || r.CreatedByNode == "" || r.DeletedAt.IsZero() || !r.IngressClosed || !r.RuntimeAbsent || !r.JailerAbsent || !r.CgroupAbsent || !r.NetworkAbsent || !r.StorageAbsent || r.RootFSPath == "" || r.InstanceDir == "" || r.BackupsDir == "" || r.TapDevice == "" || r.NetworkCIDR == "" {
		return fmt.Errorf("incomplete cleanup proof")
	}
	if r.Kind == model.KindOrb && (r.AuthKeyDisposition != "never-minted" || r.TailnetNodeID != "") {
		return fmt.Errorf("auth key disposition not proven")
	}
	if r.Kind == model.KindVM && (r.Version != 5 || r.AuthKeyDisposition != "not-asserted") {
		return fmt.Errorf("VM cleanup requires its own identity disposition")
	}
	if r.Version == 5 && !((r.TailnetNodeID == "" && r.TailnetDisposition == "absent") || (r.TailnetNodeID != "" && (r.TailnetDisposition == "removed" || r.TailnetDisposition == "retained"))) {
		return fmt.Errorf("incomplete tailnet disposition")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE lifecycle_requests SET phase='done',exit_code=0 WHERE id=? AND actor=? AND command IN ('delete','delete-proof') AND name=? AND phase='executing'`, r.RequestID, r.Actor, r.Name)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("cleanup request no longer executing: %v", err)
	}
	result, err = tx.ExecContext(ctx, `UPDATE instances SET state='deleted',updated_at=?,deleted_at=?,firecracker_pid=0,last_error='',tailscale_name='',tailscale_ip='',tailnet_node_id='' WHERE id=? AND name=? AND created_at=? AND created_by_user=? AND created_by_node=? AND kind=? AND COALESCE(NULLIF(image,''),'arch')=? AND tailnet_node_id=? AND rootfs_path=? AND tap_device=? AND network_cidr=? AND state='deleting' AND (kind='vm' OR NOT EXISTS(SELECT 1 FROM enrollment_keys WHERE instance_id=?))`, timeString(r.DeletedAt), timeString(r.DeletedAt), r.InstanceID, r.Name, timeString(r.CreatedAt), r.CreatedByUser, r.CreatedByNode, r.Kind, r.Image, r.TailnetNodeID, r.RootFSPath, r.TapDevice, r.NetworkCIDR, r.InstanceID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("cleanup identity changed: %v", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO cleanup_receipts(request_id,actor,receipt) VALUES(?,?,?)`, r.RequestID, r.Actor, string(data)); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `DELETE FROM cleanup_fences WHERE instance_id=? AND request_id=?`, r.InstanceID, r.RequestID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("cleanup fence changed: %v", err)
	}
	return tx.Commit()
}

func (s *Store) CleanupReceipt(ctx context.Context, requestID, actor string) (model.CleanupReceipt, error) {
	var data string
	var r model.CleanupReceipt
	err := s.db.QueryRowContext(ctx, `SELECT receipt FROM cleanup_receipts WHERE request_id=? AND actor=?`, requestID, actor).Scan(&data)
	if err == nil {
		err = json.Unmarshal([]byte(data), &r)
	}
	return r, err
}

// CleanupReceiptForInstance survives name reuse and does not require the caller
// to know which request deleted the instance. The service checks receipt ownership.
func (s *Store) CleanupReceiptForInstance(ctx context.Context, instanceID string) (model.CleanupReceipt, error) {
	var data string
	var r model.CleanupReceipt
	err := s.db.QueryRowContext(ctx, `SELECT receipt FROM cleanup_receipts WHERE json_extract(receipt, '$.instance_id')=?`, instanceID).Scan(&data)
	if err == nil {
		err = json.Unmarshal([]byte(data), &r)
	}
	return r, err
}
