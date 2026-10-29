package service

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"srv/internal/model"
	"srv/internal/provision"
	"srv/internal/store"
)

func TestMigrationFenceSurvivesReopenAndRejectsRestart(t *testing.T) {
	ctx := context.Background()
	cfg := loadServiceTestConfig(t, nil)
	cfg.VMRunnerSocketPath = serviceTestHotColdDiskSocket(t)
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	inst := serviceTestInstance("cold", model.StateStopped, time.Now().UTC())
	inst.RootFSPath = filepath.Join(cfg.InstancesDir(), inst.Name, "rootfs.img")
	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.RootFSPath, []byte("rootfs"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	actor := model.Actor{UserLogin: inst.CreatedByUser}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	makeApp := func() *App {
		p, err := provision.New(cfg, logger, st)
		if err != nil {
			t.Fatal(err)
		}
		return &App{cfg: cfg, store: st, provisioner: p, log: logger}
	}
	a := makeApp()
	if _, err := a.cmdIntegration(ctx, actor, []string{"integration", "add", "http", "api", "--target", "https://example.com"}, outputFormatJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := a.cmdIntegration(ctx, actor, []string{"integration", "enable", inst.Name, "api"}, outputFormatJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := a.cmdMigration(ctx, actor, []string{"migration", "prepare", inst.Name, "destination", "token"}, outputFormatJSON); err == nil {
		t.Fatal("migration must not drop enabled integrations")
	}
	if _, err := a.cmdIntegration(ctx, actor, []string{"integration", "disable", inst.Name, "api"}, outputFormatJSON); err != nil {
		t.Fatal(err)
	}
	r, err := a.cmdMigration(ctx, actor, []string{"migration", "prepare", inst.Name, "destination", "token"}, outputFormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	var m model.Migration
	if err := json.Unmarshal([]byte(r.stdout), &m); err != nil || m.Phase != "prepared" || m.WasRunning {
		t.Fatalf("prepare: %s, %v", r.stdout, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a = makeApp()
	for _, phase := range []string{"prepared", "completed"} {
		if phase == "completed" {
			if _, err := a.cmdMigration(ctx, actor, []string{"migration", "complete", inst.Name, "token"}, outputFormatJSON); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := a.cmdIntegration(ctx, actor, []string{"integration", "enable", inst.Name, "api"}, outputFormatJSON); err == nil {
			t.Fatal("integration added after source fenced")
		}
		if bindings, err := st.ListInstanceIntegrations(ctx, inst.ID); err != nil || len(bindings) != 0 {
			t.Fatalf("fenced source bindings changed: %+v %v", bindings, err)
		}
		if _, err := a.provisioner.Start(ctx, inst.Name); err == nil || !strings.Contains(err.Error(), "fenced") {
			t.Fatalf("%s start: %v", phase, err)
		}
		if _, err := a.cmdRestart(ctx, actor, []string{"restart", inst.Name}, outputFormatJSON); err == nil || !strings.Contains(err.Error(), "fenced") {
			t.Fatalf("%s restart: %v", phase, err)
		}
		if err := a.provisioner.RestoreInstance(ctx, inst.Name); err == nil || !strings.Contains(err.Error(), "fenced") {
			t.Fatalf("%s recovery: %v", phase, err)
		}
		if _, err := a.provisioner.Resize(ctx, inst.Name, provision.CreateOptions{}); err == nil || !strings.Contains(err.Error(), "fenced") {
			t.Fatalf("%s resize: %v", phase, err)
		}
		if _, err := a.provisioner.CheckRootFS(ctx, inst.Name); err == nil || !strings.Contains(err.Error(), "fenced") {
			t.Fatalf("%s fsck: %v", phase, err)
		}
		if _, _, err := a.provisioner.RestoreBackup(ctx, inst.Name, "anything"); err == nil || !strings.Contains(err.Error(), "fenced") {
			t.Fatalf("%s restore: %v", phase, err)
		}
		if _, err := a.cmdMigration(ctx, actor, []string{"migration", "abort", inst.Name, "token"}, outputFormatJSON); err == nil {
			t.Fatal("unconfirmed abort succeeded")
		}
		if _, err := a.cmdMigration(ctx, actor, []string{"migration", "abort", inst.Name, "token", "--confirm-destination-destroyed"}, outputFormatJSON); err == nil {
			t.Fatal("non-admin abort succeeded")
		}
	}
	if _, err := a.provisioner.FinishMigration(ctx, inst.Name, "token", true); err == nil {
		t.Fatal("completed source recovery succeeded")
	}
	stored, err := st.GetInstance(ctx, inst.Name)
	if err != nil || stored.State != model.StateStopped || stored.FirecrackerPID != 0 {
		t.Fatalf("runtime changed: %+v, %v", stored, err)
	}
}

func TestIncompleteImportCannotStartAfterReopen(t *testing.T) {
	ctx := context.Background()
	cfg := loadServiceTestConfig(t, nil)
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	inst := serviceTestInstance("partial", model.StateProvisioning, time.Now().UTC())
	inst.TailscaleName = "partial"
	if err := st.PutImportReceipt(ctx, model.ImportReceipt{InstanceID: inst.ID, SourceInstanceID: inst.ID, Phase: "importing"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p, err := provision.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Start(ctx, inst.Name); err == nil || !strings.Contains(err.Error(), "incomplete import") {
		t.Fatalf("start: %v", err)
	}
	if err := p.RestoreInstance(ctx, inst.Name); err == nil || !strings.Contains(err.Error(), "incomplete import") {
		t.Fatalf("recovery: %v", err)
	}
}
