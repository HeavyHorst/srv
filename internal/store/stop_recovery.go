package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// StopRecovery is NEW operator authority for one exact attempt, not a claim
// that a legacy ingress receipt recorded that attempt. Its binding is immutable.
type StopRecovery struct {
	ID                  string `json:"id"`
	OriginalRequestID   string `json:"original_request_id"`
	Actor               string `json:"actor"`
	OriginalArguments   string `json:"original_arguments"`
	Name                string `json:"name"`
	InstanceID          string `json:"instance_id"`
	CreatedAt           string `json:"created_at"`
	BackendID           string `json:"backend_id"`
	Image               string `json:"image"`
	StartRequestID      string `json:"start_request_id"`
	HostBootID          string `json:"host_boot_id"`
	BootstrapGeneration int64  `json:"bootstrap_generation"`
	SaveState           bool   `json:"save_state"`
	Phase               string `json:"phase"`
}

func (r StopRecovery) MatchesRequest(old LifecycleRequest) bool {
	return r.ID != "" && r.ID != old.ID && r.OriginalRequestID == old.ID && r.Actor == old.Actor && old.Command == "stop" && r.Name == old.Name && r.OriginalArguments == old.Arguments && r.InstanceID != "" && r.CreatedAt != "" && r.BackendID != "" && r.Image != "" && r.StartRequestID != "" && r.HostBootID != "" && r.BootstrapGeneration >= 0 && r.SaveState
}

func (s *Store) CheckStopRecoveryFence(ctx context.Context, instanceID string) error {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM stop_recoveries WHERE instance_id=? AND phase='pending' LIMIT 1`, instanceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("instance has pending stop recovery %s", id)
}

func (s *Store) BeginStopRecovery(ctx context.Context, r StopRecovery) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old LifecycleRequest
	err = tx.QueryRowContext(ctx, `SELECT id,actor,command,name,arguments,phase FROM lifecycle_requests WHERE id=? AND actor=?`, r.OriginalRequestID, r.Actor).Scan(&old.ID, &old.Actor, &old.Command, &old.Name, &old.Arguments, &old.Phase)
	if err != nil {
		return err
	}
	if !r.MatchesRequest(old) || (old.Phase != "executing" && old.Phase != "interrupted" && old.Phase != "recovered") {
		return errors.New("stop request cannot be recovered")
	}
	r.Phase = ""
	binding, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var prior string
	err = tx.QueryRowContext(ctx, `SELECT binding FROM stop_recoveries WHERE id=? OR original_request_id=?`, r.ID, r.OriginalRequestID).Scan(&prior)
	if err == nil {
		if prior != string(binding) {
			return errors.New("stop recovery binding differs")
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if old.Phase == "recovered" {
		return errors.New("recovered stop has no matching proof")
	}
	var pending string
	err = tx.QueryRowContext(ctx, `SELECT id FROM stop_recoveries WHERE instance_id=? AND phase='pending' LIMIT 1`, r.InstanceID).Scan(&pending)
	if err == nil {
		return errors.New("another stop recovery owns instance")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO stop_recoveries(id,original_request_id,actor,instance_id,binding,phase) VALUES(?,?,?,?,?,'pending')`, r.ID, r.OriginalRequestID, r.Actor, r.InstanceID, string(binding))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CompleteStopRecovery(ctx context.Context, id, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var original, binding, phase string
	if err := tx.QueryRowContext(ctx, `SELECT original_request_id,binding,phase FROM stop_recoveries WHERE id=? AND actor=?`, id, actor).Scan(&original, &binding, &phase); err != nil {
		return err
	}
	var r StopRecovery
	if err := json.Unmarshal([]byte(binding), &r); err != nil {
		return err
	}
	var old LifecycleRequest
	if err := tx.QueryRowContext(ctx, `SELECT id,actor,command,name,arguments,phase FROM lifecycle_requests WHERE id=? AND actor=?`, original, actor).Scan(&old.ID, &old.Actor, &old.Command, &old.Name, &old.Arguments, &old.Phase); err != nil {
		return err
	}
	if !r.MatchesRequest(old) || r.ID != id || (phase != "pending" && phase != "done") {
		return errors.New("invalid recovery binding")
	}
	if phase == "done" && old.Phase == "recovered" {
		return tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE lifecycle_requests SET phase='recovered',exit_code=0 WHERE id=? AND actor=? AND phase IN ('executing','interrupted')`, original, actor)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return errors.New("stop request no longer recoverable")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE stop_recoveries SET phase='done' WHERE id=? AND actor=? AND phase='pending'`, id, actor); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) stopRecoveryForRequest(ctx context.Context, old LifecycleRequest) (*StopRecovery, error) {
	var binding, phase string
	if err := s.db.QueryRowContext(ctx, `SELECT binding,phase FROM stop_recoveries WHERE original_request_id=? AND actor=?`, old.ID, old.Actor).Scan(&binding, &phase); err != nil {
		return nil, err
	}
	var r StopRecovery
	if err := json.Unmarshal([]byte(binding), &r); err != nil {
		return nil, err
	}
	if phase != "done" || !r.MatchesRequest(old) {
		return nil, errors.New("invalid linked stop recovery proof")
	}
	r.Phase = phase
	return &r, nil
}
