package provision

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"srv/internal/model"
	"srv/internal/vmrunner"
)

type coldTestRunner struct {
	noopVMRunner
	call func(vmrunner.ColdRequest) (vmrunner.ColdStatus, error)
}

func (r coldTestRunner) ColdDisk(_ context.Context, req vmrunner.ColdRequest) (vmrunner.ColdStatus, error) {
	return r.call(req)
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
				if _, err := p.recoveryCopiesAbsent(context.Background(), inst); err == nil {
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
