package store

import (
	"context"
	"fmt"
	"time"

	"srv/internal/model"
)

func (s *Store) GetMigration(ctx context.Context, instanceID string) (model.Migration, error) {
	var m model.Migration
	err := s.db.QueryRowContext(ctx, `SELECT instance_id, token, target, was_running, phase, memory_pool_name, fixed_memory FROM migrations WHERE instance_id = ?`, instanceID).Scan(&m.InstanceID, &m.Token, &m.Target, &m.WasRunning, &m.Phase, &m.MemoryPoolName, &m.FixedMemory)
	return m, err
}

func (s *Store) PutMigration(ctx context.Context, m model.Migration) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO migrations(instance_id, token, target, was_running, phase, memory_pool_name, fixed_memory) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(instance_id) DO UPDATE SET token=excluded.token, target=excluded.target, was_running=excluded.was_running, phase=excluded.phase, memory_pool_name=excluded.memory_pool_name, fixed_memory=excluded.fixed_memory`, m.InstanceID, m.Token, m.Target, m.WasRunning, m.Phase, m.MemoryPoolName, m.FixedMemory)
	return err
}

func (s *Store) GetImportReceipt(ctx context.Context, instanceID string) (model.ImportReceipt, error) {
	var r model.ImportReceipt
	err := s.db.QueryRowContext(ctx, `SELECT instance_id, token, source_instance_id, exported_at, phase FROM import_receipts WHERE instance_id = ?`, instanceID).Scan(&r.InstanceID, &r.Token, &r.SourceInstanceID, &r.ExportedAt, &r.Phase)
	return r, err
}

func (s *Store) PutImportReceipt(ctx context.Context, r model.ImportReceipt) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO import_receipts(instance_id, token, source_instance_id, exported_at, phase) VALUES (?, ?, ?, ?, ?) ON CONFLICT(instance_id) DO UPDATE SET token=excluded.token, source_instance_id=excluded.source_instance_id, exported_at=excluded.exported_at, phase=excluded.phase`, r.InstanceID, r.Token, r.SourceInstanceID, r.ExportedAt, r.Phase)
	return err
}

// ReserveImportReceipt cannot modify the journal of any visible incarnation,
// including a differently named instance carrying the artifact's ID. The
// absence check and receipt write are one SQLite statement; an orphan receipt
// left before reservation creation may safely be replaced on retry.
func (s *Store) ReserveImportReceipt(ctx context.Context, r model.ImportReceipt) error {
	result, err := s.db.ExecContext(ctx, `INSERT INTO import_receipts(instance_id, token, source_instance_id, exported_at, phase)
		SELECT ?, ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM instances WHERE id = ?)
		ON CONFLICT(instance_id) DO UPDATE SET token=excluded.token, source_instance_id=excluded.source_instance_id, exported_at=excluded.exported_at, phase=excluded.phase`, r.InstanceID, r.Token, r.SourceInstanceID, r.ExportedAt, r.Phase, r.InstanceID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("import instance ID %q already exists", r.InstanceID)
	}
	return nil
}

// FinalizeImport atomically releases the destination start fence and records
// the committed stopped instance after all artifact files are durable.
func (s *Store) FinalizeImport(ctx context.Context, instanceID string, updatedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE instances SET state = ?, firecracker_pid = 0, last_error = '', deleted_at = NULL, updated_at = ? WHERE id = ?`, model.StateStopped, timeString(updatedAt), instanceID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("import reservation missing: %v", err)
	}
	result, err = tx.ExecContext(ctx, `UPDATE import_receipts SET phase = 'imported' WHERE instance_id = ? AND phase = 'importing'`, instanceID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("import receipt missing: %v", err)
	}
	return tx.Commit()
}
