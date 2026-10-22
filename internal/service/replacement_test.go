package service

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"srv/internal/model"
)

func TestReplacementRequestsRejectForeignOwnershipWithoutChanges(t *testing.T) {
	for _, command := range []string{"new", "import", "spoofed import"} {
		t.Run(command, func(t *testing.T) {
			ctx := context.Background()
			st := newServiceTestStore(t)
			inst := serviceTestInstance("private", model.StateFailed, time.Now().UTC())
			inst.RootFSPath = filepath.Join(t.TempDir(), "rootfs.img")
			if err := os.WriteFile(inst.RootFSPath, []byte("private disk"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateInstance(ctx, inst); err != nil {
				t.Fatal(err)
			}
			migration := model.Migration{InstanceID: inst.ID, Token: "private-token", Target: "destination", Phase: "prepared"}
			if err := st.PutMigration(ctx, migration); err != nil {
				t.Fatal(err)
			}
			receipt := model.ImportReceipt{InstanceID: inst.ID, Token: "import-token", Phase: "completed"}
			if err := st.PutImportReceipt(ctx, receipt); err != nil {
				t.Fatal(err)
			}
			cfg := loadServiceTestConfig(t, nil)
			cfg.AdminUsers = []string{"admin@example.com"}
			cfg.AllowedUsers = []string{"bob@example.com"}
			// No provisioner is needed: these real command paths must reject
			// the request before reaching any destructive provisioning work.
			app := &App{cfg: cfg, store: st}
			actor := model.Actor{UserLogin: "bob@example.com"}
			if command == "new" {
				result, err := app.cmdNew(ctx, actor, []string{"new", inst.Name}, outputFormatJSON)
				if err == nil || result.exitCode != 1 || !strings.Contains(result.stderr, "owner or an admin") {
					t.Fatalf("new: %+v, %v", result, err)
				}
			} else {
				name, owner, want := inst.Name, actor.UserLogin, "owner or an admin"
				if command == "spoofed import" {
					name, owner, want = "fresh", inst.CreatedByUser, "artifact owner"
				}
				manifest, err := json.Marshal(map[string]any{
					"version": 2, "exported_at": inst.CreatedAt,
					"instance": map[string]any{"name": name, "created_at": inst.CreatedAt, "created_by_user": owner},
					"files":    map[string]string{"rootfs": "rootfs.img"},
				})
				if err != nil {
					t.Fatal(err)
				}
				var artifact bytes.Buffer
				tw := tar.NewWriter(&artifact)
				if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(manifest))}); err != nil {
					t.Fatal(err)
				}
				if _, err := tw.Write(manifest); err != nil {
					t.Fatal(err)
				}
				if err := tw.Close(); err != nil {
					t.Fatal(err)
				}
				encoder, err := zstd.NewWriter(nil)
				if err != nil {
					t.Fatal(err)
				}
				compressed := encoder.EncodeAll(artifact.Bytes(), nil)
				encoder.Close()
				sess := newServiceTestSession(ctx, string(compressed), false, []string{"import"})
				code, err := app.handleImportSession(sess, actor, []string{"import"})
				if err == nil || code != 1 || !strings.Contains(sess.stderr.String(), want) {
					t.Fatalf("import: code=%d err=%v stderr=%s", code, err, sess.stderr.String())
				}
			}
			got, err := st.GetInstance(ctx, inst.Name)
			if err != nil || !reflect.DeepEqual(got, inst) {
				t.Fatalf("record changed: %+v, %v", got, err)
			}
			if got, err := st.GetMigration(ctx, inst.ID); err != nil || got != migration {
				t.Fatalf("migration journal changed: %+v, %v", got, err)
			}
			if got, err := st.GetImportReceipt(ctx, inst.ID); err != nil || got != receipt {
				t.Fatalf("import journal changed: %+v, %v", got, err)
			}
			disk, err := os.ReadFile(inst.RootFSPath)
			if err != nil || string(disk) != "private disk" {
				t.Fatalf("disk changed: %q, %v", disk, err)
			}
			instances, err := st.ListInstances(ctx, true)
			if err != nil || len(instances) != 1 {
				t.Fatalf("unexpected records: %+v, %v", instances, err)
			}
		})
	}
}

func TestAuthorizeReplacementOwnerAdminAndNameReuse(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	cfg := loadServiceTestConfig(t, nil)
	cfg.AdminUsers = []string{"admin@example.com"}
	app := &App{cfg: cfg, store: st}
	for _, state := range []string{model.StateFailed, model.StateStopped, model.StateDeleted} {
		inst := serviceTestInstance(state, state, time.Now().UTC())
		if err := st.CreateInstance(ctx, inst); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name, user, artifactOwner string
		allowed                   bool
	}{
		{model.StateFailed, "ALICE@example.com", "", true},
		{model.StateFailed, "alice@example.com", "ALICE@example.com", true},
		{model.StateFailed, "ADMIN@example.com", "other@example.com", true},
		{model.StateFailed, "bob@example.com", "", false},
		{model.StateStopped, "bob@example.com", "", false},
		{model.StateDeleted, "bob@example.com", "", true},
		{"unused", "bob@example.com", "", true},
		{"unused", "bob@example.com", "alice@example.com", false},
		{model.StateDeleted, "bob@example.com", "alice@example.com", false},
	} {
		t.Run(tt.name+"/"+tt.user+"/"+tt.artifactOwner, func(t *testing.T) {
			unlock := app.lockInstance(tt.name)
			defer unlock()
			err := app.authorizeReplacement(ctx, model.Actor{UserLogin: tt.user}, tt.name, tt.artifactOwner)
			if (err == nil) != tt.allowed {
				t.Fatalf("allowed=%v, err=%v", tt.allowed, err)
			}
		})
	}
}
