package provision

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"srv/internal/host"
	"srv/internal/model"
	"srv/internal/vmrunner"
)

// Model the runner's permanent name+UUID deletion fence without a hypervisor.
// Successful admission stops at an explicit sentinel, not a pretend guest boot.
type importIncarnationRunner struct {
	noopVMRunner
	deleted  map[string]bool
	admitted error
}

func (r *importIncarnationRunner) ColdDisk(_ context.Context, req vmrunner.ColdRequest) (vmrunner.ColdStatus, error) {
	if req.Action == "delete" {
		r.deleted[req.Name+"/"+req.InstanceID] = true
	}
	return vmrunner.ColdStatus{Phase: "hot"}, nil
}

func (r *importIncarnationRunner) StartInstanceVM(_ context.Context, req vmrunner.StartRequest) (vmrunner.StartResponse, error) {
	if r.deleted[req.Name+"/"+req.InstanceID] {
		return vmrunner.StartResponse{}, errors.New("incarnation permanently deleted")
	}
	return vmrunner.StartResponse{}, r.admitted
}

func TestImportAfterDeleteUsesFreshIncarnation(t *testing.T) {
	for _, migration := range []bool{false, true} {
		name := "ordinary"
		if migration {
			name = "migration-return"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg := loadProvisionTestConfig(t, nil)
			cfg.ColdStorageHost = "cold-host"
			cfg.BaseKernelPath = filepath.Join(t.TempDir(), "kernel")
			cfg.BaseInitrdPath = ""
			cfg.OutboundInterface = "fixture0"
			if err := os.WriteFile(cfg.BaseKernelPath, []byte("kernel"), 0600); err != nil {
				t.Fatal(err)
			}
			st := newProvisionTestStore(t, cfg)
			runner := &importIncarnationRunner{deleted: make(map[string]bool), admitted: errors.New("fresh incarnation admitted")}
			p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), vmRunner: runner, networkHelper: noopNetworkHelper{}, readFilesystemUsage: host.DefaultReadFilesystemUsage}
			inst := provisionTestInstance(cfg, "returning", model.StateStopped, time.Now().UTC())
			inst.Kind = model.KindOrb
			inst.RootFSSizeBytes = 6
			if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(inst.RootFSPath, []byte("rootfs"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateInstance(ctx, inst); err != nil {
				t.Fatal(err)
			}
			if migration {
				if _, err := p.PrepareMigration(ctx, inst.Name, "away", "outbound"); err != nil {
					t.Fatal(err)
				}
			}
			var artifact bytes.Buffer
			if _, err := p.ExportInstance(ctx, inst.Name, &artifact); err != nil {
				t.Fatal(err)
			}
			if migration {
				if _, err := p.FinishMigration(ctx, inst.Name, "outbound", false); err != nil {
					t.Fatal(err)
				}
				// A legacy destination may still carry the original UUID. Its
				// return artifact must not resurrect that UUID on this host.
				manifest := portableManifestFromInstance(time.Now().UTC(), inst)
				manifest.MigrationToken = "return-token"
				artifact.Reset()
				artifact.Write(portableArtifactTestStream(t, manifest, map[string][]byte{backupRootFSName: []byte("rootfs")}))
			}
			if _, err := p.Delete(ctx, inst.Name); err != nil {
				t.Fatal(err)
			}
			imported, info, err := p.ImportInstance(ctx, model.Actor{}, bytes.NewReader(artifact.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if imported.ID == "" || imported.ID == inst.ID {
				t.Fatalf("reused deleted incarnation: %q", imported.ID)
			}
			receipt, err := st.GetImportReceipt(ctx, imported.ID)
			if err != nil || receipt.SourceInstanceID != inst.ID || receipt.Token != info.MigrationToken || receipt.Phase != "imported" {
				t.Fatalf("receipt: %+v, %v", receipt, err)
			}
			if _, err := p.Start(ctx, inst.Name); err == nil || !strings.Contains(err.Error(), runner.admitted.Error()) {
				t.Fatalf("fresh import did not reach runner start admission: %v", err)
			}
			if _, err := runner.StartInstanceVM(ctx, vmrunner.StartRequest{Name: inst.Name, InstanceID: inst.ID}); err == nil || !strings.Contains(err.Error(), "permanently deleted") {
				t.Fatalf("stale incarnation fence lost: %v", err)
			}
		})
	}
}
