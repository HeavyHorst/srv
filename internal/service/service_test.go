package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"srv/internal/config"
	"srv/internal/format"
	"srv/internal/model"
	"srv/internal/provision"
	"srv/internal/store"
)

func TestAuthorize(t *testing.T) {
	actor := model.Actor{UserLogin: "alice@example.com"}

	tests := []struct {
		name         string
		allowedUsers []string
		adminUsers   []string
		actor        model.Actor
		command      string
		wantAllowed  bool
		wantReason   string
	}{
		{
			name:         "empty allowlist permits all",
			allowedUsers: nil,
			actor:        actor,
			command:      "list",
			wantAllowed:  true,
			wantReason:   "allowed because SRV_ALLOWED_USERS is empty",
		},
		{
			name:         "allowlist matches case-insensitively",
			allowedUsers: []string{"ALICE@example.com"},
			actor:        actor,
			command:      "new",
			wantAllowed:  true,
			wantReason:   "alice@example.com allowed to run new",
		},
		{
			name:         "unknown user denied",
			allowedUsers: []string{"bob@example.com"},
			actor:        actor,
			command:      "delete",
			wantAllowed:  false,
			wantReason:   "alice@example.com is not in SRV_ALLOWED_USERS",
		},
		{
			name:        "status requires admin",
			actor:       actor,
			command:     "status",
			wantAllowed: false,
			wantReason:  "alice@example.com is not in SRV_ADMIN_USERS",
		},
		{
			name:        "status allows admin",
			adminUsers:  []string{"alice@example.com"},
			actor:       actor,
			command:     "status",
			wantAllowed: true,
			wantReason:  "alice@example.com allowed to run status as admin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := &App{cfg: config.Config{AllowedUsers: tt.allowedUsers, AdminUsers: tt.adminUsers}}
			allowed, reason := app.authorize(tt.actor, tt.command)
			if allowed != tt.wantAllowed {
				t.Fatalf("authorize() allowed = %v, want %v", allowed, tt.wantAllowed)
			}
			if reason != tt.wantReason {
				t.Fatalf("authorize() reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func TestCmdListFormatsVisibleInstances(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	app := &App{
		cfg:   config.Config{Hostname: "srv"},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}

	ready := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	ready.TailscaleIP = "100.64.0.10"
	ready.TailscaleName = "alpha.tailnet"
	otherOwner := serviceTestInstance("beta", model.StateStopped, ready.CreatedAt.Add(time.Minute))
	otherOwner.CreatedByUser = "bob@example.com"
	deleted := serviceTestInstance("gamma", model.StateDeleted, ready.CreatedAt.Add(2*time.Minute))

	for _, inst := range []model.Instance{ready, otherOwner, deleted} {
		if err := st.CreateInstance(ctx, inst); err != nil {
			t.Fatalf("CreateInstance(%s): %v", inst.Name, err)
		}
	}

	result, err := app.cmdList(ctx, model.Actor{UserLogin: "alice@example.com"}, outputFormatText)
	if err != nil {
		t.Fatalf("cmdList(): %v", err)
	}
	if result.exitCode != 0 {
		t.Fatalf("cmdList() exitCode = %d, want 0", result.exitCode)
	}
	upperOutput := strings.ToUpper(result.stdout)
	for _, want := range []string{"NAME", "STATE", "VCPUS", "MEMORY", "ROOTFS SIZE", "TAILSCALE IP", "TAILSCALE NAME"} {
		if !strings.Contains(upperOutput, want) {
			t.Fatalf("cmdList() stdout missing table header %q\nfull output:\n%s", want, result.stdout)
		}
	}
	rows := listOutputRows(result.stdout)
	if got := rows["alpha"]; !reflect.DeepEqual(got, []string{"alpha", "ready", "2", "2.0 GiB", "4.0 GiB", "100.64.0.10", "alpha.tailnet"}) {
		t.Fatalf("cmdList() parsed row = %v, want %v\nfull output:\n%s", got, []string{"alpha", "ready", "2", "2.0 GiB", "4.0 GiB", "100.64.0.10", "alpha.tailnet"}, result.stdout)
	}
}

func TestCmdListAdminSeesAllVisibleInstances(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	app := &App{
		cfg:   config.Config{Hostname: "srv", AdminUsers: []string{"ops@example.com"}},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}

	alpha := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	beta := serviceTestInstance("beta", model.StateStopped, alpha.CreatedAt.Add(time.Minute))
	beta.CreatedByUser = "bob@example.com"

	for _, inst := range []model.Instance{alpha, beta} {
		if err := st.CreateInstance(ctx, inst); err != nil {
			t.Fatalf("CreateInstance(%s): %v", inst.Name, err)
		}
	}

	result, err := app.cmdList(ctx, model.Actor{UserLogin: "ops@example.com"}, outputFormatText)
	if err != nil {
		t.Fatalf("cmdList(): %v", err)
	}
	rows := listOutputRows(result.stdout)
	if got := rows["alpha"]; len(got) < 2 || got[1] != "ready" {
		t.Fatalf("cmdList() alpha row = %v\nfull output:\n%s", got, result.stdout)
	}
	if got := rows["beta"]; len(got) < 2 || got[1] != "stopped" {
		t.Fatalf("cmdList() beta row = %v\nfull output:\n%s", got, result.stdout)
	}
}

func TestCmdListJSONReturnsStructuredInstances(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	app := &App{
		cfg:   config.Config{Hostname: "srv"},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}

	inst := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	inst.TailscaleIP = "100.64.0.10"
	inst.TailscaleName = "alpha.tailnet"
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	result, err := app.cmdList(ctx, model.Actor{UserLogin: "alice@example.com"}, outputFormatJSON)
	if err != nil {
		t.Fatalf("cmdList(json): %v", err)
	}

	var payload struct {
		Instances []instanceSummaryJSON `json:"instances"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &payload); err != nil {
		t.Fatalf("json.Unmarshal(cmdList): %v\noutput:\n%s", err, result.stdout)
	}
	if len(payload.Instances) != 1 {
		t.Fatalf("cmdList(json) instances = %#v", payload.Instances)
	}
	got := payload.Instances[0]
	if got.Name != "alpha" || got.State != model.StateReady || got.VCPUCount != 2 || got.MemoryMiB != 2048 || got.RootFSSizeBytes != 4<<30 || got.TailscaleIP != "100.64.0.10" || got.TailscaleName != "alpha.tailnet" {
		t.Fatalf("cmdList(json) instance = %#v", got)
	}
}

func TestCmdStatusFormatsCapacitySummary(t *testing.T) {
	ctx := context.Background()
	cfg := loadServiceTestConfig(t, nil)
	cfg.AdminUsers = []string{"ops@example.com"}
	st := newServiceTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	prov, err := provision.New(cfg, logger, st)
	if err != nil {
		t.Fatalf("provision.New(): %v", err)
	}
	app := &App{
		cfg:         cfg,
		log:         logger,
		store:       st,
		provisioner: prov,
	}

	ready := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC))
	ready.FirecrackerPID = os.Getpid()
	stopped := serviceTestInstance("beta", model.StateStopped, ready.CreatedAt.Add(time.Minute))
	failed := serviceTestInstance("gamma", model.StateFailed, ready.CreatedAt.Add(2*time.Minute))
	failed.RootFSPath = filepath.Join(cfg.InstancesDir(), failed.Name, "rootfs.img")
	failed.RootFSSizeBytes = 2 << 30
	failedAllocated := writeSparseServiceTestFile(t, failed.RootFSPath, failed.RootFSSizeBytes)
	deleting := serviceTestInstance("epsilon", model.StateDeleting, ready.CreatedAt.Add(3*time.Minute))
	deleting.RootFSPath = filepath.Join(cfg.InstancesDir(), deleting.Name, "rootfs.img")
	deleting.RootFSSizeBytes = 2 << 30
	deletingAllocated := writeSparseServiceTestFile(t, deleting.RootFSPath, deleting.RootFSSizeBytes)
	deleted := serviceTestInstance("delta", model.StateDeleted, ready.CreatedAt.Add(3*time.Minute))
	deleted.RootFSPath = filepath.Join(cfg.InstancesDir(), deleted.Name, "rootfs.img")
	deleted.RootFSSizeBytes = 2 << 30
	deletedAllocated := writeSparseServiceTestFile(t, deleted.RootFSPath, deleted.RootFSSizeBytes)
	deletedAt := deleted.CreatedAt.Add(time.Minute)
	deleted.DeletedAt = &deletedAt
	expectedDiskAllocated := ready.RootFSSizeBytes + stopped.RootFSSizeBytes + failedAllocated + deletingAllocated + deletedAllocated

	for _, inst := range []model.Instance{ready, stopped, failed, deleting, deleted} {
		if err := st.CreateInstance(ctx, inst); err != nil {
			t.Fatalf("CreateInstance(%s): %v", inst.Name, err)
		}
	}

	result, err := app.cmdStatus(ctx, model.Actor{UserLogin: "ops@example.com"}, []string{"status"}, outputFormatText)
	if err != nil {
		t.Fatalf("cmdStatus(): %v", err)
	}
	if result.exitCode != 0 {
		t.Fatalf("cmdStatus() exitCode = %d, want 0", result.exitCode)
	}
	for _, want := range []string{
		"srv",
		"total",
		"running",
		"stopped",
		"failed",
		"CPU",
		"MEMORY",
		"DISK",
		"advisory only; overcommit allowed",
		format.BinarySize(expectedDiskAllocated),
		"┌",
		"┐",
		"└",
		"┘",
		"│",
	} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("cmdStatus() stdout missing %q\nfull output:\n%s", want, result.stdout)
		}
	}
	if strings.Contains(result.stdout, "\nready:") {
		t.Fatalf("cmdStatus() included duplicated state block\nfull output:\n%s", result.stdout)
	}
}

func TestCmdStatusJSONReturnsStructuredSummary(t *testing.T) {
	ctx := context.Background()
	cfg := loadServiceTestConfig(t, nil)
	cfg.AdminUsers = []string{"ops@example.com"}
	st := newServiceTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	prov, err := provision.New(cfg, logger, st)
	if err != nil {
		t.Fatalf("provision.New(): %v", err)
	}
	app := &App{
		cfg:         cfg,
		log:         logger,
		store:       st,
		provisioner: prov,
	}

	ready := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC))
	ready.FirecrackerPID = os.Getpid()
	stopped := serviceTestInstance("beta", model.StateStopped, ready.CreatedAt.Add(time.Minute))
	failed := serviceTestInstance("gamma", model.StateFailed, ready.CreatedAt.Add(2*time.Minute))
	failed.RootFSPath = filepath.Join(cfg.InstancesDir(), failed.Name, "rootfs.img")
	failed.RootFSSizeBytes = 2 << 30
	failedAllocated := writeSparseServiceTestFile(t, failed.RootFSPath, failed.RootFSSizeBytes)
	deleting := serviceTestInstance("epsilon", model.StateDeleting, ready.CreatedAt.Add(3*time.Minute))
	deleting.RootFSPath = filepath.Join(cfg.InstancesDir(), deleting.Name, "rootfs.img")
	deleting.RootFSSizeBytes = 2 << 30
	deletingAllocated := writeSparseServiceTestFile(t, deleting.RootFSPath, deleting.RootFSSizeBytes)
	deleted := serviceTestInstance("delta", model.StateDeleted, ready.CreatedAt.Add(3*time.Minute))
	deleted.RootFSPath = filepath.Join(cfg.InstancesDir(), deleted.Name, "rootfs.img")
	deleted.RootFSSizeBytes = 2 << 30
	deletedAllocated := writeSparseServiceTestFile(t, deleted.RootFSPath, deleted.RootFSSizeBytes)
	deletedAt := deleted.CreatedAt.Add(time.Minute)
	deleted.DeletedAt = &deletedAt
	expectedDiskAllocated := ready.RootFSSizeBytes + stopped.RootFSSizeBytes + failedAllocated + deletingAllocated + deletedAllocated

	for _, inst := range []model.Instance{ready, stopped, failed, deleting, deleted} {
		if err := st.CreateInstance(ctx, inst); err != nil {
			t.Fatalf("CreateInstance(%s): %v", inst.Name, err)
		}
	}

	result, err := app.cmdStatus(ctx, model.Actor{UserLogin: "ops@example.com"}, []string{"status"}, outputFormatJSON)
	if err != nil {
		t.Fatalf("cmdStatus(json): %v", err)
	}

	var payload statusResponseJSON
	if err := json.Unmarshal([]byte(result.stdout), &payload); err != nil {
		t.Fatalf("json.Unmarshal(cmdStatus): %v\noutput:\n%s", err, result.stdout)
	}
	if payload.Hostname != "srv" {
		t.Fatalf("status hostname = %q, want %q", payload.Hostname, "srv")
	}
	if payload.Instances.Total != 4 || payload.Instances.Running != 1 || payload.Instances.Stopped != 1 || payload.Instances.Failed != 1 {
		t.Fatalf("status instances = %#v", payload.Instances)
	}
	if payload.Instances.ByState[model.StateReady] != 1 || payload.Instances.ByState[model.StateStopped] != 1 || payload.Instances.ByState[model.StateFailed] != 1 || payload.Instances.ByState[model.StateDeleting] != 1 {
		t.Fatalf("status by_state = %#v", payload.Instances.ByState)
	}
	if _, ok := payload.Instances.ByState[model.StateDeleted]; ok {
		t.Fatalf("status by_state unexpectedly included deleted state: %#v", payload.Instances.ByState)
	}

	resources := make(map[string]statusResourceJSON, len(payload.Capacity))
	for _, resource := range payload.Capacity {
		resources[resource.Resource] = resource
	}
	cpu := resources["cpu"]
	if cpu.Allocated != 2 || cpu.Budget != int64(runtime.NumCPU()) || cpu.Left != cpu.Budget-cpu.Allocated || !cpu.Advisory {
		t.Fatalf("cpu status = %#v", cpu)
	}
	memory := resources["memory"]
	if memory.Allocated != 2048*mib || memory.Reserve != 512*mib || memory.Budget != max(memory.Total-memory.Reserve, int64(0)) || memory.Left != memory.Budget-memory.Allocated {
		t.Fatalf("memory status = %#v", memory)
	}
	disk := resources["disk"]
	if disk.Allocated != expectedDiskAllocated || disk.Reserve != 1<<30 || disk.Budget != max(disk.Total-disk.Reserve, int64(0)) || disk.Left != disk.Budget-disk.Allocated {
		t.Fatalf("disk status = %#v", disk)
	}
}

func TestCmdBackupListFormatsBackupsAsTable(t *testing.T) {
	ctx := context.Background()
	cfg := loadServiceTestConfig(t, nil)
	st := newServiceTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	prov, err := provision.New(cfg, logger, st)
	if err != nil {
		t.Fatalf("provision.New(): %v", err)
	}
	app := &App{cfg: cfg, log: logger, store: st, provisioner: prov}

	inst := serviceTestInstance("alpha", model.StateStopped, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	inst.RootFSSizeBytes = 8 << 30
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance(): %v", err)
	}

	backupID := "20260331T120000.000000000Z"
	backupDir := filepath.Join(cfg.BackupsDir(), inst.Name, backupID)
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(backup dir): %v", err)
	}
	manifest := map[string]any{
		"version":    1,
		"id":         backupID,
		"created_at": inst.CreatedAt.Add(5 * time.Minute),
		"instance":   inst,
		"files": map[string]any{
			"rootfs":          "rootfs.img",
			"serial_log":      "serial.log",
			"firecracker_log": "firecracker.log",
		},
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest): %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "manifest.json"), payload, 0o644); err != nil {
		t.Fatalf("WriteFile(manifest): %v", err)
	}

	result, err := app.cmdBackup(ctx, model.Actor{UserLogin: "alice@example.com"}, []string{"backup", "list", inst.Name}, outputFormatText)
	if err != nil {
		t.Fatalf("cmdBackup(list): %v", err)
	}
	if result.exitCode != 0 {
		t.Fatalf("cmdBackup(list) exitCode = %d, want 0", result.exitCode)
	}
	upperOutput := strings.ToUpper(result.stdout)
	for _, want := range []string{"backups for alpha:", "ID", "Created At", "RootFS Size", "VCPUs", "Memory", "Logs", backupID, "8.0 GiB", "serial,firecracker"} {
		if !strings.Contains(upperOutput, strings.ToUpper(want)) {
			t.Fatalf("cmdBackup(list) stdout missing %q\nfull output:\n%s", want, result.stdout)
		}
	}
}

func TestCmdBackupListJSONReturnsStructuredBackups(t *testing.T) {
	ctx := context.Background()
	cfg := loadServiceTestConfig(t, nil)
	st := newServiceTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	prov, err := provision.New(cfg, logger, st)
	if err != nil {
		t.Fatalf("provision.New(): %v", err)
	}
	app := &App{cfg: cfg, log: logger, store: st, provisioner: prov}

	inst := serviceTestInstance("alpha", model.StateStopped, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	inst.RootFSSizeBytes = 8 << 30
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance(): %v", err)
	}

	backupID := "20260331T120000.000000000Z"
	backupDir := filepath.Join(cfg.BackupsDir(), inst.Name, backupID)
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(backup dir): %v", err)
	}
	manifest := map[string]any{
		"version":    1,
		"id":         backupID,
		"created_at": inst.CreatedAt.Add(5 * time.Minute),
		"instance":   inst,
		"files": map[string]any{
			"rootfs":          "rootfs.img",
			"serial_log":      "serial.log",
			"firecracker_log": "firecracker.log",
		},
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest): %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "manifest.json"), payload, 0o644); err != nil {
		t.Fatalf("WriteFile(manifest): %v", err)
	}

	result, err := app.cmdBackup(ctx, model.Actor{UserLogin: "alice@example.com"}, []string{"backup", "list", inst.Name}, outputFormatJSON)
	if err != nil {
		t.Fatalf("cmdBackup(list json): %v", err)
	}

	var response backupListResponseJSON
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
		t.Fatalf("json.Unmarshal(cmdBackup): %v\noutput:\n%s", err, result.stdout)
	}
	if response.Instance != inst.Name || len(response.Backups) != 1 {
		t.Fatalf("cmdBackup(list json) response = %#v", response)
	}
	backup := response.Backups[0]
	if backup.ID != backupID || backup.RootFSSizeBytes != 8<<30 || !backup.HasSerialLog || !backup.HasFirecrackerLog {
		t.Fatalf("cmdBackup(list json) backup = %#v", backup)
	}
}

func listOutputRows(output string) map[string][]string {
	rows := make(map[string][]string)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if fields := boxedTableFields(trimmed); len(fields) > 0 {
			if strings.EqualFold(fields[0], "name") {
				continue
			}
			rows[fields[0]] = fields
			continue
		}
		if strings.Trim(trimmed, "-+┌┐└┘├┤┬┴┼─ ") == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 || strings.EqualFold(fields[0], "name") {
			continue
		}
		rows[fields[0]] = fields
	}
	return rows
}

func boxedTableFields(line string) []string {
	separator := ""
	if strings.Contains(line, "│") {
		separator = "│"
	} else if strings.Count(line, "|") >= 2 {
		separator = "|"
	}
	if separator == "" {
		return nil
	}
	parts := strings.Split(line, separator)
	fields := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields = append(fields, part)
	}
	return fields
}

func TestCmdInspectFormatsInstanceAndEvents(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	app := &App{
		cfg:   config.Config{Hostname: "srv", VCPUCount: 1, MemoryMiB: 1024},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}

	inst := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	inst.VCPUCount = 4
	inst.MemoryMiB = 4096
	inst.RootFSSizeBytes = 8 << 30
	inst.TailscaleName = "alpha.tailnet"
	inst.TailscaleIP = "100.64.0.10"
	inst.LastError = "previous boot hiccup"
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if err := st.RecordEvent(ctx, model.InstanceEvent{
		InstanceID: inst.ID,
		CreatedAt:  inst.CreatedAt.Add(10 * time.Second),
		Type:       "create",
		Message:    "instance record created",
	}); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}

	result, err := app.cmdInspect(ctx, model.Actor{UserLogin: "alice@example.com"}, []string{"inspect", inst.Name}, outputFormatText)
	if err != nil {
		t.Fatalf("cmdInspect(): %v", err)
	}
	if result.exitCode != 0 {
		t.Fatalf("cmdInspect() exitCode = %d, want 0", result.exitCode)
	}

	wants := []string{
		"name: alpha\n",
		"state: ready\n",
		"created-by: alice@example.com via laptop\n",
		"vcpus: 4\n",
		"memory: 4096 MiB\n",
		"rootfs-size: 8.0 GiB\n",
		"tailscale-name: alpha.tailnet\n",
		"tailscale-ip: 100.64.0.10\n",
		"last-error: previous boot hiccup\n",
		"logs-serial: ssh srv logs alpha serial\n",
		"logs-firecracker: ssh srv logs alpha firecracker\n",
		"debug-hint: boot and runtime failures usually show up first in the serial log, then in the Firecracker log\n",
		"events:\n",
		"- 2026-03-29T12:00:10Z [create] instance record created\n",
	}
	for _, want := range wants {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("cmdInspect() stdout missing %q\nfull output:\n%s", want, result.stdout)
		}
	}
}

func TestCmdInspectJSONReturnsStructuredInstanceAndEvents(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	app := &App{
		cfg:   config.Config{Hostname: "srv", VCPUCount: 1, MemoryMiB: 1024},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}

	inst := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	inst.VCPUCount = 4
	inst.MemoryMiB = 4096
	inst.RootFSSizeBytes = 8 << 30
	inst.TailscaleName = "alpha.tailnet"
	inst.TailscaleIP = "100.64.0.10"
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if err := st.RecordEvent(ctx, model.InstanceEvent{
		InstanceID: inst.ID,
		CreatedAt:  inst.CreatedAt.Add(10 * time.Second),
		Type:       "create",
		Message:    "instance record created",
	}); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}

	result, err := app.cmdInspect(ctx, model.Actor{UserLogin: "alice@example.com"}, []string{"inspect", inst.Name}, outputFormatJSON)
	if err != nil {
		t.Fatalf("cmdInspect(json): %v", err)
	}

	var payload inspectResponseJSON
	if err := json.Unmarshal([]byte(result.stdout), &payload); err != nil {
		t.Fatalf("json.Unmarshal(cmdInspect): %v\noutput:\n%s", err, result.stdout)
	}
	if payload.Instance.Name != inst.Name || payload.Instance.State != model.StateReady || payload.Instance.VCPUCount != 4 || payload.Instance.MemoryMiB != 4096 || payload.Instance.RootFSSizeBytes != 8<<30 {
		t.Fatalf("cmdInspect(json) instance = %#v", payload.Instance)
	}
	if payload.Instance.Logs.SerialCommand != "ssh srv logs alpha serial" || payload.Instance.Logs.FirecrackerCommand != "ssh srv logs alpha firecracker" {
		t.Fatalf("cmdInspect(json) logs = %#v", payload.Instance.Logs)
	}
	if len(payload.Events) != 1 || payload.Events[0].Type != "create" || payload.Events[0].Message != "instance record created" {
		t.Fatalf("cmdInspect(json) events = %#v", payload.Events)
	}
}

func TestCmdInspectMissingInstanceReturnsFriendlyError(t *testing.T) {
	app := &App{
		cfg:   config.Config{Hostname: "srv"},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: newServiceTestStore(t),
	}

	result, err := app.cmdInspect(context.Background(), model.Actor{UserLogin: "alice@example.com"}, []string{"inspect", "missing"}, outputFormatText)
	if err == nil {
		t.Fatalf("cmdInspect() error = nil, want non-nil")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cmdInspect() returned unrelated not-exist error: %v", err)
	}
	if !strings.Contains(err.Error(), `instance "missing" does not exist`) {
		t.Fatalf("cmdInspect() error = %q, want friendly missing-instance message", err.Error())
	}
	if result.exitCode != 1 {
		t.Fatalf("cmdInspect() exitCode = %d, want 1", result.exitCode)
	}
	if !strings.Contains(result.stderr, `inspect missing: instance "missing" does not exist`) {
		t.Fatalf("cmdInspect() stderr = %q", result.stderr)
	}
}

func TestCmdInspectHidesInstancesFromOtherUsers(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	app := &App{
		cfg:   config.Config{Hostname: "srv"},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}

	inst := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	result, err := app.cmdInspect(ctx, model.Actor{UserLogin: "bob@example.com"}, []string{"inspect", inst.Name}, outputFormatText)
	if err == nil {
		t.Fatalf("cmdInspect() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), `instance "alpha" does not exist`) {
		t.Fatalf("cmdInspect() error = %q", err.Error())
	}
	if !strings.Contains(result.stderr, `inspect alpha: instance "alpha" does not exist`) {
		t.Fatalf("cmdInspect() stderr = %q", result.stderr)
	}
}

func TestCmdInspectAwaitingTailnetPointsToSerialLog(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	app := &App{
		cfg:   config.Config{Hostname: "srv"},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}

	inst := serviceTestInstance("alpha", model.StateAwaitingTailnet, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	result, err := app.cmdInspect(ctx, model.Actor{UserLogin: "alice@example.com"}, []string{"inspect", inst.Name}, outputFormatText)
	if err != nil {
		t.Fatalf("cmdInspect(): %v", err)
	}
	if !strings.Contains(result.stdout, "debug-hint: guest has not finished initial tailnet bootstrap; start with the serial log\n") {
		t.Fatalf("cmdInspect() stdout = %q", result.stdout)
	}
}

func TestTrimNodeName(t *testing.T) {
	if got := trimNodeName("node.example.", "fallback."); got != "node.example" {
		t.Fatalf("trimNodeName(primary) = %q, want %q", got, "node.example")
	}
	if got := trimNodeName("", "fallback."); got != "fallback" {
		t.Fatalf("trimNodeName(fallback) = %q, want %q", got, "fallback")
	}
}

func TestEnsureHostSignerPersistsKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "host_key")

	signer1, err := ensureHostSigner(path)
	if err != nil {
		t.Fatalf("ensureHostSigner(create): %v", err)
	}
	signer2, err := ensureHostSigner(path)
	if err != nil {
		t.Fatalf("ensureHostSigner(reuse): %v", err)
	}

	if !bytes.Equal(signer1.PublicKey().Marshal(), signer2.PublicKey().Marshal()) {
		t.Fatalf("public keys differ between create and reuse")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("host key mode = %o, want 600", info.Mode().Perm())
	}
}

func TestHelpResultIncludesLifecycleCommands(t *testing.T) {
	result := helpResult()
	for _, want := range []string{
		"ssh srv [--json] <command>",
		"new <name> [--cpus N] [--ram SIZE] [--rootfs-size SIZE]",
		"resize <name> [--cpus N] [--ram SIZE] [--rootfs-size SIZE]",
		"backup create <name>",
		"backup list <name>",
		"export <name>",
		"import",
		"logs <name> [serial|firecracker]",
		"logs -f <name> <target>",
		"status",
		"restore <name> <backup-id>",
		"start <name>",
		"stop <name>",
		"restart <name>",
		"GLOBAL OPTIONS",
		"--json",
		"NEW AND RESIZE OPTIONS",
		"--cpus N",
		"--ram SIZE",
		"--rootfs-size SIZE",
	} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("helpResult() missing %q in %q", want, result.stdout)
		}
	}
}

func TestParseCommandRequestSupportsGlobalJSONFlag(t *testing.T) {
	req, err := parseCommandRequest([]string{"--json", "inspect", "alpha"})
	if err != nil {
		t.Fatalf("parseCommandRequest(): %v", err)
	}
	if req.format != outputFormatJSON {
		t.Fatalf("parseCommandRequest() format = %v, want %v", req.format, outputFormatJSON)
	}
	if !reflect.DeepEqual(req.args, []string{"inspect", "alpha"}) {
		t.Fatalf("parseCommandRequest() args = %#v", req.args)
	}
}

func TestMaybeUnsupportedJSONCommand(t *testing.T) {
	tests := []struct {
		name         string
		command      string
		format       outputFormat
		wantRejected bool
		wantStderr   string
	}{
		{name: "logs json rejected", command: "logs", format: outputFormatJSON, wantRejected: true, wantStderr: "logs does not support --json\n"},
		{name: "export json rejected", command: "export", format: outputFormatJSON, wantRejected: true, wantStderr: "export does not support --json\n"},
		{name: "import json rejected", command: "import", format: outputFormatJSON, wantRejected: true, wantStderr: "import does not support --json\n"},
		{name: "snapshot json rejected", command: "snapshot", format: outputFormatJSON, wantRejected: true, wantStderr: "snapshot does not support --json\n"},
		{name: "help json rejected", command: "help", format: outputFormatJSON, wantRejected: true, wantStderr: "help does not support --json\n"},
		{name: "list json allowed", command: "list", format: outputFormatJSON, wantRejected: false},
		{name: "logs text allowed", command: "logs", format: outputFormatText, wantRejected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err, rejected := maybeUnsupportedJSONCommand(tt.command, tt.format)
			if rejected != tt.wantRejected {
				t.Fatalf("maybeUnsupportedJSONCommand() rejected = %v, want %v", rejected, tt.wantRejected)
			}
			if !tt.wantRejected {
				if err != nil {
					t.Fatalf("maybeUnsupportedJSONCommand() unexpected error = %v", err)
				}
				if result != (commandResult{}) {
					t.Fatalf("maybeUnsupportedJSONCommand() unexpected result = %#v", result)
				}
				return
			}
			if err == nil {
				t.Fatalf("maybeUnsupportedJSONCommand() error = nil, want non-nil")
			}
			if result.exitCode != 2 || result.stderr != tt.wantStderr {
				t.Fatalf("maybeUnsupportedJSONCommand() result = %#v, want stderr %q exitCode 2", result, tt.wantStderr)
			}
		})
	}
}

func TestDispatchRejectsJSONHelp(t *testing.T) {
	app := &App{}
	result, err := app.dispatch(context.Background(), model.Actor{}, commandRequest{args: []string{"help"}, format: outputFormatJSON})
	if err == nil {
		t.Fatalf("dispatch(help json) error = nil, want non-nil")
	}
	if result.exitCode != 2 {
		t.Fatalf("dispatch(help json) exitCode = %d, want 2", result.exitCode)
	}
	if result.stderr != "help does not support --json\n" {
		t.Fatalf("dispatch(help json) stderr = %q", result.stderr)
	}
}

func TestCmdResizeUpdatesStoppedInstance(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{Hostname: "srv", VCPUCount: 1, MemoryMiB: 1024}
	prov, err := provision.New(cfg, logger, st)
	if err != nil {
		t.Fatalf("provision.New(): %v", err)
	}
	app := &App{cfg: cfg, log: logger, store: st, provisioner: prov}

	inst := serviceTestInstance("alpha", model.StateStopped, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	result, err := app.cmdResize(ctx, model.Actor{UserLogin: "alice@example.com"}, []string{"resize", inst.Name, "--cpus", "4", "--ram", "6G"}, outputFormatText)
	if err != nil {
		t.Fatalf("cmdResize(): %v", err)
	}
	if result.exitCode != 0 {
		t.Fatalf("cmdResize() exitCode = %d, want 0", result.exitCode)
	}
	for _, want := range []string{"resized: alpha\n", "state: stopped\n", "vcpus: 4\n", "memory: 6144 MiB\n"} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("cmdResize() stdout missing %q\nfull output:\n%s", want, result.stdout)
		}
	}

	updated, err := st.GetInstance(ctx, inst.Name)
	if err != nil {
		t.Fatalf("GetInstance(): %v", err)
	}
	if updated.VCPUCount != 4 || updated.MemoryMiB != 6144 || updated.RootFSSizeBytes != inst.RootFSSizeBytes {
		t.Fatalf("updated instance = %#v", updated)
	}
	if !updated.UpdatedAt.After(inst.UpdatedAt) {
		t.Fatalf("updated timestamp did not advance: before=%s after=%s", inst.UpdatedAt, updated.UpdatedAt)
	}
}

func TestCmdResizeDeniesOtherUsers(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{Hostname: "srv", VCPUCount: 1, MemoryMiB: 1024}
	prov, err := provision.New(cfg, logger, st)
	if err != nil {
		t.Fatalf("provision.New(): %v", err)
	}
	app := &App{cfg: cfg, log: logger, store: st, provisioner: prov}

	inst := serviceTestInstance("alpha", model.StateStopped, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	result, err := app.cmdResize(ctx, model.Actor{UserLogin: "bob@example.com"}, []string{"resize", inst.Name, "--cpus", "4"}, outputFormatText)
	if err == nil {
		t.Fatalf("cmdResize() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), `instance "alpha" does not exist`) {
		t.Fatalf("cmdResize() error = %q", err.Error())
	}
	if !strings.Contains(result.stderr, `resize alpha: instance "alpha" does not exist`) {
		t.Fatalf("cmdResize() stderr = %q", result.stderr)
	}
}

func TestCmdLogsReturnsRecentOutput(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	app := &App{
		cfg:   config.Config{Hostname: "srv"},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}

	inst := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	baseDir := t.TempDir()
	inst.SerialLogPath = filepath.Join(baseDir, "serial.log")
	inst.LogPath = filepath.Join(baseDir, "firecracker.log")
	if err := os.WriteFile(inst.SerialLogPath, []byte("serial-1\nserial-2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(serial): %v", err)
	}
	if err := os.WriteFile(inst.LogPath, []byte("fc-1\nfc-2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(firecracker): %v", err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	result, err := app.cmdLogsRequest(ctx, model.Actor{UserLogin: "alice@example.com"}, logsRequest{name: inst.Name, target: logTargetAll})
	if err != nil {
		t.Fatalf("cmdLogsRequest(): %v", err)
	}
	for _, want := range []string{"serial-log: " + inst.SerialLogPath + "\n", "serial-1\n", "serial-2\n", "firecracker-log: " + inst.LogPath + "\n", "fc-1\n", "fc-2\n"} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("cmdLogsRequest() stdout missing %q\nfull output:\n%s", want, result.stdout)
		}
	}
	serialIndex := strings.Index(result.stdout, "serial-log: ")
	firecrackerIndex := strings.Index(result.stdout, "firecracker-log: ")
	if serialIndex < 0 || firecrackerIndex < 0 || serialIndex >= firecrackerIndex {
		t.Fatalf("cmdLogsRequest() stdout did not keep serial before firecracker:\n%s", result.stdout)
	}
}

func TestCmdLogsCanSelectSingleSurface(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	app := &App{
		cfg:   config.Config{Hostname: "srv"},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}

	inst := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	baseDir := t.TempDir()
	inst.SerialLogPath = filepath.Join(baseDir, "serial.log")
	inst.LogPath = filepath.Join(baseDir, "firecracker.log")
	if err := os.WriteFile(inst.LogPath, []byte("fc-only\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(firecracker): %v", err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	result, err := app.cmdLogsRequest(ctx, model.Actor{UserLogin: "alice@example.com"}, logsRequest{name: inst.Name, target: logTargetFirecracker})
	if err != nil {
		t.Fatalf("cmdLogsRequest(): %v", err)
	}
	if strings.Contains(result.stdout, "serial-log: ") {
		t.Fatalf("cmdLogsRequest() unexpectedly included serial output:\n%s", result.stdout)
	}
	if !strings.Contains(result.stdout, "firecracker-log: "+inst.LogPath+"\n") {
		t.Fatalf("cmdLogsRequest() stdout = %q", result.stdout)
	}
}

func TestCmdLogsSanitizesSerialOutputOnly(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	app := &App{
		cfg:   config.Config{Hostname: "srv"},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}

	inst := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	baseDir := t.TempDir()
	inst.SerialLogPath = filepath.Join(baseDir, "serial.log")
	inst.LogPath = filepath.Join(baseDir, "firecracker.log")
	serialPayload := []byte("prefix\x1b[43;1Rsuffix\nname\x1b]0;title\adone\nutf8: \xc4\x9b\nxy\x01\tz\r\n")
	if err := os.WriteFile(inst.SerialLogPath, serialPayload, 0o644); err != nil {
		t.Fatalf("WriteFile(serial): %v", err)
	}
	firecrackerPayload := []byte("fc\x1b[43;1Rraw\n")
	if err := os.WriteFile(inst.LogPath, firecrackerPayload, 0o644); err != nil {
		t.Fatalf("WriteFile(firecracker): %v", err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	serialResult, err := app.cmdLogsRequest(ctx, model.Actor{UserLogin: "alice@example.com"}, logsRequest{name: inst.Name, target: logTargetSerial})
	if err != nil {
		t.Fatalf("cmdLogsRequest(serial): %v", err)
	}
	utf8Line := "utf8: " + string([]byte{0xc4, 0x9b}) + "\n"
	for _, want := range []string{"prefixsuffix\n", "namedone\n", utf8Line, "xy\tz\r\n"} {
		if !strings.Contains(serialResult.stdout, want) {
			t.Fatalf("cmdLogsRequest(serial) stdout missing %q\nfull output:\n%s", want, serialResult.stdout)
		}
	}
	if strings.Contains(serialResult.stdout, "\x1b[") || strings.Contains(serialResult.stdout, "\x1b]") {
		t.Fatalf("cmdLogsRequest(serial) leaked escape sequences:\n%s", serialResult.stdout)
	}

	firecrackerResult, err := app.cmdLogsRequest(ctx, model.Actor{UserLogin: "alice@example.com"}, logsRequest{name: inst.Name, target: logTargetFirecracker})
	if err != nil {
		t.Fatalf("cmdLogsRequest(firecracker): %v", err)
	}
	if !strings.Contains(firecrackerResult.stdout, string(firecrackerPayload)) {
		t.Fatalf("cmdLogsRequest(firecracker) stdout = %q, want raw payload %q", firecrackerResult.stdout, string(firecrackerPayload))
	}
}

func TestCmdLogsPreservesTrailingPartialLine(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	app := &App{
		cfg:   config.Config{Hostname: "srv"},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store: st,
	}

	inst := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	inst.SerialLogPath = filepath.Join(t.TempDir(), "serial.log")
	if err := os.WriteFile(inst.SerialLogPath, []byte("login: "), 0o644); err != nil {
		t.Fatalf("WriteFile(serial): %v", err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	result, err := app.cmdLogsRequest(ctx, model.Actor{UserLogin: "alice@example.com"}, logsRequest{name: inst.Name, target: logTargetSerial})
	if err != nil {
		t.Fatalf("cmdLogsRequest(serial): %v", err)
	}
	if !strings.HasSuffix(result.stdout, "login: ") {
		t.Fatalf("cmdLogsRequest(serial) should preserve trailing partial line:\n%s", result.stdout)
	}
	if strings.Contains(result.stdout, "login: \n") {
		t.Fatalf("cmdLogsRequest(serial) injected a newline into trailing partial line:\n%s", result.stdout)
	}
}

func TestReadLastLinesSplitsLongChunkWithoutError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serial.log")
	payload := bytes.Repeat([]byte("a"), maxLogChunkBytes+17)
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("WriteFile(serial): %v", err)
	}

	lines, offset, exists, err := readLastLines(path, 4)
	if err != nil {
		t.Fatalf("readLastLines(): %v", err)
	}
	if !exists {
		t.Fatalf("readLastLines() exists = false, want true")
	}
	if offset != int64(len(payload)) {
		t.Fatalf("readLastLines() offset = %d, want %d", offset, len(payload))
	}
	if got := strings.Join(lines, ""); got != string(payload) {
		t.Fatalf("readLastLines() lost data for long chunk: got %d bytes, want %d", len(got), len(payload))
	}
}

func TestStreamLogOutputFollowsSingleLog(t *testing.T) {
	oldPollInterval := logFollowPollInterval
	oldKeepAliveInterval := logFollowKeepAliveInterval
	logFollowPollInterval = 10 * time.Millisecond
	logFollowKeepAliveInterval = time.Hour
	t.Cleanup(func() {
		logFollowPollInterval = oldPollInterval
		logFollowKeepAliveInterval = oldKeepAliveInterval
	})

	inst := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	inst.SerialLogPath = filepath.Join(t.TempDir(), "serial.log")
	if err := os.WriteFile(inst.SerialLogPath, []byte("serial-1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(serial): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	output := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- streamLogOutput(ctx, output, inst, logTargetSerial, nil)
	}()

	waitForOutput(t, output, "serial-1\n")
	file, err := os.OpenFile(inst.SerialLogPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("OpenFile(serial): %v", err)
	}
	if _, err := io.WriteString(file, "serial-2\n"); err != nil {
		_ = file.Close()
		t.Fatalf("WriteString(serial): %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close(serial): %v", err)
	}
	waitForOutput(t, output, "serial-2\n")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("streamLogOutput(): %v", err)
	}
	if strings.Contains(output.String(), "firecracker-log:") {
		t.Fatalf("streamLogOutput() unexpectedly included firecracker output:\n%s", output.String())
	}
}

func TestStreamLogOutputReturnsKeepAliveError(t *testing.T) {
	oldPollInterval := logFollowPollInterval
	oldKeepAliveInterval := logFollowKeepAliveInterval
	logFollowPollInterval = time.Hour
	logFollowKeepAliveInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		logFollowPollInterval = oldPollInterval
		logFollowKeepAliveInterval = oldKeepAliveInterval
	})

	inst := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	inst.SerialLogPath = filepath.Join(t.TempDir(), "serial.log")
	if err := os.WriteFile(inst.SerialLogPath, []byte("serial-1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(serial): %v", err)
	}

	keepAliveErr := errors.New("keepalive failed")
	output := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- streamLogOutput(context.Background(), output, inst, logTargetSerial, func() error {
			return keepAliveErr
		})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, keepAliveErr) {
			t.Fatalf("streamLogOutput() error = %v, want %v", err, keepAliveErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("streamLogOutput() did not return keepalive error")
	}
}

func TestStreamLogOutputSanitizesSplitEscapeSequence(t *testing.T) {
	oldPollInterval := logFollowPollInterval
	oldKeepAliveInterval := logFollowKeepAliveInterval
	logFollowPollInterval = 10 * time.Millisecond
	logFollowKeepAliveInterval = time.Hour
	t.Cleanup(func() {
		logFollowPollInterval = oldPollInterval
		logFollowKeepAliveInterval = oldKeepAliveInterval
	})

	inst := serviceTestInstance("alpha", model.StateReady, time.Date(2026, time.March, 29, 12, 0, 0, 0, time.UTC))
	inst.SerialLogPath = filepath.Join(t.TempDir(), "serial.log")
	if err := os.WriteFile(inst.SerialLogPath, []byte("prefix\x1b[6"), 0o644); err != nil {
		t.Fatalf("WriteFile(serial): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	output := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- streamLogOutput(ctx, output, inst, logTargetSerial, nil)
	}()

	waitForOutput(t, output, "serial-log: ")
	file, err := os.OpenFile(inst.SerialLogPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("OpenFile(serial): %v", err)
	}
	if _, err := io.WriteString(file, "n suffix\n"); err != nil {
		_ = file.Close()
		t.Fatalf("WriteString(serial): %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close(serial): %v", err)
	}
	waitForOutput(t, output, "prefix suffix\n")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("streamLogOutput(): %v", err)
	}
	if strings.Contains(output.String(), "\x1b[") || strings.Contains(output.String(), "[6n") {
		t.Fatalf("streamLogOutput() leaked split escape sequence:\n%s", output.String())
	}
}

func TestParseNewArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantName string
		wantOpts provision.CreateOptions
		wantErr  string
	}{
		{
			name:     "parses name before flags",
			args:     []string{"new", "demo", "--cpus", "2", "--ram", "4G", "--rootfs-size", "12G"},
			wantName: "demo",
			wantOpts: provision.CreateOptions{VCPUCount: 2, MemoryMiB: 4096, RootFSSizeBytes: 12 << 30},
		},
		{
			name:     "parses flags before name and plain mib values",
			args:     []string{"new", "--ram=1536", "--cpus=4", "demo"},
			wantName: "demo",
			wantOpts: provision.CreateOptions{VCPUCount: 4, MemoryMiB: 1536},
		},
		{
			name:    "rejects unknown options",
			args:    []string{"new", "demo", "--wat", "1"},
			wantErr: `unknown option "--wat"`,
		},
		{
			name:    "requires a name",
			args:    []string{"new", "--cpus", "2"},
			wantErr: newUsage(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotOpts, err := parseNewArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseNewArgs() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseNewArgs() error = %v", err)
			}
			if gotName != tt.wantName {
				t.Fatalf("parseNewArgs() name = %q, want %q", gotName, tt.wantName)
			}
			if !reflect.DeepEqual(gotOpts, tt.wantOpts) {
				t.Fatalf("parseNewArgs() opts = %#v, want %#v", gotOpts, tt.wantOpts)
			}
		})
	}
}

func TestParseResizeArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantName string
		wantOpts provision.CreateOptions
		wantErr  string
	}{
		{
			name:     "parses name and one flag",
			args:     []string{"resize", "demo", "--rootfs-size", "12G"},
			wantName: "demo",
			wantOpts: provision.CreateOptions{RootFSSizeBytes: 12 << 30},
		},
		{
			name:    "requires at least one option",
			args:    []string{"resize", "demo"},
			wantErr: "resize requires at least one of --cpus, --ram, or --rootfs-size",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotOpts, err := parseResizeArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseResizeArgs() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseResizeArgs() error = %v", err)
			}
			if gotName != tt.wantName {
				t.Fatalf("parseResizeArgs() name = %q, want %q", gotName, tt.wantName)
			}
			if !reflect.DeepEqual(gotOpts, tt.wantOpts) {
				t.Fatalf("parseResizeArgs() opts = %#v, want %#v", gotOpts, tt.wantOpts)
			}
		})
	}
}

func TestParseBackupArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantAction string
		wantName   string
		wantErr    string
	}{
		{name: "create", args: []string{"backup", "create", "demo"}, wantAction: "create", wantName: "demo"},
		{name: "list", args: []string{"backup", "list", "demo"}, wantAction: "list", wantName: "demo"},
		{name: "rejects unknown action", args: []string{"backup", "prune", "demo"}, wantErr: `unknown backup action "prune"`},
		{name: "requires name", args: []string{"backup", "create"}, wantErr: backupUsage()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotAction, gotName, err := parseBackupArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseBackupArgs() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBackupArgs() error = %v", err)
			}
			if gotAction != tt.wantAction || gotName != tt.wantName {
				t.Fatalf("parseBackupArgs() = (%q, %q), want (%q, %q)", gotAction, gotName, tt.wantAction, tt.wantName)
			}
		})
	}
}

func TestParseRestoreArgs(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantName     string
		wantBackupID string
		wantErr      string
	}{
		{name: "valid", args: []string{"restore", "demo", "20260331T120000Z"}, wantName: "demo", wantBackupID: "20260331T120000Z"},
		{name: "requires backup id", args: []string{"restore", "demo"}, wantErr: restoreUsage()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotBackupID, err := parseRestoreArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseRestoreArgs() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRestoreArgs() error = %v", err)
			}
			if gotName != tt.wantName || gotBackupID != tt.wantBackupID {
				t.Fatalf("parseRestoreArgs() = (%q, %q), want (%q, %q)", gotName, gotBackupID, tt.wantName, tt.wantBackupID)
			}
		})
	}
}

func TestParseExportArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantName string
		wantErr  string
	}{
		{name: "valid", args: []string{"export", "demo"}, wantName: "demo"},
		{name: "requires name", args: []string{"export"}, wantErr: exportUsage()},
		{name: "rejects extra args", args: []string{"export", "demo", "extra"}, wantErr: exportUsage()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, err := parseExportArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseExportArgs() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseExportArgs() error = %v", err)
			}
			if gotName != tt.wantName {
				t.Fatalf("parseExportArgs() name = %q, want %q", gotName, tt.wantName)
			}
		})
	}
}

func TestParseImportArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "valid", args: []string{"import"}},
		{name: "rejects extra args", args: []string{"import", "demo", "extra"}, wantErr: importUsage()},
		{name: "rejects rename arg", args: []string{"import", "demo-moved"}, wantErr: importUsage()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := parseImportArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseImportArgs() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseImportArgs() error = %v", err)
			}
		})
	}
}

func TestFormatImportProgress(t *testing.T) {
	got := formatImportProgress(provision.ImportProgress{
		Name:           "rootfs.img",
		CompletedBytes: 6 * 1024 * 1024,
		TotalBytes:     18 * 1024 * 1024,
	})
	if got != "import rootfs.img 6.0 MiB / 18.0 MiB (33%)" {
		t.Fatalf("formatImportProgress() = %q", got)
	}
}

func TestParseLogsArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    logsRequest
		wantErr string
	}{
		{name: "default to both logs", args: []string{"logs", "demo"}, want: logsRequest{name: "demo", target: logTargetAll}},
		{name: "select serial", args: []string{"logs", "demo", "serial"}, want: logsRequest{name: "demo", target: logTargetSerial}},
		{name: "follow before name", args: []string{"logs", "-f", "demo", "serial"}, want: logsRequest{name: "demo", target: logTargetSerial, follow: true}},
		{name: "follow after target", args: []string{"logs", "demo", "firecracker", "--follow"}, want: logsRequest{name: "demo", target: logTargetFirecracker, follow: true}},
		{name: "reject unknown target", args: []string{"logs", "demo", "kernel"}, wantErr: `unexpected argument "kernel"`},
		{name: "reject follow without explicit target", args: []string{"logs", "-f", "demo"}, wantErr: "follow requires an explicit log target"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLogsArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseLogsArgs() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLogsArgs() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseLogsArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func waitForOutput(t *testing.T, output *lockedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in output:\n%s", want, output.String())
}

func newServiceTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "app.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
	})
	return st
}

func loadServiceTestConfig(t *testing.T, env map[string]string) config.Config {
	t.Helper()
	oldArgs := os.Args
	oldCommandLine := flag.CommandLine

	fs := flag.NewFlagSet("srv.test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flag.CommandLine = fs
	os.Args = []string{"srv.test"}
	t.Cleanup(func() {
		flag.CommandLine = oldCommandLine
		os.Args = oldArgs
	})

	dataDir := t.TempDir()
	t.Setenv("SRV_DATA_DIR", dataDir)
	for key, value := range env {
		t.Setenv(key, value)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load(): %v", err)
	}
	return cfg
}

func serviceTestInstance(name, state string, createdAt time.Time) model.Instance {
	baseDir := filepath.Join("/tmp", name)
	return model.Instance{
		ID:              name + "-id",
		Name:            name,
		State:           state,
		CreatedAt:       createdAt,
		UpdatedAt:       createdAt.Add(30 * time.Second),
		CreatedByUser:   "alice@example.com",
		CreatedByNode:   "laptop",
		VCPUCount:       2,
		MemoryMiB:       2048,
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

func writeSparseServiceTestFile(t *testing.T, path string, size int64) int64 {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatalf("Truncate(%q): %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Stat(%q) sys = %T, want *syscall.Stat_t", path, info.Sys())
	}
	return stat.Blocks * 512
}
