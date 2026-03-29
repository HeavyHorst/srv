package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"srv/internal/model"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) CreateInstance(ctx context.Context, inst model.Instance) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO instances (
			id, name, state, created_at, updated_at, created_by_user, created_by_node,
			rootfs_path, kernel_path, initrd_path, socket_path, log_path, serial_log_path,
			tap_device, guest_mac, network_cidr, host_addr, guest_addr, gateway_addr,
			firecracker_pid, tailscale_name, tailscale_ip, last_error, deleted_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		inst.ID,
		inst.Name,
		inst.State,
		timeString(inst.CreatedAt),
		timeString(inst.UpdatedAt),
		inst.CreatedByUser,
		inst.CreatedByNode,
		inst.RootFSPath,
		inst.KernelPath,
		inst.InitrdPath,
		inst.SocketPath,
		inst.LogPath,
		inst.SerialLogPath,
		inst.TapDevice,
		inst.GuestMAC,
		inst.NetworkCIDR,
		inst.HostAddr,
		inst.GuestAddr,
		inst.GatewayAddr,
		inst.FirecrackerPID,
		inst.TailscaleName,
		inst.TailscaleIP,
		inst.LastError,
		nullableTime(inst.DeletedAt),
	)
	if err != nil {
		return fmt.Errorf("insert instance: %w", err)
	}
	return nil
}

func (s *Store) UpdateInstance(ctx context.Context, inst model.Instance) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE instances
		SET state = ?, updated_at = ?, rootfs_path = ?, kernel_path = ?, initrd_path = ?, socket_path = ?,
			log_path = ?, serial_log_path = ?, tap_device = ?, guest_mac = ?, network_cidr = ?,
			host_addr = ?, guest_addr = ?, gateway_addr = ?, firecracker_pid = ?, tailscale_name = ?,
			tailscale_ip = ?, last_error = ?, deleted_at = ?
		WHERE name = ?
	`,
		inst.State,
		timeString(inst.UpdatedAt),
		inst.RootFSPath,
		inst.KernelPath,
		inst.InitrdPath,
		inst.SocketPath,
		inst.LogPath,
		inst.SerialLogPath,
		inst.TapDevice,
		inst.GuestMAC,
		inst.NetworkCIDR,
		inst.HostAddr,
		inst.GuestAddr,
		inst.GatewayAddr,
		inst.FirecrackerPID,
		inst.TailscaleName,
		inst.TailscaleIP,
		inst.LastError,
		nullableTime(inst.DeletedAt),
		inst.Name,
	)
	if err != nil {
		return fmt.Errorf("update instance %s: %w", inst.Name, err)
	}
	return nil
}

func (s *Store) GetInstance(ctx context.Context, name string) (model.Instance, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, state, created_at, updated_at, created_by_user, created_by_node,
			rootfs_path, kernel_path, initrd_path, socket_path, log_path, serial_log_path,
			tap_device, guest_mac, network_cidr, host_addr, guest_addr, gateway_addr,
			firecracker_pid, tailscale_name, tailscale_ip, last_error, deleted_at
		FROM instances
		WHERE name = ?
	`, name)
	inst, err := scanInstance(row.Scan)
	if err != nil {
		return model.Instance{}, err
	}
	return inst, nil
}

func (s *Store) FindInstance(ctx context.Context, name string) (model.Instance, bool, error) {
	inst, err := s.GetInstance(ctx, name)
	if err == nil {
		return inst, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return model.Instance{}, false, nil
	}
	return model.Instance{}, false, err
}

func (s *Store) DeleteInstance(ctx context.Context, name string) error {
	inst, found, err := s.FindInstance(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM instance_events WHERE instance_id = ?`, inst.ID); err != nil {
		return fmt.Errorf("delete instance events for %s: %w", name, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM instances WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete instance %s: %w", name, err)
	}
	return nil
}

func (s *Store) ListInstances(ctx context.Context, includeDeleted bool) ([]model.Instance, error) {
	query := `
		SELECT id, name, state, created_at, updated_at, created_by_user, created_by_node,
			rootfs_path, kernel_path, initrd_path, socket_path, log_path, serial_log_path,
			tap_device, guest_mac, network_cidr, host_addr, guest_addr, gateway_addr,
			firecracker_pid, tailscale_name, tailscale_ip, last_error, deleted_at
		FROM instances
	`
	if !includeDeleted {
		query += ` WHERE state <> 'deleted'`
	}
	query += ` ORDER BY created_at ASC`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	defer rows.Close()

	var out []model.Instance
	for rows.Next() {
		inst, err := scanInstance(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

func (s *Store) RecordEvent(ctx context.Context, event model.InstanceEvent) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO instance_events (instance_id, created_at, type, message, payload)
		VALUES (?, ?, ?, ?, ?)
	`, event.InstanceID, timeString(event.CreatedAt), event.Type, event.Message, event.Payload)
	if err != nil {
		return fmt.Errorf("insert instance event: %w", err)
	}
	return nil
}

func (s *Store) ListEvents(ctx context.Context, instanceID string, limit int) ([]model.InstanceEvent, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, instance_id, created_at, type, message, payload
		FROM instance_events
		WHERE instance_id = ?
		ORDER BY created_at DESC
		LIMIT ?
	`, instanceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var out []model.InstanceEvent
	for rows.Next() {
		var evt model.InstanceEvent
		var createdAt string
		if err := rows.Scan(&evt.ID, &evt.InstanceID, &createdAt, &evt.Type, &evt.Message, &evt.Payload); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		evt.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		out = append(out, evt)
	}
	return out, rows.Err()
}

func (s *Store) RecordCommand(ctx context.Context, audit model.CommandAudit) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO commands (
			created_at, actor_user, actor_display_name, actor_node, remote_addr, ssh_user,
			command, args_json, allowed, reason, duration_ms, error_text
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		timeString(audit.CreatedAt),
		audit.ActorUser,
		audit.ActorDisplayName,
		audit.ActorNode,
		audit.RemoteAddr,
		audit.SSHUser,
		audit.Command,
		audit.ArgsJSON,
		boolToInt(audit.Allowed),
		audit.Reason,
		audit.DurationMS,
		audit.ErrorText,
	)
	if err != nil {
		return fmt.Errorf("insert command audit: %w", err)
	}
	return nil
}

func (s *Store) RecordAuthz(ctx context.Context, decision model.AuthzDecision) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO authz_decisions (created_at, actor_user, actor_node, remote_addr, action, allowed, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`,
		timeString(decision.CreatedAt),
		decision.ActorUser,
		decision.ActorNode,
		decision.RemoteAddr,
		decision.Action,
		boolToInt(decision.Allowed),
		decision.Reason,
	)
	if err != nil {
		return fmt.Errorf("insert authz audit: %w", err)
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`PRAGMA journal_mode = WAL;`,
		`CREATE TABLE IF NOT EXISTS instances (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			state TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			created_by_user TEXT NOT NULL,
			created_by_node TEXT NOT NULL,
			rootfs_path TEXT NOT NULL,
			kernel_path TEXT NOT NULL,
			initrd_path TEXT NOT NULL,
			socket_path TEXT NOT NULL,
			log_path TEXT NOT NULL,
			serial_log_path TEXT NOT NULL,
			tap_device TEXT NOT NULL,
			guest_mac TEXT NOT NULL,
			network_cidr TEXT NOT NULL,
			host_addr TEXT NOT NULL,
			guest_addr TEXT NOT NULL,
			gateway_addr TEXT NOT NULL,
			firecracker_pid INTEGER NOT NULL DEFAULT 0,
			tailscale_name TEXT NOT NULL DEFAULT '',
			tailscale_ip TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '',
			deleted_at TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS instance_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			instance_id TEXT NOT NULL,
			created_at TEXT NOT NULL,
			type TEXT NOT NULL,
			message TEXT NOT NULL,
			payload TEXT NOT NULL DEFAULT '',
			FOREIGN KEY(instance_id) REFERENCES instances(id)
		);`,
		`CREATE TABLE IF NOT EXISTS commands (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT NOT NULL,
			actor_user TEXT NOT NULL,
			actor_display_name TEXT NOT NULL,
			actor_node TEXT NOT NULL,
			remote_addr TEXT NOT NULL,
			ssh_user TEXT NOT NULL,
			command TEXT NOT NULL,
			args_json TEXT NOT NULL,
			allowed INTEGER NOT NULL,
			reason TEXT NOT NULL,
			duration_ms INTEGER NOT NULL,
			error_text TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE TABLE IF NOT EXISTS authz_decisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT NOT NULL,
			actor_user TEXT NOT NULL,
			actor_node TEXT NOT NULL,
			remote_addr TEXT NOT NULL,
			action TEXT NOT NULL,
			allowed INTEGER NOT NULL,
			reason TEXT NOT NULL
		);`,
	}

	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("run migration %q: %w", stmt, err)
		}
	}
	return nil
}

func scanInstance(scan func(dest ...any) error) (model.Instance, error) {
	var inst model.Instance
	var createdAt string
	var updatedAt string
	var deletedAt sql.NullString
	if err := scan(
		&inst.ID,
		&inst.Name,
		&inst.State,
		&createdAt,
		&updatedAt,
		&inst.CreatedByUser,
		&inst.CreatedByNode,
		&inst.RootFSPath,
		&inst.KernelPath,
		&inst.InitrdPath,
		&inst.SocketPath,
		&inst.LogPath,
		&inst.SerialLogPath,
		&inst.TapDevice,
		&inst.GuestMAC,
		&inst.NetworkCIDR,
		&inst.HostAddr,
		&inst.GuestAddr,
		&inst.GatewayAddr,
		&inst.FirecrackerPID,
		&inst.TailscaleName,
		&inst.TailscaleIP,
		&inst.LastError,
		&deletedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Instance{}, err
		}
		return model.Instance{}, fmt.Errorf("scan instance: %w", err)
	}
	var err error
	inst.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return model.Instance{}, err
	}
	inst.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return model.Instance{}, err
	}
	if deletedAt.Valid {
		t, err := parseTime(deletedAt.String)
		if err != nil {
			return model.Instance{}, err
		}
		inst.DeletedAt = &t
	}
	return inst, nil
}

func parseTime(v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", v, err)
	}
	return t, nil
}

func timeString(t time.Time) string {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return timeString(*t)
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
