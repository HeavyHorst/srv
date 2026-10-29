package provision

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"srv/internal/host"
	"srv/internal/model"
	"srv/internal/nethelper"
	"srv/internal/storage"
	"srv/internal/store"
	"srv/internal/vmrunner"
	"tailscale.com/client/tailscale/v2"
)

type cleanupNetwork struct {
	noopNetworkHelper
	verify func() error
}

func (n cleanupNetwork) FinalizeInstanceNetwork(context.Context, nethelper.FinalizeRequest) error {
	return n.verify()
}

type cleanupRunner struct {
	noopVMRunner
	verify func() error
}

func (r cleanupRunner) VerifyInstanceAbsent(context.Context, vmrunner.StartResultRequest) error {
	return r.verify()
}

func (r cleanupRunner) ColdDisk(_ context.Context, req vmrunner.ColdRequest) (vmrunner.ColdStatus, error) {
	if req.Action == "status" {
		return vmrunner.ColdStatus{Phase: "hot"}, nil
	}
	return vmrunner.ColdStatus{Phase: "hot"}, r.verify()
}

func TestAuthoritativeOrbCleanup(t *testing.T) {
	for _, scenario := range []string{"success", "template", "snapshot-template", "legacy-vm", "node", "key", "import", "backup", "runtime", "network", "disk"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			cfg := loadProvisionTestConfig(t, nil)
			cfg.OutboundInterface = "fixture0"
			st := newProvisionTestStore(t, cfg)
			i := provisionTestInstance(cfg, "proof", model.StateStopped, time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC))
			i.Kind, i.Image, i.TailnetNodeID = model.KindOrb, "amp-orb", ""
			if scenario == "legacy-vm" {
				i.Kind = model.KindVM
			}
			if scenario == "node" {
				i.TailnetNodeID = "unexpected-node"
			}
			if err := os.MkdirAll(filepath.Dir(i.RootFSPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(i.RootFSPath, []byte("retained data"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateInstance(ctx, i); err != nil {
				t.Fatal(err)
			}
			if scenario == "key" {
				if err := st.RecordEnrollmentKey(ctx, i.ID, "unexpected-key"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "import" {
				if err := st.PutImportReceipt(ctx, model.ImportReceipt{InstanceID: i.ID, Phase: "imported"}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "backup" {
				if err := os.MkdirAll(filepath.Join(cfg.BackupsDir(), i.Name, "retained"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.AdmitLifecycleRequest(ctx, store.LifecycleRequest{ID: "request", Actor: "owner", Command: "delete-proof", Name: i.Name, Arguments: "bound"}); err != nil {
				t.Fatal(err)
			}
			p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), vmRunner: cleanupRunner{verify: func() error {
				if scenario == "runtime" {
					return errors.New("runtime remains")
				}
				return nil
			}}, networkHelper: cleanupNetwork{verify: func() error {
				if scenario == "network" {
					return errors.New("network remains")
				}
				return nil
			}}}
			var template string
			if scenario == "template" || scenario == "snapshot-template" {
				template = filepath.Join(cfg.DataDirAbs(), "templates", "owner", "retained")
				if scenario == "snapshot-template" {
					template = filepath.Join(cfg.SnapshotsDir(), "snapshot", "templates", "owner", "retained")
				}
				if err := os.MkdirAll(template, 0700); err != nil {
					t.Fatal(err)
				}
				manifest, _ := json.Marshal(templateManifest{Version: 1, ID: "retained", Owner: i.CreatedByUser, SourceID: i.ID, Image: i.Image, Kind: i.Kind})
				if err := os.WriteFile(filepath.Join(template, "manifest.json"), manifest, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(template, "rootfs.img"), []byte("project copy"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "disk" {
				old := removeTree
				removeTree = func(context.Context, string) error { return errors.New("busy") }
				defer func() { removeTree = old }()
			}
			proof, err := p.DeleteWithProof(ctx, i, "request", "owner", "backend-one", func(context.Context, model.Instance) error { return nil })
			if scenario == "success" || scenario == "template" || scenario == "snapshot-template" || scenario == "backup" || scenario == "legacy-vm" || scenario == "import" {
				if err != nil || proof.Version != 5 || !proof.GatewaysAbsent || proof.Kind != i.Kind || !proof.StorageAbsent || proof.BackupsAbsent || proof.SnapshotsAbsent || proof.RetainedTemplates != nil {
					t.Fatal(proof, err)
				}
				if _, err := os.Stat(i.RootFSPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(cfg.BackupsDir(), i.Name, "retained")); scenario == "backup" && err != nil {
					t.Fatal(err)
				}
				if template != "" {
					if _, err := os.Stat(filepath.Join(template, "rootfs.img")); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				if err == nil {
					t.Fatal("partial proof succeeded")
				}
				if _, err := st.CleanupReceipt(ctx, "request", "owner"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatal(err)
				}
				if _, err := os.Stat(i.RootFSPath); err != nil {
					t.Fatal("disk lost", err)
				}
				if scenario == "runtime" || scenario == "network" || scenario == "disk" {
					if err := st.InterruptLifecycleRequests(ctx); err != nil {
						t.Fatal(err)
					}
					for _, operation := range []func(context.Context, string) (model.Instance, error){p.ReconcileInstance, p.Delete, p.Start, p.Stop} {
						if _, err := operation(ctx, i.Name); err == nil {
							t.Fatal("cleanup fence bypassed")
						}
					}
				}
			}
		})
	}
}

func TestOrbCleanupResumesPartialStorageRemoval(t *testing.T) {
	for _, failure := range []string{"partial-disk", "after-disk"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			cfg := loadProvisionTestConfig(t, nil)
			cfg.OutboundInterface = "fixture0"
			st := newProvisionTestStore(t, cfg)
			inst := provisionTestInstance(cfg, "retry", model.StateStopped, time.Now().UTC())
			inst.Kind, inst.Image = model.KindOrb, "amp-orb"
			backup := filepath.Join(cfg.BackupsDir(), inst.Name, "partial")
			preserved := []string{
				filepath.Join(backup, "manifest.json"),
				filepath.Join(backup, "disk"),
				filepath.Join(cfg.BackupsDir(), "neighbor", "keep"),
				filepath.Join(cfg.InstancesDir(), "neighbor", "disk", "rootfs.img"),
				filepath.Join(cfg.SnapshotsDir(), "shared", "instances", inst.Name, "copy"),
				filepath.Join(cfg.DataDirAbs(), "templates", "owner", "broken-manifest"),
			}
			for _, path := range append(append([]string{}, preserved...), inst.RootFSPath) {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("keep exact bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.CreateInstance(ctx, inst); err != nil {
				t.Fatal(err)
			}
			if _, err := st.AdmitLifecycleRequest(ctx, store.LifecycleRequest{ID: "request", Actor: "owner", Command: "delete-proof", Name: inst.Name, Arguments: "bound"}); err != nil {
				t.Fatal(err)
			}
			p := &Provisioner{cfg: cfg, store: st, vmRunner: cleanupRunner{verify: func() error { return nil }}, networkHelper: cleanupNetwork{verify: func() error { return nil }}}
			original := removeTree
			t.Cleanup(func() { removeTree = original })
			removeTree = func(ctx context.Context, path string) error {
				if failure == "partial-disk" && path == filepath.Join(cfg.InstancesDir(), inst.Name) {
					if err := os.Remove(inst.RootFSPath); err != nil {
						return err
					}
					return errors.New("disk removal interrupted")
				}
				if err := storage.RemoveTree(ctx, path); err != nil {
					return err
				}
				if failure == "after-disk" && path == filepath.Join(cfg.InstancesDir(), inst.Name) {
					return errors.New("crash before receipt")
				}
				return nil
			}
			closeGateways := func(context.Context, model.Instance) error { return nil }
			if _, err := p.DeleteWithProof(ctx, inst, "request", "owner", "backend", closeGateways); err == nil {
				t.Fatal("injected failure ignored")
			}
			if _, err := st.CleanupReceipt(ctx, "request", "owner"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("premature receipt", err)
			}
			if err := st.CheckCleanupFence(ctx, inst.ID); err == nil {
				t.Fatal("failure released fence")
			}
			if err := st.InterruptLifecycleRequests(ctx); err != nil {
				t.Fatal(err)
			}
			removeTree = original
			inst, err := st.GetInstance(ctx, inst.Name)
			if err != nil {
				t.Fatal(err)
			}
			proof, err := p.DeleteWithProof(ctx, inst, "request", "owner", "backend", closeGateways)
			if err != nil || proof.Version != 5 || proof.BackupsAbsent {
				t.Fatal(proof, err)
			}
			if err := absentPath(filepath.Join(cfg.InstancesDir(), inst.Name)); err != nil {
				t.Fatal(err)
			}
			for _, path := range preserved {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "keep exact bytes" {
					t.Fatalf("retained data %s: %q, %v", path, data, err)
				}
			}
			if err := st.CheckCleanupFence(ctx, inst.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOrbCleanupDoesNotDeadlockImport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := loadProvisionTestConfig(t, nil)
	cfg.OutboundInterface = "fixture0"
	st := newProvisionTestStore(t, cfg)
	inst := provisionTestInstance(cfg, "delete-during-import", model.StateStopped, time.Now().UTC())
	inst.Kind, inst.Image = model.KindOrb, "amp-orb"
	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.RootFSPath, []byte("discard"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdmitLifecycleRequest(ctx, store.LifecycleRequest{ID: "request", Actor: "owner", Command: "delete-proof", Name: inst.Name, Arguments: "bound"}); err != nil {
		t.Fatal(err)
	}
	importHeld, allowImport, deleteWaiting := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseImport := func() { once.Do(func() { close(allowImport) }) }
	defer releaseImport()
	acquire := storage.AcquirePayload
	var workers sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		releaseImport()
		workers.Wait()
		storage.AcquirePayload = acquire
	})
	var acquisitions atomic.Int32
	storage.AcquirePayload = func(ctx context.Context, dir string) (context.Context, func(), error) {
		if acquisitions.Add(1) == 2 {
			close(deleteWaiting)
		}
		return acquire(ctx, dir)
	}
	p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), vmRunner: cleanupRunner{verify: func() error { return nil }}, networkHelper: cleanupNetwork{verify: func() error { return nil }}}
	p.readFilesystemUsage = func(path string) (host.FilesystemUsage, error) {
		close(importHeld)
		select {
		case <-allowImport:
		case <-ctx.Done():
			return host.FilesystemUsage{}, ctx.Err()
		}
		return host.DefaultReadFilesystemUsage(path)
	}
	manifest := portableManifest{Version: portableManifestVersion, ExportedAt: time.Now().UTC(), Instance: portableInstance{Name: "neighbor-import", CreatedAt: time.Now().UTC(), RootFSSizeBytes: 6}, Files: backupFiles{RootFS: backupRootFSName}}
	stream := portableArtifactTestStream(t, manifest, map[string][]byte{backupRootFSName: []byte("import")})
	importDone, deleteDone := make(chan error, 1), make(chan error, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		_, _, err := p.ImportInstance(ctx, model.Actor{}, bytes.NewReader(stream), nil)
		importDone <- err
	}()
	select {
	case <-importHeld:
	case err := <-importDone:
		t.Fatal("import did not acquire payload", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		_, err := p.DeleteWithProof(ctx, inst, "request", "owner", "backend", func(context.Context, model.Instance) error { return nil })
		deleteDone <- err
	}()
	select {
	case <-deleteWaiting:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	releaseImport() // Import must still be able to acquire admissionMu.
	importErr, deleteErr := <-importDone, <-deleteDone
	if importErr != nil || deleteErr != nil {
		t.Fatalf("import: %v; deletion: %v", importErr, deleteErr)
	}
	if err := absentPath(inst.RootFSPath); err != nil {
		t.Fatal(err)
	}
	imported, err := st.GetInstance(ctx, manifest.Instance.Name)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(imported.RootFSPath); err != nil || string(data) != "import" {
		t.Fatal("neighbor import damaged", string(data), err)
	}
}

func TestDeleteResumesColdAndLocalCleanup(t *testing.T) {
	for _, kind := range []string{model.KindVM, model.KindOrb} {
		for _, failure := range []string{"cold", "network", "disk", "commit"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				cfg := loadProvisionTestConfig(t, nil)
				cfg.OutboundInterface = "fixture0"
				st := newProvisionTestStore(t, cfg)
				inst := provisionTestInstance(cfg, "retry-cold", model.StateFailed, time.Now().UTC())
				inst.Kind = kind // No start attempt and no local rootfs.
				if err := st.CreateInstance(ctx, inst); err != nil {
					t.Fatal(err)
				}
				marker := filepath.Join(cfg.InstancesDir(), inst.Name, "cold-disk.json")
				backup := filepath.Join(cfg.BackupsDir(), inst.Name, "backup", "rootfs.img")
				neighbor := filepath.Join(cfg.InstancesDir(), "neighbor", "disk", "rootfs.img")
				for _, path := range []string{marker, backup, neighbor} {
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("preserved"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				data, _ := json.Marshal(map[string]any{"instance_id": inst.ID, "phase": "cold", "remote": true})
				if err := os.WriteFile(marker, data, 0600); err != nil {
					t.Fatal(err)
				}
				fail, remote := true, true
				p := &Provisioner{cfg: cfg, store: st, vmRunner: coldTestRunner{call: func(q vmrunner.ColdRequest) (vmrunner.ColdStatus, error) {
					if q.Action != "delete" || q.InstanceID != inst.ID {
						t.Fatalf("unexpected disk operation: %+v", q)
					}
					// Model the runner's independent payload lock; deletion must not
					// hold it while waiting for this RPC (including on retry).
					_, release, err := storage.AcquirePayload(ctx, cfg.DataDirAbs())
					if err != nil {
						return vmrunner.ColdStatus{}, err
					}
					defer release()
					if fail && failure == "cold" {
						return vmrunner.ColdStatus{}, errors.New("remote unavailable")
					}
					remote = false
					if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
						return vmrunner.ColdStatus{}, err
					}
					return vmrunner.ColdStatus{Phase: "hot"}, nil
				}}, networkHelper: cleanupNetwork{verify: func() error {
					if fail && failure == "network" {
						return errors.New("network unavailable")
					}
					return nil
				}}}
				p.SetCleanup("backend", func(context.Context, model.Instance) error { return nil })
				original := removeTree
				t.Cleanup(func() { removeTree = original })
				removeTree = func(ctx context.Context, path string) error {
					if fail && failure == "disk" {
						return errors.New("disk busy")
					}
					return original(ctx, path)
				}
				db, err := sql.Open("sqlite", cfg.DatabasePath())
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if failure == "commit" {
					if _, err := db.Exec(`CREATE TRIGGER fail_cleanup BEFORE INSERT ON cleanup_receipts BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
						t.Fatal(err)
					}
				}
				pending, err := p.Delete(ctx, inst.Name)
				if err == nil || pending.State != model.StateDeleting {
					t.Fatalf("failure not retained: %+v, %v", pending, err)
				}
				request, err := st.CleanupRequest(ctx, inst.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.CleanupReceipt(ctx, request.ID, request.Actor); !errors.Is(err, sql.ErrNoRows) {
					t.Fatal("premature receipt", err)
				}
				if remote != (failure == "cold") {
					t.Fatal("wrong remote disposition")
				}
				fail = false
				if failure == "commit" {
					if _, err := db.Exec(`DROP TRIGGER fail_cleanup`); err != nil {
						t.Fatal(err)
					}
				}
				if err := st.InterruptLifecycleRequests(ctx); err != nil {
					t.Fatal(err)
				}
				deleted, err := p.ReconcileInstance(ctx, inst.Name)
				if err != nil || deleted.State != model.StateDeleted || remote {
					t.Fatalf("retry failed: %+v, %v", deleted, err)
				}
				proof, err := st.CleanupReceipt(ctx, request.ID, request.Actor)
				if err != nil || proof.Version != 5 || proof.Kind != kind || proof.BackupsAbsent {
					t.Fatal(proof, err)
				}
				if err := absentPath(filepath.Dir(marker)); err != nil {
					t.Fatal(err)
				}
				if data, err := os.ReadFile(backup); err != nil || string(data) != "preserved" {
					t.Fatal("backup changed", err)
				}
				if data, err := os.ReadFile(neighbor); err != nil || string(data) != "preserved" {
					t.Fatal("neighbor changed", err)
				}
			})
		}
	}
}

func TestVMDeletionPreservesOtherTailnetIdentities(t *testing.T) {
	for _, partialImport := range []bool{false, true} {
		t.Run(fmt.Sprint(partialImport), func(t *testing.T) {
			ctx := context.Background()
			cfg := loadProvisionTestConfig(t, nil)
			cfg.OutboundInterface = "fixture0"
			st := newProvisionTestStore(t, cfg)
			inst := provisionTestInstance(cfg, "tailnet-delete", model.StateStopped, time.Now().UTC())
			inst.TailnetNodeID = "owned-node"
			if partialImport {
				inst.State = model.StateProvisioning
			}
			if err := st.CreateInstance(ctx, inst); err != nil {
				t.Fatal(err)
			}
			if partialImport {
				if err := st.PutImportReceipt(ctx, model.ImportReceipt{InstanceID: inst.ID, Phase: "importing"}); err != nil {
					t.Fatal(err)
				}
			}
			var removed atomic.Bool
			var fail atomic.Bool
			fail.Store(true)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if partialImport {
					t.Error("import cleanup touched shared identity")
				}
				if r.Method == http.MethodGet {
					devices := []tailscale.Device{{NodeID: "neighbor-node", Name: inst.Name}}
					if !removed.Load() {
						devices = append(devices, tailscale.Device{NodeID: inst.TailnetNodeID, Name: inst.Name})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"devices": devices})
					return
				}
				if r.Method != http.MethodDelete || r.URL.Path != "/api/v2/device/owned-node" {
					t.Errorf("wrong deletion: %s %s", r.Method, r.URL.Path)
				}
				removed.Store(true)
				if fail.Load() {
					http.Error(w, "reply lost after deletion", 503)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			p := &Provisioner{cfg: cfg, store: st, vmRunner: noopVMRunner{}, networkHelper: noopNetworkHelper{}, tsClient: &tailscale.Client{BaseURL: base}}
			p.SetCleanup("backend", func(context.Context, model.Instance) error { return nil })
			got, err := p.Delete(ctx, inst.Name)
			if !partialImport {
				if err == nil || got.State != model.StateDeleting || got.TailnetNodeID != inst.TailnetNodeID {
					t.Fatalf("lost retry binding: %+v %v", got, err)
				}
				fail.Store(false)
				got, err = p.ReconcileInstance(ctx, inst.Name)
			}
			if err != nil || got.State != model.StateDeleted || removed.Load() == partialImport {
				t.Fatalf("wrong cleanup: %+v %v", got, err)
			}
		})
	}
}
