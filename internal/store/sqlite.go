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

func (s *Store) Checkpoint(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE);`); err != nil {
		return fmt.Errorf("checkpoint sqlite wal: %w", err)
	}
	return nil
}

func (s *Store) CreateInstance(ctx context.Context, inst model.Instance) error {
	normalizeInstanceMemory(&inst)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO instances (
			id, name, state, created_at, updated_at, created_by_user, created_by_node,
			vcpu_count, memory_mib, memory_mode, memory_pool_id, rootfs_size_bytes,
			rootfs_path, kernel_path, initrd_path, socket_path, log_path, serial_log_path,
			tap_device, guest_mac, network_cidr, host_addr, guest_addr, gateway_addr,
			firecracker_pid, tailscale_name, tailscale_ip, last_error, deleted_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		inst.ID,
		inst.Name,
		inst.State,
		timeString(inst.CreatedAt),
		timeString(inst.UpdatedAt),
		inst.CreatedByUser,
		inst.CreatedByNode,
		inst.VCPUCount,
		inst.MemoryMiB,
		inst.MemoryMode,
		inst.MemoryPoolID,
		inst.RootFSSizeBytes,
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
	normalizeInstanceMemory(&inst)
	_, err := s.db.ExecContext(ctx, `
		UPDATE instances
		SET state = ?, updated_at = ?, vcpu_count = ?, memory_mib = ?, memory_mode = ?, memory_pool_id = ?, rootfs_size_bytes = ?,
			rootfs_path = ?, kernel_path = ?, initrd_path = ?, socket_path = ?, log_path = ?,
			serial_log_path = ?, tap_device = ?, guest_mac = ?, network_cidr = ?, host_addr = ?,
			guest_addr = ?, gateway_addr = ?, firecracker_pid = ?, tailscale_name = ?,
			tailscale_ip = ?, last_error = ?, deleted_at = ?
		WHERE name = ?
	`,
		inst.State,
		timeString(inst.UpdatedAt),
		inst.VCPUCount,
		inst.MemoryMiB,
		inst.MemoryMode,
		inst.MemoryPoolID,
		inst.RootFSSizeBytes,
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
			vcpu_count, memory_mib, memory_mode, memory_pool_id, rootfs_size_bytes,
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
	if _, err := s.db.ExecContext(ctx, `DELETE FROM instance_integrations WHERE instance_id = ?`, inst.ID); err != nil {
		return fmt.Errorf("delete instance integrations for %s: %w", name, err)
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
			vcpu_count, memory_mib, memory_mode, memory_pool_id, rootfs_size_bytes,
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

func (s *Store) CreateMemoryPool(ctx context.Context, pool model.MemoryPool) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_pools (
			id, name, reserved_bytes, created_at, updated_at, created_by_user, created_by_node
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`,
		pool.ID,
		pool.Name,
		pool.ReservedBytes,
		timeString(pool.CreatedAt),
		timeString(pool.UpdatedAt),
		pool.CreatedByUser,
		pool.CreatedByNode,
	)
	if err != nil {
		return fmt.Errorf("insert memory pool: %w", err)
	}
	return nil
}

func (s *Store) GetMemoryPoolByName(ctx context.Context, name string) (model.MemoryPool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, reserved_bytes, created_at, updated_at, created_by_user, created_by_node
		FROM memory_pools
		WHERE name = ?
	`, name)
	pool, err := scanMemoryPool(row.Scan)
	if err != nil {
		return model.MemoryPool{}, err
	}
	return pool, nil
}

func (s *Store) FindMemoryPoolByName(ctx context.Context, name string) (model.MemoryPool, bool, error) {
	pool, err := s.GetMemoryPoolByName(ctx, name)
	if err == nil {
		return pool, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return model.MemoryPool{}, false, nil
	}
	return model.MemoryPool{}, false, err
}

func (s *Store) GetMemoryPoolByID(ctx context.Context, id string) (model.MemoryPool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, reserved_bytes, created_at, updated_at, created_by_user, created_by_node
		FROM memory_pools
		WHERE id = ?
	`, id)
	pool, err := scanMemoryPool(row.Scan)
	if err != nil {
		return model.MemoryPool{}, err
	}
	return pool, nil
}

func (s *Store) ListMemoryPools(ctx context.Context) ([]model.MemoryPool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, reserved_bytes, created_at, updated_at, created_by_user, created_by_node
		FROM memory_pools
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list memory pools: %w", err)
	}
	defer rows.Close()

	var out []model.MemoryPool
	for rows.Next() {
		pool, err := scanMemoryPool(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, pool)
	}
	return out, rows.Err()
}

func (s *Store) CountMemoryPoolMembers(ctx context.Context, poolID string) (int, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM instances
		WHERE memory_pool_id = ? AND state <> ?
	`, poolID, model.StateDeleted)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count memory pool members: %w", err)
	}
	return count, nil
}

func (s *Store) DeleteMemoryPool(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM memory_pools WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete memory pool %s: %w", id, err)
	}
	return nil
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

func (s *Store) CreateIntegration(ctx context.Context, integration model.Integration) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO integrations (
			id, name, kind, target_url, auth_mode, bearer_env, basic_user,
			basic_password_env, headers_json, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		integration.ID,
		integration.Name,
		integration.Kind,
		integration.TargetURL,
		integration.AuthMode,
		integration.BearerEnv,
		integration.BasicUser,
		integration.BasicPasswordEnv,
		integration.HeadersJSON,
		timeString(integration.CreatedAt),
		timeString(integration.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert integration %s: %w", integration.Name, err)
	}
	return nil
}

func (s *Store) GetIntegrationByName(ctx context.Context, name string) (model.Integration, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, kind, target_url, auth_mode, bearer_env, basic_user,
			basic_password_env, headers_json, created_at, updated_at
		FROM integrations
		WHERE name = ?
	`, name)
	return scanIntegration(row.Scan)
}

func (s *Store) ListIntegrations(ctx context.Context) ([]model.Integration, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, kind, target_url, auth_mode, bearer_env, basic_user,
			basic_password_env, headers_json, created_at, updated_at
		FROM integrations
		ORDER BY name ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list integrations: %w", err)
	}
	defer rows.Close()

	var out []model.Integration
	for rows.Next() {
		integration, err := scanIntegration(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, integration)
	}
	return out, rows.Err()
}

func (s *Store) DeleteIntegration(ctx context.Context, name string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM integrations WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete integration %s: %w", name, err)
	}
	return nil
}

func (s *Store) CountIntegrationBindings(ctx context.Context, integrationID string) (int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM instance_integrations WHERE integration_id = ?`, integrationID)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count integration bindings: %w", err)
	}
	return count, nil
}

func (s *Store) BindIntegrationToInstance(ctx context.Context, binding model.InstanceIntegrationBinding) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO instance_integrations (
			instance_id, integration_id, created_at, created_by_user, created_by_node
		) VALUES (?, ?, ?, ?, ?)
	`,
		binding.InstanceID,
		binding.IntegrationID,
		timeString(binding.CreatedAt),
		binding.CreatedByUser,
		binding.CreatedByNode,
	)
	if err != nil {
		return fmt.Errorf("bind integration to instance: %w", err)
	}
	return nil
}

func (s *Store) UnbindIntegrationFromInstance(ctx context.Context, instanceID, integrationID string) error {
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM instance_integrations
		WHERE instance_id = ? AND integration_id = ?
	`, instanceID, integrationID); err != nil {
		return fmt.Errorf("unbind integration from instance: %w", err)
	}
	return nil
}

func (s *Store) ListInstanceIntegrations(ctx context.Context, instanceID string) ([]model.Integration, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.name, i.kind, i.target_url, i.auth_mode, i.bearer_env, i.basic_user,
			i.basic_password_env, i.headers_json, i.created_at, i.updated_at
		FROM integrations i
		JOIN instance_integrations ii ON ii.integration_id = i.id
		WHERE ii.instance_id = ?
		ORDER BY i.name ASC
	`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("list instance integrations: %w", err)
	}
	defer rows.Close()

	var out []model.Integration
	for rows.Next() {
		integration, err := scanIntegration(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, integration)
	}
	return out, rows.Err()
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
			vcpu_count INTEGER NOT NULL DEFAULT 0,
			memory_mib INTEGER NOT NULL DEFAULT 0,
			memory_mode TEXT NOT NULL DEFAULT 'fixed',
			memory_pool_id TEXT NOT NULL DEFAULT '',
			rootfs_size_bytes INTEGER NOT NULL DEFAULT 0,
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
		`CREATE TABLE IF NOT EXISTS memory_pools (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			reserved_bytes INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			created_by_user TEXT NOT NULL,
			created_by_node TEXT NOT NULL
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
		`CREATE TABLE IF NOT EXISTS integrations (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			kind TEXT NOT NULL,
			target_url TEXT NOT NULL,
			auth_mode TEXT NOT NULL,
			bearer_env TEXT NOT NULL DEFAULT '',
			basic_user TEXT NOT NULL DEFAULT '',
			basic_password_env TEXT NOT NULL DEFAULT '',
			headers_json TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS instance_integrations (
			instance_id TEXT NOT NULL,
			integration_id TEXT NOT NULL,
			created_at TEXT NOT NULL,
			created_by_user TEXT NOT NULL,
			created_by_node TEXT NOT NULL,
			PRIMARY KEY (instance_id, integration_id),
			FOREIGN KEY(instance_id) REFERENCES instances(id),
			FOREIGN KEY(integration_id) REFERENCES integrations(id)
		);`,
	}

	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("run migration %q: %w", stmt, err)
		}
	}
	for _, column := range []struct {
		name string
		decl string
	}{
		{name: "vcpu_count", decl: "INTEGER NOT NULL DEFAULT 0"},
		{name: "memory_mib", decl: "INTEGER NOT NULL DEFAULT 0"},
		{name: "memory_mode", decl: "TEXT NOT NULL DEFAULT 'fixed'"},
		{name: "memory_pool_id", decl: "TEXT NOT NULL DEFAULT ''"},
		{name: "rootfs_size_bytes", decl: "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := s.ensureColumn(ctx, "instances", column.name, column.decl); err != nil {
			return err
		}
	}
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_instance_integrations_integration_id
			ON instance_integrations (integration_id)
		;`,
		`CREATE INDEX IF NOT EXISTS idx_instances_memory_pool_id
			ON instances (memory_pool_id)
		;`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("run migration %q: %w", stmt, err)
		}
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, table, column, decl string) error {
	exists, err := s.hasColumn(ctx, table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl)
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Store) hasColumn(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid      int
			name     string
			declType string
			notNull  int
			defaultV sql.NullString
			pk       int
		)
		if err := rows.Scan(&cid, &name, &declType, &notNull, &defaultV, &pk); err != nil {
			return false, fmt.Errorf("scan %s columns: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate %s columns: %w", table, err)
	}
	return false, nil
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
		&inst.VCPUCount,
		&inst.MemoryMiB,
		&inst.MemoryMode,
		&inst.MemoryPoolID,
		&inst.RootFSSizeBytes,
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
	normalizeInstanceMemory(&inst)
	return inst, nil
}

func scanMemoryPool(scan func(dest ...any) error) (model.MemoryPool, error) {
	var pool model.MemoryPool
	var createdAt string
	var updatedAt string
	if err := scan(
		&pool.ID,
		&pool.Name,
		&pool.ReservedBytes,
		&createdAt,
		&updatedAt,
		&pool.CreatedByUser,
		&pool.CreatedByNode,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.MemoryPool{}, err
		}
		return model.MemoryPool{}, fmt.Errorf("scan memory pool: %w", err)
	}
	var err error
	pool.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return model.MemoryPool{}, err
	}
	pool.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return model.MemoryPool{}, err
	}
	return pool, nil
}

func normalizeInstanceMemory(inst *model.Instance) {
	if inst == nil {
		return
	}
	inst.MemoryMode = model.NormalizeMemoryMode(inst.MemoryMode)
	if inst.MemoryMode != model.MemoryModePool {
		inst.MemoryPoolID = ""
	}
}

func scanIntegration(scan func(dest ...any) error) (model.Integration, error) {
	var integration model.Integration
	var createdAt string
	var updatedAt string
	if err := scan(
		&integration.ID,
		&integration.Name,
		&integration.Kind,
		&integration.TargetURL,
		&integration.AuthMode,
		&integration.BearerEnv,
		&integration.BasicUser,
		&integration.BasicPasswordEnv,
		&integration.HeadersJSON,
		&createdAt,
		&updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Integration{}, err
		}
		return model.Integration{}, fmt.Errorf("scan integration: %w", err)
	}
	var err error
	integration.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return model.Integration{}, err
	}
	integration.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return model.Integration{}, err
	}
	return integration, nil
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
