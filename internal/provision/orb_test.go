package provision

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"srv/internal/model"
	"srv/internal/vmrunner"
)

// The runner substitutes only the hypervisor. Readiness uses the real Unix
// transport and independently implements Firecracker's vsock handshake.
type orbLifecycleRunner struct {
	noopVMRunner
	running bool
	starts  []vmrunner.StartRequest
}

func (r *orbLifecycleRunner) StartInstanceVM(_ context.Context, req vmrunner.StartRequest) (vmrunner.StartResponse, error) {
	r.running = true
	r.starts = append(r.starts, req)
	return vmrunner.StartResponse{Outcome: vmrunner.StartRunning, PID: os.Getpid()}, nil
}

func (r *orbLifecycleRunner) StopInstanceVM(context.Context, vmrunner.StopRequest) error {
	r.running = false
	return nil
}

func (r *orbLifecycleRunner) ResolveInstanceStart(context.Context, vmrunner.StartResultRequest) (vmrunner.StartResponse, error) {
	if r.running {
		return vmrunner.StartResponse{Outcome: vmrunner.StartRunning, PID: os.Getpid()}, nil
	}
	return vmrunner.StartResponse{Outcome: vmrunner.StartAbsent}, nil
}

func TestOrbLifecycleWithoutGuestTailscale(t *testing.T) {
	templateTestTools(t)
	ctx := context.Background()
	// Keep the Unix socket below Linux's pathname limit.
	dir, err := os.MkdirTemp("", "orb-life-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cfg := loadProvisionTestConfig(t, map[string]string{"SRV_DATA_DIR": dir})
	cfg.BaseRootFSPath, cfg.BaseKernelPath = filepath.Join(dir, "base.img"), filepath.Join(dir, "kernel")
	cfg.OutboundInterface, cfg.GuestReadyTimeout = "fixture0", time.Second
	cfg.GuestAuthTags = nil
	templateTestImage(t, cfg.BaseRootFSPath, map[string]string{"home/user/prepared": "keep-me"})
	if err := os.WriteFile(cfg.BaseKernelPath, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	st := newProvisionTestStore(t, cfg)
	runner := &orbLifecycleRunner{}
	p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		vmRunner: runner, networkHelper: noopNetworkHelper{}}
	if err := os.MkdirAll(cfg.InstancesDir(), 0700); err != nil {
		t.Fatal(err)
	}
	// Create starts synchronously; provide its endpoint after the directory exists.
	ready := make(chan error, 1)
	serverCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		path := filepath.Join(cfg.InstancesDir(), "orb", "orb.vsock")
		for {
			if _, err := os.Stat(filepath.Dir(path)); err == nil {
				break
			}
			select {
			case <-serverCtx.Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
		listener, err := net.Listen("unix", path)
		if err != nil {
			ready <- err
			return
		}
		defer listener.Close()
		stop := context.AfterFunc(serverCtx, func() { listener.Close() })
		defer stop()
		ready <- nil
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.SetDeadline(time.Now().Add(time.Second))
			reader := bufio.NewReader(conn)
			line, _ := reader.ReadString('\n')
			if line == "CONNECT 4050\n" {
				fmt.Fprintln(conn, "OK 4050")
				var request map[string]string
				if json.NewDecoder(reader).Decode(&request) == nil && request["action"] == "ping" {
					fmt.Fprintln(conn, `{"ok":true}`)
				}
			}
			conn.Close()
		}
	}()
	actor := model.Actor{UserLogin: "owner"}
	inst, err := p.Create(ctx, "orb", actor, CreateOptions{Kind: model.KindOrb, MemoryMiB: 128, RootFSSizeBytes: 32 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	if inst.State != model.StateReady || inst.Kind != model.KindOrb || inst.TailscaleIP != "" || inst.TailnetNodeID != "" {
		t.Fatalf("unexpected orb: %+v", inst)
	}
	before, err := os.ReadFile(inst.RootFSPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stop(ctx, inst.Name); err != nil {
		t.Fatal(err)
	}
	if inst, err = p.Start(ctx, inst.Name); err != nil || inst.State != model.StateReady {
		t.Fatalf("resume: %+v %v", inst, err)
	}
	after, err := os.ReadFile(inst.RootFSPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("resume changed disk: %v", err)
	}
	for _, req := range runner.starts {
		if req.Kind != model.KindOrb || req.Bootstrap.TailscaleAuthKey != "" || len(req.Bootstrap.TailscaleTags) != 0 {
			t.Fatal("orb launch requested native enrollment")
		}
	}
	if _, err := p.Create(ctx, "vm", actor, CreateOptions{Kind: model.KindVM}); err == nil || !strings.Contains(err.Error(), "requires TS_TAILNET") {
		t.Fatalf("ordinary VM Tailscale prerequisites: %v", err)
	}
	if _, err := p.Stop(ctx, inst.Name); err != nil {
		t.Fatal(err)
	}
	if err := p.CreateTemplate(ctx, inst.Name, "prepared-orb", actor); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(ctx, "wrong-kind", actor, CreateOptions{Kind: model.KindVM, Template: "prepared-orb"}); err == nil || !strings.Contains(err.Error(), "template workload kind") {
		t.Fatalf("Orb template kind check: %v", err)
	}
	backup, err := p.CreateBackup(ctx, inst.Name)
	if err != nil {
		t.Fatal(err)
	}
	if restored, _, err := p.RestoreBackup(ctx, inst.Name, backup.ID); err != nil || restored.Kind != model.KindOrb {
		t.Fatalf("restored kind: %s, %v", restored.Kind, err)
	}
	var artifact bytes.Buffer
	if _, err := p.ExportInstance(ctx, inst.Name, &artifact); err != nil {
		t.Fatal(err)
	}
	tr, zr, err := newPortableArtifactTarReader(bytes.NewReader(artifact.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := readPortableManifest(tr)
	zr.Close()
	if err != nil || manifest.Version != 4 || manifest.Instance.Kind != model.KindOrb {
		t.Fatalf("Orb export must require kind-aware reader: %+v, %v", manifest, err)
	}
	destCfg := loadProvisionTestConfig(t, map[string]string{
		"SRV_DATA_DIR": t.TempDir(), "SRV_BASE_KERNEL": cfg.BaseKernelPath,
	})
	dest := &Provisioner{cfg: destCfg, store: newProvisionTestStore(t, destCfg), log: p.log}
	if err := os.MkdirAll(destCfg.InstancesDir(), 0700); err != nil {
		t.Fatal(err)
	}
	imported, _, err := dest.ImportInstance(ctx, actor, bytes.NewReader(artifact.Bytes()), nil)
	if err != nil || imported.Kind != model.KindOrb {
		t.Fatalf("imported kind: %s, %v", imported.Kind, err)
	}
	stored, err := dest.store.GetInstance(ctx, inst.Name)
	if err != nil || stored.Kind != model.KindOrb {
		t.Fatalf("persisted import kind: %s, %v", stored.Kind, err)
	}
}
