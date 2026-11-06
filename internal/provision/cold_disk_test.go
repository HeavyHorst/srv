package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"srv/internal/host"
	"srv/internal/model"
	"srv/internal/storage"
	"srv/internal/vmrunner"
)

type coldTestRunner struct {
	noopVMRunner
	templateTestBootResolver
	call func(vmrunner.ColdRequest) (vmrunner.ColdStatus, error)
}

func (r coldTestRunner) ColdDisk(_ context.Context, req vmrunner.ColdRequest) (vmrunner.ColdStatus, error) {
	return r.call(req)
}

// Keep disk consumers real; replace only the runner's privileged transfer.
// The active disk is genuinely absent until the command requests hydration.
func offloadTestDisk(t *testing.T, p *Provisioner, inst model.Instance) {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "remote.img")
	if err := os.Rename(inst.RootFSPath, remote); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(p.cfg.InstancesDir(), inst.Name, "cold-disk.json")
	phase := "cold"
	writeMarker := func() {
		t.Helper()
		data, err := json.Marshal(coldDiskMarker{InstanceID: inst.ID, Phase: phase, Remote: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeMarker()
	p.vmRunner = coldTestRunner{call: func(q vmrunner.ColdRequest) (vmrunner.ColdStatus, error) {
		if q.InstanceID != inst.ID {
			return vmrunner.ColdStatus{Phase: "hot"}, nil
		}
		switch q.Action {
		case "status":
		case "hydrate":
			if phase == "cold" {
				if err := os.Rename(remote, inst.RootFSPath); err != nil {
					return vmrunner.ColdStatus{}, err
				}
				phase = "hot"
				writeMarker()
			}
		default:
			return vmrunner.ColdStatus{}, errors.New("unexpected cold operation: " + q.Action)
		}
		return vmrunner.ColdStatus{Phase: phase, Remote: true, JournalPresent: true}, nil
	}}
}

func TestResizePoolShrunkDuringHydration(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	now := time.Now().UTC()
	pool := model.MemoryPool{ID: "pool-id", Name: "pool-a", ReservedBytes: 2 << 30, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateMemoryPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	inst := provisionTestInstance(cfg, "member", model.StateStopped, now)
	inst.MemoryMode, inst.MemoryPoolID, inst.MemoryMiB = model.MemoryModePool, pool.ID, 1024
	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.RootFSPath, []byte("disk"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	offloadTestDisk(t, p, inst)
	runner := p.vmRunner.(coldTestRunner)
	p.vmRunner = coldTestRunner{call: func(q vmrunner.ColdRequest) (vmrunner.ColdStatus, error) {
		if q.Action == "hydrate" {
			if _, err := p.ResizeMemoryPool(ctx, pool.Name, 1<<30); err != nil {
				return vmrunner.ColdStatus{}, err
			}
		}
		return runner.ColdDisk(ctx, q)
	}}
	if _, err := p.Resize(ctx, inst.Name, CreateOptions{MemoryMiB: 1536}); err == nil || !strings.Contains(err.Error(), "exceeds memory pool") {
		t.Fatalf("resize error = %v", err)
	}
	stored, err := st.GetInstance(ctx, inst.Name)
	if err != nil || stored.MemoryMiB != 1024 {
		t.Fatalf("guest RAM changed: %+v, %v", stored, err)
	}
	storedPool, err := st.GetMemoryPoolByName(ctx, pool.Name)
	if err != nil || storedPool.ReservedBytes != 1<<30 {
		t.Fatalf("pool did not shrink during hydration: %+v, %v", storedPool, err)
	}
}

func TestResizePoolDeletedDuringHydration(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleted", true: "recreated"}[recreate], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cfg := loadProvisionTestConfig(t, nil)
			st := newProvisionTestStore(t, cfg)
			p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			inst := provisionTestInstance(cfg, "cold-source", model.StateStopped, time.Now().UTC())
			inst.MemoryMiB = 1024
			inst.MemoryMode = model.MemoryModeFixed
			if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(inst.RootFSPath, []byte("disk"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateInstance(ctx, inst); err != nil {
				t.Fatal(err)
			}
			pool := model.MemoryPool{ID: "original", Name: "target", ReservedBytes: 2 << 30, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
			if err := st.CreateMemoryPool(ctx, pool); err != nil {
				t.Fatal(err)
			}
			offloadTestDisk(t, p, inst)
			runner := p.vmRunner.(coldTestRunner)
			hydrating, resume := make(chan struct{}), make(chan struct{})
			p.vmRunner = coldTestRunner{call: func(q vmrunner.ColdRequest) (vmrunner.ColdStatus, error) {
				if q.Action == "hydrate" {
					close(hydrating)
					select {
					case <-resume:
					case <-ctx.Done():
						return vmrunner.ColdStatus{}, ctx.Err()
					}
				}
				return runner.ColdDisk(ctx, q)
			}}
			done := make(chan error, 1)
			var workers sync.WaitGroup
			workers.Add(1)
			t.Cleanup(func() { cancel(); workers.Wait() })
			go func() {
				defer workers.Done()
				_, err := p.Resize(ctx, inst.Name, CreateOptions{MemoryPoolName: pool.Name, MemoryMiB: 2048})
				done <- err
			}()
			select {
			case <-hydrating:
			case err := <-done:
				t.Fatal("resize did not reach hydration", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if _, err := p.DeleteMemoryPool(ctx, pool.Name); err != nil {
				t.Fatal(err)
			}
			if recreate {
				pool.ID = "replacement"
				if err := st.CreateMemoryPool(ctx, pool); err != nil {
					t.Fatal(err)
				}
			}
			close(resume)
			if err := <-done; err == nil || !strings.Contains(err.Error(), "load memory pool") {
				t.Fatalf("resize error = %v, want missing resolved pool", err)
			}
			updated, err := st.GetInstance(ctx, inst.Name)
			if err != nil {
				t.Fatal(err)
			}
			if updated.MemoryPoolID != inst.MemoryPoolID || updated.MemoryMode != inst.MemoryMode || updated.MemoryMiB != inst.MemoryMiB {
				t.Fatalf("failed resize changed memory configuration: %+v", updated)
			}
		})
	}
}

func TestAutomaticHydrationDoesNotBlockImportOrHotResize(t *testing.T) {
	for _, command := range []string{"migration", "fsck", "resize", "template"} {
		t.Run(command, func(t *testing.T) {
			templateTestTools(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cfg := loadProvisionTestConfig(t, nil)
			st := newProvisionTestStore(t, cfg)
			p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			inst := provisionTestInstance(cfg, "cold-source", model.StateStopped, time.Now().UTC())
			templateTestImage(t, inst.RootFSPath, map[string]string{"home/user/project/data": "preserved"})
			templateTestBootFiles(t, inst)
			if err := st.CreateInstance(ctx, inst); err != nil {
				t.Fatal(err)
			}
			offloadTestDisk(t, p, inst)
			runner := p.vmRunner.(coldTestRunner)
			hot := provisionTestInstance(cfg, "hot-neighbor", model.StateStopped, time.Now().UTC())
			if err := os.MkdirAll(filepath.Dir(hot.RootFSPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(hot.RootFSPath, []byte("hot"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateInstance(ctx, hot); err != nil {
				t.Fatal(err)
			}

			importHeld, allowImport := make(chan struct{}), make(chan struct{})
			hydrateWaiting, downloading, allowHydrate := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var gateImport sync.Once
			p.readFilesystemUsage = func(path string) (host.FilesystemUsage, error) {
				gateImport.Do(func() {
					close(importHeld) // Import owns payload but has not acquired admission.
					select {
					case <-allowImport:
					case <-ctx.Done():
					}
				})
				return host.DefaultReadFilesystemUsage(path)
			}
			p.vmRunner = coldTestRunner{call: func(q vmrunner.ColdRequest) (vmrunner.ColdStatus, error) {
				if q.Action == "hydrate" {
					close(hydrateWaiting)
					// Match the runner's independent host payload lock, including real
					// flock contention with the import (ownership checks are stubbed).
					_, release, err := storage.AcquirePayload(ctx, cfg.DataDirAbs())
					if err != nil {
						return vmrunner.ColdStatus{}, err
					}
					defer release()
					close(downloading)
					select {
					case <-allowHydrate:
					case <-ctx.Done():
						return vmrunner.ColdStatus{}, ctx.Err()
					}
				}
				return runner.ColdDisk(ctx, q)
			}}
			var workers sync.WaitGroup
			t.Cleanup(func() { cancel(); workers.Wait() })
			manifest := portableManifest{Version: portableManifestVersion, ExportedAt: time.Now().UTC(), Instance: portableInstance{Name: "import-neighbor", CreatedAt: time.Now().UTC(), RootFSSizeBytes: 6}, Files: backupFiles{RootFS: backupRootFSName}}
			stream := portableArtifactTestStream(t, manifest, map[string][]byte{backupRootFSName: []byte("import")})
			importDone, commandDone := make(chan error, 1), make(chan error, 1)
			workers.Add(1)
			go func() {
				defer workers.Done()
				_, _, err := p.ImportInstance(ctx, model.Actor{}, bytes.NewReader(stream))
				importDone <- err
			}()
			select {
			case <-importHeld:
			case err := <-importDone:
				t.Fatal("import did not reach payload gate", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				var err error
				switch command {
				case "migration":
					_, err = p.PrepareMigration(ctx, inst.Name, "destination", "token")
				case "fsck":
					_, err = p.CheckRootFS(ctx, inst.Name)
				case "resize":
					_, err = p.Resize(ctx, inst.Name, CreateOptions{VCPUCount: 4})
				case "template":
					err = p.CreateTemplate(ctx, inst.Name, "prepared", model.Actor{UserLogin: inst.CreatedByUser})
				}
				commandDone <- err
			}()
			select {
			case <-hydrateWaiting:
			case err := <-commandDone:
				t.Fatal("command did not reach hydration", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			close(allowImport)
			select {
			case err := <-importDone:
				if err != nil {
					t.Fatal("parallel import failed", err)
				}
			case <-ctx.Done():
				t.Fatal("hydration blocked import admission", ctx.Err())
			}
			select {
			case <-downloading:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// A long download must not hold up unrelated admission work either.
			hotDone := make(chan error, 1)
			workers.Add(1)
			go func() {
				defer workers.Done()
				_, err := p.Resize(ctx, hot.Name, CreateOptions{VCPUCount: 4})
				hotDone <- err
			}()
			select {
			case err := <-hotDone:
				if err != nil {
					t.Fatal("hot resize failed during download", err)
				}
			case <-ctx.Done():
				t.Fatal("download blocked unrelated admission", ctx.Err())
			}
			close(allowHydrate)
			if err := <-commandDone; err != nil {
				t.Fatal("cold command failed", err)
			}
			imported, err := st.GetInstance(ctx, manifest.Instance.Name)
			if err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(imported.RootFSPath); err != nil || string(data) != "import" {
				t.Fatalf("parallel import data = %q, %v", data, err)
			}
			resized, err := st.GetInstance(ctx, hot.Name)
			if err != nil || resized.VCPUCount != 4 {
				t.Fatalf("hot resize was not persisted: %+v, %v", resized, err)
			}
		})
	}
}

func TestColdDiskHiddenJournal(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	cfg.ColdStorageHost = ""
	st := newProvisionTestStore(t, cfg)
	inst := provisionTestInstance(cfg, "hidden", model.StateStopped, time.Now())
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	phase := "cold"
	var actions []string
	p := &Provisioner{cfg: cfg, store: st, vmRunner: coldTestRunner{call: func(req vmrunner.ColdRequest) (vmrunner.ColdStatus, error) {
		actions = append(actions, req.Action)
		switch req.Action {
		case "status":
		case "hydrate":
			if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(inst.RootFSPath, []byte("restored"), 0600); err != nil {
				t.Fatal(err)
			}
			phase = "hot"
		default:
			t.Fatalf("unexpected action %s", req.Action)
		}
		return vmrunner.ColdStatus{Phase: phase, Remote: true, JournalPresent: true}, nil
	}}}
	status, err := p.ColdDisk(ctx, inst.Name, "status")
	if err != nil || status.Phase != "cold" {
		t.Fatalf("hidden status: %+v %v", status, err)
	}
	if err := p.coldDiskFence(ctx, inst, false); err == nil {
		t.Fatal("hidden cold journal admitted mutation")
	}
	if err := p.hydrateColdDisk(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if strings.Join(actions, ",") != "status,status,status,hydrate,status" {
		t.Fatalf("actions=%v", actions)
	}
	if err := p.coldDiskFence(ctx, inst, false); err != nil {
		t.Fatalf("hot retained generation blocks ordinary mutation: %v", err)
	}
	// Even a hot journal with no remote object is not absence of recovery state.
	p.vmRunner = coldTestRunner{call: func(vmrunner.ColdRequest) (vmrunner.ColdStatus, error) {
		return vmrunner.ColdStatus{Phase: "hot", JournalPresent: true}, nil
	}}
	if err := p.coldDiskFence(ctx, inst, true); err == nil {
		t.Fatal("hidden hot journal admitted absence proof")
	}
}

func TestColdDiskFailsClosed(t *testing.T) {
	for _, kind := range []string{"rpc-error", "status-error", "unknown", "empty", "missing-rootfs", "legacy", "legacy-no-rootfs", "legacy-configured"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			cfg := loadProvisionTestConfig(t, nil)
			cfg.ColdStorageHost = ""
			inst := provisionTestInstance(cfg, "closed", model.StateStopped, time.Now())
			if kind != "missing-rootfs" && kind != "legacy-no-rootfs" {
				if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(inst.RootFSPath, []byte("local"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			p := &Provisioner{cfg: cfg, vmRunner: coldTestRunner{call: func(req vmrunner.ColdRequest) (vmrunner.ColdStatus, error) {
				if req.Action != "status" {
					t.Fatalf("unexpected action: %+v", req)
				}
				switch kind {
				case "rpc-error":
					return vmrunner.ColdStatus{}, errors.New("upgrade srv-vm-runner")
				case "status-error":
					return vmrunner.ColdStatus{Phase: "hot", Error: "journal unreadable"}, nil
				case "unknown":
					return vmrunner.ColdStatus{Phase: "future-phase"}, nil
				case "empty":
					return vmrunner.ColdStatus{}, nil
				default:
					return vmrunner.ColdStatus{Phase: "hot"}, nil
				}
			}}}
			if strings.HasPrefix(kind, "legacy") {
				p.vmRunner = struct{ vmRunner }{noopVMRunner{}}
			}
			if kind == "legacy-configured" {
				p.cfg.ColdStorageHost = "cold-host"
			}
			err := p.hydrateColdDisk(ctx, inst)
			if (err == nil) != (kind == "legacy") {
				t.Fatalf("hydrate: %v", err)
			}
			if kind == "rpc-error" && !strings.Contains(err.Error(), "upgrade srv-vm-runner") {
				t.Fatalf("RPC error hidden: %v", err)
			}
		})
	}
}

func TestColdDiskCleanupForRemoval(t *testing.T) {
	for _, kind := range []string{"cleared", "rpc-error", "remote", "journal", "marker-remains"} {
		t.Run(kind, func(t *testing.T) {
			cfg := loadProvisionTestConfig(t, nil)
			cfg.ColdStorageHost = ""
			inst := provisionTestInstance(cfg, "remove", model.StateStopped, time.Now())
			p := &Provisioner{cfg: cfg, vmRunner: coldTestRunner{call: func(req vmrunner.ColdRequest) (vmrunner.ColdStatus, error) {
				if req.Action != "delete" || req.InstanceID != inst.ID {
					t.Fatalf("request: %+v", req)
				}
				if kind == "rpc-error" {
					return vmrunner.ColdStatus{}, errors.New("remote unavailable")
				}
				if kind == "marker-remains" {
					dir := filepath.Join(cfg.InstancesDir(), inst.Name)
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "cold-disk.json"), []byte(`{"instance_id":"`+inst.ID+`","phase":"hot","remote":false}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return vmrunner.ColdStatus{Phase: "hot", Remote: kind == "remote", JournalPresent: kind == "journal"}, nil
			}}}
			if err := p.cleanupColdForRemoval(context.Background(), inst); (err == nil) != (kind == "cleared") {
				t.Fatalf("cleanup without rootfs: %v", err)
			}
		})
	}
}

func TestColdDiskFencesAndHydration(t *testing.T) {
	for _, phase := range []string{"absent", "hot", "cold", "uploading", "malformed", "wrong-id", "missing-remote"} {
		t.Run(phase, func(t *testing.T) {
			cfg := loadProvisionTestConfig(t, nil)
			inst := provisionTestInstance(cfg, "cold-test", model.StateStopped, time.Now())
			dir := filepath.Join(cfg.InstancesDir(), inst.Name)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(inst.RootFSPath, []byte("local"), 0600); err != nil {
				t.Fatal(err)
			}
			marker := `{"instance_id":"` + inst.ID + `","phase":"` + phase + `","remote":true}`
			if phase == "malformed" {
				marker = "{"
			}
			if phase == "wrong-id" {
				marker = `{"instance_id":"other","phase":"hot","remote":true}`
			}
			if phase == "missing-remote" {
				marker = `{"instance_id":"` + inst.ID + `","phase":"hot"}`
			}
			if phase != "absent" {
				if err := os.WriteFile(filepath.Join(dir, "cold-disk.json"), []byte(marker), 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			p := &Provisioner{cfg: cfg, vmRunner: coldTestRunner{call: func(req vmrunner.ColdRequest) (vmrunner.ColdStatus, error) {
				calls++
				if req.InstanceID != inst.ID || req.Name != inst.Name {
					t.Fatalf("request: %+v", req)
				}
				if req.Action == "status" {
					if phase == "absent" || phase == "hot" {
						return vmrunner.ColdStatus{Phase: "hot"}, nil
					}
					return vmrunner.ColdStatus{Phase: "cold", Remote: true}, nil
				}
				if req.Action != "hydrate" {
					t.Fatalf("request: %+v", req)
				}
				return vmrunner.ColdStatus{}, errors.New("remote unavailable")
			}}}
			hot := phase == "absent" || phase == "hot"
			if err := p.hydrateColdDisk(context.Background(), inst); (err == nil) != hot {
				t.Fatalf("hydrate: %v", err)
			}
			wantCalls := 0
			if hot {
				wantCalls = 1
			} else if phase == "cold" || phase == "uploading" {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d want=%d", calls, wantCalls)
			}
			if err := p.coldDiskFence(context.Background(), inst, false); (err == nil) != hot {
				t.Fatalf("mutation fence: %v", err)
			}
			if phase == "cold" {
				if err := p.coldDiskFence(context.Background(), inst, false); !strings.Contains(err.Error(), "disk hydrate") {
					t.Fatal(err)
				}
			}
			if phase != "absent" {
				if err := p.coldDiskFence(context.Background(), inst, true); err == nil {
					t.Fatal("cleanup proof accepted marker")
				}
			}
		})
	}
}

func TestColdDiskLifecycleFailuresRetainState(t *testing.T) {
	for _, action := range []string{"start", "delete", "offload", "status", "hydrate", "purge"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			cfg := loadProvisionTestConfig(t, nil)
			st := newProvisionTestStore(t, cfg)
			inst := provisionTestInstance(cfg, "cold-life", model.StateStopped, time.Now())
			inst.Kind = model.KindOrb
			if err := st.CreateInstance(ctx, inst); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(cfg.InstancesDir(), inst.Name)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			// Missing public marker AND disabled config must not hide a journal.
			if action != "status" && action != "hydrate" && action != "purge" {
				if err := os.WriteFile(filepath.Join(dir, "cold-disk.json"), []byte(`{"instance_id":"`+inst.ID+`","phase":"cold","remote":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), vmRunner: coldTestRunner{call: func(req vmrunner.ColdRequest) (vmrunner.ColdStatus, error) {
				calls++
				want := action
				if want == "start" {
					want = "status"
				}
				if req.Action != want {
					t.Fatalf("action=%s want=%s", req.Action, want)
				}
				return vmrunner.ColdStatus{}, errors.New("remote unavailable")
			}}}
			p.networkHelper = noopNetworkHelper{}
			p.SetCleanup("backend", func(context.Context, model.Instance) error { return nil })
			var err error
			switch action {
			case "start":
				_, err = p.Start(ctx, inst.Name)
			case "delete":
				_, err = p.Delete(ctx, inst.Name)
			default:
				_, err = p.ColdDisk(ctx, inst.Name, action)
			}
			if err == nil {
				t.Fatal("expected failure")
			}
			wantCalls := 1
			if action == "offload" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d want=%d: %v", calls, wantCalls, err)
			}
			current, err := st.GetInstance(ctx, inst.Name)
			if err != nil {
				t.Fatal(err)
			}
			wantState := model.StateStopped
			if action == "delete" {
				wantState = model.StateDeleting
			}
			if current.State != wantState {
				t.Fatalf("state=%s want=%s", current.State, wantState)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Fatal("instance metadata removed", err)
			}
		})
	}
}
