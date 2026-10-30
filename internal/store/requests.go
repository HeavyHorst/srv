package store

import (
	"context"
	"fmt"
	"time"
)

// LifecycleRequest is an ingress receipt, not proof that a runtime is absent.
// IDs are retained indefinitely, including requests cancelled before execution.
type LifecycleRequest struct {
	ID        string `json:"id"`
	Actor     string `json:"actor"`
	Command   string `json:"command"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Phase     string `json:"phase"`
	ExitCode  int    `json:"exit_code"`
}

func (s *Store) GetLifecycleRequest(ctx context.Context, id, actor string) (LifecycleRequest, error) {
	var r LifecycleRequest
	err := s.db.QueryRowContext(ctx, `SELECT id, actor, command, name, arguments, phase, exit_code FROM lifecycle_requests WHERE id=? AND actor=?`, id, actor).Scan(&r.ID, &r.Actor, &r.Command, &r.Name, &r.Arguments, &r.Phase, &r.ExitCode)
	return r, err
}

// AdmitLifecycleRequest never executes a duplicate, even if its first execution
// failed. Existing request IDs cannot be rebound to another operation.
func (s *Store) AdmitLifecycleRequest(ctx context.Context, r LifecycleRequest) (bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO lifecycle_requests(id,actor,command,name,arguments,phase,created_at) VALUES(?,?,?,?,?,'admitted',?) ON CONFLICT(id) DO NOTHING`, r.ID, r.Actor, r.Command, r.Name, r.Arguments, timeString(time.Now().UTC()))
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		return true, nil
	}
	old, err := s.GetLifecycleRequest(ctx, r.ID, r.Actor)
	if err != nil {
		return false, err
	}
	if old.Command != r.Command || old.Name != r.Name || old.Arguments != r.Arguments {
		return false, fmt.Errorf("request ID already bound to another operation")
	}
	return false, nil
}

func (s *Store) FinishLifecycleRequest(ctx context.Context, id, actor string, exitCode int) (bool, error) {
	phase := "done"
	if exitCode != 0 {
		phase = "failed"
	}
	// An admitted request may fail validation without ever reaching execution.
	// Success, however, requires execution; terminal or interrupted rows stay fenced.
	result, err := s.db.ExecContext(ctx, `UPDATE lifecycle_requests SET phase=?, exit_code=? WHERE id=? AND actor=? AND (phase='executing' OR (phase='admitted' AND ? <> 0))`, phase, exitCode, id, actor, exitCode)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// RejectLifecycleStart records a proven pre-launch rejection, not a generic
// failure. Its tombstone still prevents the same request from executing again.
func (s *Store) RejectLifecycleStart(ctx context.Context, id, actor string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE lifecycle_requests SET phase='rejected',exit_code=1 WHERE id=? AND actor=? AND command='start' AND phase='executing'`, id, actor)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("start request no longer executing: %v", err)
	}
	return nil
}

// Called once by the exclusive service owner before accepting commands, not by
// Store.Open (which is also used by offline tools). Interrupted is uncertain.
func (s *Store) InterruptLifecycleRequests(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE lifecycle_requests SET phase=CASE WHEN phase='admitted' THEN 'cancelled' ELSE 'interrupted' END WHERE phase IN ('admitted','executing')`)
	return err
}

func (s *Store) BeginLifecycleRequest(ctx context.Context, id, actor string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE lifecycle_requests SET phase='executing' WHERE id=? AND actor=? AND phase='admitted'`, id, actor)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("request not admitted: %v", err)
	}
	return nil
}

func (s *Store) CancelLifecycleRequest(ctx context.Context, r LifecycleRequest) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO lifecycle_requests(id,actor,command,name,arguments,phase,created_at) VALUES(?,?,?,?,?,'cancelled',?) ON CONFLICT(id) DO NOTHING`, r.ID, r.Actor, r.Command, r.Name, r.Arguments, timeString(time.Now().UTC()))
	if err != nil {
		return err
	}
	old, err := s.GetLifecycleRequest(ctx, r.ID, r.Actor)
	if err != nil {
		return err
	}
	if old.Command != r.Command || old.Name != r.Name || old.Arguments != r.Arguments {
		return fmt.Errorf("request binding differs")
	}
	return nil
}

func (s *Store) BootstrapGeneration(ctx context.Context, instanceID string, created time.Time) (int64, error) {
	var generation int64
	err := s.db.QueryRowContext(ctx, `SELECT generation FROM orb_bootstrap WHERE instance_id=? AND created_at=?`, instanceID, timeString(created)).Scan(&generation)
	return generation, err
}

// Admission and the high-water mark commit together. No default for a missing
// row: old installations or lost state require reconciliation, not a reset.
func (s *Store) AdmitBootstrap(ctx context.Context, r LifecycleRequest, instanceID string, created time.Time, generation int64) error {
	if generation <= 0 {
		return fmt.Errorf("positive bootstrap generation required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE orb_bootstrap SET generation=? WHERE instance_id=? AND created_at=? AND generation<?`, generation, instanceID, timeString(created), generation)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("missing or non-increasing bootstrap generation")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO lifecycle_requests(id,actor,command,name,arguments,phase,created_at) VALUES(?,?,?,?,?,'executing',?)`, r.ID, r.Actor, r.Command, r.Name, r.Arguments, timeString(time.Now().UTC())); err != nil {
		return err
	}
	return tx.Commit()
}
