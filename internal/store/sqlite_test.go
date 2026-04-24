package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"srv/internal/model"
)

func TestStoreInstanceRoundTripAndListFiltering(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	createdAt := time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(2 * time.Minute)
	deletedAt := createdAt.Add(10 * time.Minute)

	ready := testInstance("alpha", model.StateReady, createdAt)
	deleted := testInstance("beta", model.StateDeleted, createdAt.Add(time.Minute))
	deleted.UpdatedAt = updatedAt
	deleted.DeletedAt = &deletedAt

	if err := s.CreateInstance(ctx, ready); err != nil {
		t.Fatalf("CreateInstance(ready): %v", err)
	}
	if err := s.CreateInstance(ctx, deleted); err != nil {
		t.Fatalf("CreateInstance(deleted): %v", err)
	}

	gotReady, err := s.GetInstance(ctx, ready.Name)
	if err != nil {
		t.Fatalf("GetInstance(%q): %v", ready.Name, err)
	}
	if !reflect.DeepEqual(gotReady, ready) {
		t.Fatalf("GetInstance(%q) mismatch\nwant: %#v\n got: %#v", ready.Name, ready, gotReady)
	}

	visible, err := s.ListInstances(ctx, false)
	if err != nil {
		t.Fatalf("ListInstances(false): %v", err)
	}
	if len(visible) != 1 || visible[0].Name != ready.Name {
		t.Fatalf("ListInstances(false) = %#v, want only %q", visible, ready.Name)
	}

	all, err := s.ListInstances(ctx, true)
	if err != nil {
		t.Fatalf("ListInstances(true): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListInstances(true) returned %d rows, want 2", len(all))
	}
	if all[0].Name != ready.Name || all[1].Name != deleted.Name {
		t.Fatalf("ListInstances(true) order = [%s %s], want [%s %s]", all[0].Name, all[1].Name, ready.Name, deleted.Name)
	}

	ready.State = model.StateFailed
	ready.UpdatedAt = createdAt.Add(5 * time.Minute)
	ready.LastError = "boot failed"
	ready.FirecrackerPID = 1234
	ready.TailscaleName = "alpha.tailnet"
	ready.TailscaleIP = "100.64.0.10"
	if err := s.UpdateInstance(ctx, ready); err != nil {
		t.Fatalf("UpdateInstance(%q): %v", ready.Name, err)
	}

	updated, err := s.GetInstance(ctx, ready.Name)
	if err != nil {
		t.Fatalf("GetInstance(%q) after update: %v", ready.Name, err)
	}
	if !reflect.DeepEqual(updated, ready) {
		t.Fatalf("updated instance mismatch\nwant: %#v\n got: %#v", ready, updated)
	}
}

func TestStoreFindInstanceMissingReturnsFalse(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	inst, found, err := s.FindInstance(ctx, "missing")
	if err != nil {
		t.Fatalf("FindInstance: %v", err)
	}
	if found {
		t.Fatalf("FindInstance reported found for missing instance: %#v", inst)
	}
	if !reflect.DeepEqual(inst, model.Instance{}) {
		t.Fatalf("FindInstance returned unexpected instance for missing row: %#v", inst)
	}
}

func TestStoreListEventsDefaultsToTwentyNewestFirst(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	inst := testInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	if err := s.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	base := time.Date(2026, time.March, 29, 13, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		event := model.InstanceEvent{
			InstanceID: inst.ID,
			CreatedAt:  base.Add(time.Duration(i) * time.Second),
			Type:       "status",
			Message:    "event",
			Payload:    "payload",
		}
		if err := s.RecordEvent(ctx, event); err != nil {
			t.Fatalf("RecordEvent(%d): %v", i, err)
		}
	}

	events, err := s.ListEvents(ctx, inst.ID, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 20 {
		t.Fatalf("ListEvents returned %d rows, want 20", len(events))
	}
	if !events[0].CreatedAt.Equal(base.Add(24 * time.Second)) {
		t.Fatalf("newest event time = %s, want %s", events[0].CreatedAt, base.Add(24*time.Second))
	}
	if !events[len(events)-1].CreatedAt.Equal(base.Add(5 * time.Second)) {
		t.Fatalf("oldest returned event time = %s, want %s", events[len(events)-1].CreatedAt, base.Add(5*time.Second))
	}
}

func TestStoreDeleteInstanceRemovesEvents(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	inst := testInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	if err := s.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if err := s.RecordEvent(ctx, model.InstanceEvent{
		InstanceID: inst.ID,
		CreatedAt:  inst.CreatedAt.Add(time.Second),
		Type:       "create",
		Message:    "created",
	}); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}

	if err := s.DeleteInstance(ctx, inst.Name); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}

	if _, err := s.GetInstance(ctx, inst.Name); err == nil || !reflect.DeepEqual(err, sql.ErrNoRows) && err != sql.ErrNoRows {
		if err == nil {
			t.Fatalf("GetInstance after delete returned nil error")
		}
		if err != sql.ErrNoRows {
			t.Fatalf("GetInstance after delete error = %v, want %v", err, sql.ErrNoRows)
		}
	}

	events, err := s.ListEvents(ctx, inst.ID, 10)
	if err != nil {
		t.Fatalf("ListEvents after delete: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("ListEvents after delete returned %d rows, want 0", len(events))
	}

	if err := s.DeleteInstance(ctx, inst.Name); err != nil {
		t.Fatalf("DeleteInstance on missing row should be a no-op, got %v", err)
	}
}

func TestStoreIntegrationLifecycleAndBindings(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	inst := testInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	if err := s.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	integration := model.Integration{
		ID:          "openai-id",
		Name:        "openai",
		Kind:        model.IntegrationKindHTTP,
		TargetURL:   "https://api.openai.com/v1",
		AuthMode:    model.IntegrationAuthBearerEnv,
		BearerEnv:   "SRV_SECRET_OPENAI_PROD",
		HeadersJSON: `[{"name":"X-App","value":"srv"}]`,
		CreatedAt:   inst.CreatedAt,
		UpdatedAt:   inst.UpdatedAt,
	}
	if err := s.CreateIntegration(ctx, integration); err != nil {
		t.Fatalf("CreateIntegration: %v", err)
	}

	gotIntegration, err := s.GetIntegrationByName(ctx, integration.Name)
	if err != nil {
		t.Fatalf("GetIntegrationByName: %v", err)
	}
	if !reflect.DeepEqual(gotIntegration, integration) {
		t.Fatalf("GetIntegrationByName mismatch\nwant: %#v\n got: %#v", integration, gotIntegration)
	}

	list, err := s.ListIntegrations(ctx)
	if err != nil {
		t.Fatalf("ListIntegrations: %v", err)
	}
	if len(list) != 1 || !reflect.DeepEqual(list[0], integration) {
		t.Fatalf("ListIntegrations = %#v, want %#v", list, []model.Integration{integration})
	}

	binding := model.InstanceIntegrationBinding{
		InstanceID:    inst.ID,
		IntegrationID: integration.ID,
		CreatedAt:     inst.CreatedAt.Add(time.Minute),
		CreatedByUser: inst.CreatedByUser,
		CreatedByNode: inst.CreatedByNode,
	}
	if err := s.BindIntegrationToInstance(ctx, binding); err != nil {
		t.Fatalf("BindIntegrationToInstance: %v", err)
	}

	bound, err := s.ListInstanceIntegrations(ctx, inst.ID)
	if err != nil {
		t.Fatalf("ListInstanceIntegrations: %v", err)
	}
	if len(bound) != 1 || !reflect.DeepEqual(bound[0], integration) {
		t.Fatalf("ListInstanceIntegrations = %#v, want [%#v]", bound, integration)
	}

	count, err := s.CountIntegrationBindings(ctx, integration.ID)
	if err != nil {
		t.Fatalf("CountIntegrationBindings: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountIntegrationBindings = %d, want 1", count)
	}

	if err := s.UnbindIntegrationFromInstance(ctx, inst.ID, integration.ID); err != nil {
		t.Fatalf("UnbindIntegrationFromInstance: %v", err)
	}
	count, err = s.CountIntegrationBindings(ctx, integration.ID)
	if err != nil {
		t.Fatalf("CountIntegrationBindings after unbind: %v", err)
	}
	if count != 0 {
		t.Fatalf("CountIntegrationBindings after unbind = %d, want 0", count)
	}

	if err := s.BindIntegrationToInstance(ctx, binding); err != nil {
		t.Fatalf("BindIntegrationToInstance second time: %v", err)
	}
	if err := s.DeleteInstance(ctx, inst.Name); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	count, err = s.CountIntegrationBindings(ctx, integration.ID)
	if err != nil {
		t.Fatalf("CountIntegrationBindings after instance delete: %v", err)
	}
	if count != 0 {
		t.Fatalf("CountIntegrationBindings after instance delete = %d, want 0", count)
	}

	if err := s.DeleteIntegration(ctx, integration.Name); err != nil {
		t.Fatalf("DeleteIntegration: %v", err)
	}
	if _, err := s.GetIntegrationByName(ctx, integration.Name); err != sql.ErrNoRows {
		t.Fatalf("GetIntegrationByName after delete error = %v, want %v", err, sql.ErrNoRows)
	}
}

func TestStoreRecordAuditRows(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	commandTime := time.Date(2026, time.March, 29, 14, 0, 0, 0, time.UTC)
	if err := s.RecordCommand(ctx, model.CommandAudit{
		CreatedAt:        commandTime,
		ActorUser:        "alice@example.com",
		ActorDisplayName: "Alice",
		ActorNode:        "laptop",
		RemoteAddr:       "100.64.0.1:1234",
		SSHUser:          "root",
		Command:          "list",
		ArgsJSON:         `["list"]`,
		Allowed:          true,
		Reason:           "allowlisted",
		DurationMS:       42,
		ErrorText:        "",
	}); err != nil {
		t.Fatalf("RecordCommand: %v", err)
	}

	if err := s.RecordAuthz(ctx, model.AuthzDecision{
		CreatedAt:  commandTime,
		ActorUser:  "alice@example.com",
		ActorNode:  "laptop",
		RemoteAddr: "100.64.0.1:1234",
		Action:     "list",
		Allowed:    false,
		Reason:     "denylisted",
	}); err != nil {
		t.Fatalf("RecordAuthz: %v", err)
	}

	var actorUser, displayName, actorNode, remoteAddr, sshUser, command, argsJSON, reason, errorText string
	var allowed int
	var durationMS int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT actor_user, actor_display_name, actor_node, remote_addr, ssh_user, command, args_json, allowed, reason, duration_ms, error_text
		FROM commands
	`).Scan(&actorUser, &displayName, &actorNode, &remoteAddr, &sshUser, &command, &argsJSON, &allowed, &reason, &durationMS, &errorText); err != nil {
		t.Fatalf("query commands row: %v", err)
	}
	if actorUser != "alice@example.com" || displayName != "Alice" || actorNode != "laptop" || remoteAddr != "100.64.0.1:1234" || sshUser != "root" || command != "list" || argsJSON != `["list"]` || allowed != 1 || reason != "allowlisted" || durationMS != 42 || errorText != "" {
		t.Fatalf("unexpected commands row: user=%q display=%q node=%q remote=%q ssh=%q command=%q args=%q allowed=%d reason=%q duration=%d error=%q", actorUser, displayName, actorNode, remoteAddr, sshUser, command, argsJSON, allowed, reason, durationMS, errorText)
	}

	var authActorUser, authActorNode, authRemoteAddr, action, authReason string
	var authAllowed int
	if err := s.db.QueryRowContext(ctx, `
		SELECT actor_user, actor_node, remote_addr, action, allowed, reason
		FROM authz_decisions
	`).Scan(&authActorUser, &authActorNode, &authRemoteAddr, &action, &authAllowed, &authReason); err != nil {
		t.Fatalf("query authz row: %v", err)
	}
	if authActorUser != "alice@example.com" || authActorNode != "laptop" || authRemoteAddr != "100.64.0.1:1234" || action != "list" || authAllowed != 0 || authReason != "denylisted" {
		t.Fatalf("unexpected authz row: user=%q node=%q remote=%q action=%q allowed=%d reason=%q", authActorUser, authActorNode, authRemoteAddr, action, authAllowed, authReason)
	}
}

func TestOpenMigratesExistingInstancesTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "app.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(): %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE instances (
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
		);
		CREATE TABLE instance_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			instance_id TEXT NOT NULL,
			created_at TEXT NOT NULL,
			type TEXT NOT NULL,
			message TEXT NOT NULL,
			payload TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE commands (
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
		);
		CREATE TABLE authz_decisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT NOT NULL,
			actor_user TEXT NOT NULL,
			actor_node TEXT NOT NULL,
			remote_addr TEXT NOT NULL,
			action TEXT NOT NULL,
			allowed INTEGER NOT NULL,
			reason TEXT NOT NULL
		);
	`); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
	})

	rows, err := s.db.Query(`PRAGMA table_info(instances)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(instances): %v", err)
	}
	defer rows.Close()

	columns := map[string]bool{}
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
			t.Fatalf("scan table_info row: %v", err)
		}
		columns[name] = true
	}
	for _, want := range []string{"vcpu_count", "memory_mib", "memory_mode", "memory_pool_id", "rootfs_size_bytes"} {
		if !columns[want] {
			t.Fatalf("missing migrated column %q in %#v", want, columns)
		}
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "app.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
	})
	return s
}

func testInstance(name, state string, createdAt time.Time) model.Instance {
	updatedAt := createdAt.Add(30 * time.Second)
	baseDir := filepath.Join("/tmp", name)
	return model.Instance{
		ID:              name + "-id",
		Name:            name,
		State:           state,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
		CreatedByUser:   "alice@example.com",
		CreatedByNode:   "laptop",
		VCPUCount:       2,
		MemoryMiB:       2048,
		MemoryMode:      model.MemoryModeFixed,
		RootFSSizeBytes: 4 << 30,
		RootFSPath:      filepath.Join(baseDir, "rootfs.img"),
		KernelPath:      filepath.Join(baseDir, "vmlinux"),
		InitrdPath:      filepath.Join(baseDir, "initrd.img"),
		SocketPath:      filepath.Join(baseDir, "firecracker.sock"),
		LogPath:         filepath.Join(baseDir, "firecracker.log"),
		SerialLogPath:   filepath.Join(baseDir, "serial.log"),
		TapDevice:       "tap-1234567890",
		GuestMAC:        "02:fc:aa:bb:cc:dd",
		NetworkCIDR:     "172.28.0.0/30",
		HostAddr:        "172.28.0.1/30",
		GuestAddr:       "172.28.0.2/30",
		GatewayAddr:     "172.28.0.1",
	}
}
