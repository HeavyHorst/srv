package provision

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"srv/internal/config"
	"srv/internal/model"
	"srv/internal/vmrunner"
)

type upgradeTestRunner struct {
	cleanupRunner
	bootClient *vmrunner.Client
}

func (r upgradeTestRunner) ResolveBootArtifacts(ctx context.Context, req vmrunner.StartRequest) (vmrunner.BootFiles, error) {
	return r.bootClient.ResolveBootArtifacts(ctx, req)
}

func TestKernelUpgradeRequiresStoppedInstance(t *testing.T) {
	for _, tc := range []struct{ kind, state string }{
		{model.KindVM, model.StateReady}, {model.KindVM, model.StateStopping},
		{model.KindOrb, model.StateReady}, {model.KindOrb, model.StateStopping},
	} {
		t.Run(tc.kind+"-"+tc.state, func(t *testing.T) {
			cfg := loadProvisionTestConfig(t, nil)
			st := newProvisionTestStore(t, cfg)
			inst := provisionTestInstance(cfg, "demo", tc.state, time.Now())
			inst.Kind = tc.kind
			if err := st.CreateInstance(context.Background(), inst); err != nil {
				t.Fatal(err)
			}
			p := &Provisioner{cfg: cfg, store: st}
			for _, rollback := range []bool{false, true} {
				if _, err := p.UpgradeKernel(context.Background(), inst.Name, rollback); err == nil {
					t.Fatal("unsafe upgrade/rollback accepted")
				}
			}
		})
	}
}

func TestKernelUpgradeAndRecovery(t *testing.T) {
	for _, kind := range []string{model.KindVM, model.KindOrb} {
		for _, failure := range []string{"none", "hash", "database", "symlink", "external-boot", "external-images", "legacy-only", "old-runner", "legacy-symlinks", "boot-symlinks", "disk-symlink", "escape-target", "escape-kernel", "escape-initrd", "private-root", "private-initrd"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				realClone := reflinkCloneFile
				templateTestTools(t)
				ctx := context.Background()
				cfg := loadProvisionTestConfig(t, nil)
				st := newProvisionTestStore(t, cfg)
				p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), vmRunner: cleanupRunner{verify: func() error { return nil }}}
				inst := provisionTestInstance(cfg, "upgrade-test", model.StateStopped, time.Now())
				inst.Kind = kind
				if kind == model.KindOrb {
					inst.Image = "amp-orb"
				}
				inst.InitrdPath = ""
				inst.RootFSSizeBytes = 32 << 20
				templateTestImage(t, inst.RootFSPath, map[string]string{"usr/lib/modules/old/old.ko": "old module", "home/user/data": "keep me"})
				base := filepath.Join(cfg.ImagesDir(), "new", "rootfs-base.img")
				runnerImages := cfg.ImagesDir()
				if failure == "external-images" {
					runnerImages = t.TempDir()
					base = filepath.Join(runnerImages, "new", "rootfs-base.img")
					inst.KernelPath = filepath.Join(runnerImages, "old-vmlinux")
				}
				templateTestImage(t, base, map[string]string{"usr/lib/modules/new/new.ko": "new module", "home/user/data": "must not replace guest data"})
				// Real base images contain root-owned files; extraction must work
				// without CAP_CHOWN in the unprivileged control-plane service.
				baseDisk := templateDisk{ctx: ctx, path: base}
				for _, field := range []string{"uid", "gid"} {
					if _, err := baseDisk.run("set_inode_field /usr/lib/modules/new/new.ko " + field + " 0"); err != nil {
						t.Fatal(err)
					}
				}
				kernel := filepath.Join(filepath.Dir(base), "vmlinux")
				if failure == "private-initrd" {
					inst.InitrdPath = filepath.Join(cfg.ImagesDir(), "initrd.img")
					if err := os.WriteFile(inst.InitrdPath, []byte("old initrd"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "legacy-only" {
					runnerImages = ""
					inst.KernelPath = filepath.Join(cfg.InstancesDir(), inst.Name, "upgrade-previous", "vmlinux")
					if err := os.MkdirAll(filepath.Dir(inst.KernelPath), 0700); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "external-boot" || failure == "legacy-symlinks" {
					bootDir, err := os.MkdirTemp("/dev/shm", "srv-upgrade-test-")
					if err != nil {
						t.Skipf("tmpfs boot-artifact fixture unavailable: %v", err)
					}
					t.Cleanup(func() { os.RemoveAll(bootDir) })
					inst.KernelPath = filepath.Join(bootDir, "old-vmlinux")
					inst.InitrdPath = filepath.Join(bootDir, "old-initrd")
					p.cfg.BaseKernelPath, p.cfg.BaseInitrdPath = inst.KernelPath, inst.InitrdPath
					if err := os.WriteFile(inst.InitrdPath, []byte("old initrd"), 0644); err != nil {
						t.Fatal(err)
					}
					// Disks use the portable fixture copier; boot files must exercise
					// real filesystem capabilities, not the suite's reflink fallback.
					diskClone := reflinkCloneFile
					reflinkCloneFile = func(ctx context.Context, src, dst string) error {
						if strings.HasPrefix(src, bootDir+"/") {
							return realClone(ctx, src, dst)
						}
						return diskClone(ctx, src, dst)
					}
				}
				for path, data := range map[string]string{kernel: "new kernel", inst.KernelPath: "old kernel"} {
					if err := os.WriteFile(path, []byte(data), 0644); err != nil {
						t.Fatal(err)
					}
				}
				var links []string
				if failure == "disk-symlink" {
					links = []string{base}
				}
				if failure == "legacy-symlinks" {
					links = []string{inst.KernelPath, inst.InitrdPath}
				}
				if failure == "external-images" {
					links = []string{kernel}
				}
				for _, path := range links {
					if err := os.Rename(path, path+".real"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(path+".real", path); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "boot-symlinks" || strings.HasPrefix(failure, "escape-") {
					inst.InitrdPath = filepath.Join(cfg.ImagesDir(), "initrd")
					if err := os.WriteFile(inst.InitrdPath, []byte("old initrd"), 0644); err != nil {
						t.Fatal(err)
					}
					for _, path := range []string{kernel, inst.KernelPath, inst.InitrdPath} {
						target := path + ".real"
						if (failure == "escape-kernel" && path == inst.KernelPath) || (failure == "escape-initrd" && path == inst.InitrdPath) || (failure == "escape-target" && path == kernel) {
							target = filepath.Join(t.TempDir(), filepath.Base(path))
						}
						if err := os.Rename(path, target); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(target, path); err != nil {
							t.Fatal(err)
						}
					}
				}
				p.cfg.Images = map[string]config.Image{inst.ImageName(): {RootFS: base, Kernel: kernel}}
				manifest := ""
				for _, path := range []string{base, kernel} {
					digest, err := diskDigest(ctx, path)
					if err != nil {
						t.Fatal(err)
					}
					manifest += fmt.Sprintf("%s  %s\n", digest, filepath.Base(path))
				}
				if err := os.WriteFile(filepath.Join(filepath.Dir(base), "manifest.txt"), []byte(manifest), 0644); err != nil {
					t.Fatal(err)
				}
				if failure == "hash" {
					if err := os.WriteFile(kernel, []byte("mismatched build"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "symlink" {
					d := templateDisk{ctx: ctx, path: inst.RootFSPath, remaining: 100}
					if err := d.clean("/usr/lib"); err != nil {
						t.Fatal(err)
					}
					if _, err := d.run("symlink /usr/lib /home/user"); err != nil {
						t.Fatal(err)
					}
				}
				before, err := diskDigest(ctx, inst.RootFSPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := st.CreateInstance(ctx, inst); err != nil {
					t.Fatal(err)
				}
				// A second VM shares the old kernel; upgrading must never overwrite it.
				neighbor := provisionTestInstance(cfg, "neighbor", model.StateStopped, time.Now())
				neighbor.KernelPath = inst.KernelPath
				if err := st.CreateInstance(ctx, neighbor); err != nil {
					t.Fatal(err)
				}
				db, err := sql.Open("sqlite", cfg.DatabasePath())
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if failure == "database" {
					if _, err := db.Exec(`CREATE TRIGGER fail_upgrade BEFORE UPDATE ON instances BEGIN SELECT RAISE(FAIL, 'upgrade DB failure'); END`); err != nil {
						t.Fatal(err)
					}
				}
				// Resolve through a real helper socket with its own effective config,
				// rather than duplicating the policy in a provisioner test double.
				socketDir, err := os.MkdirTemp("", "srv-boot-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.RemoveAll(socketDir) })
				socket := filepath.Join(socketDir, "api.sock")
				listener, err := net.Listen("unix", socket)
				if err != nil {
					t.Fatal(err)
				}
				runnerConfig := vmrunner.ServerConfig{
					ImagesDir: runnerImages, InstancesDir: cfg.InstancesDir(),
					KernelPath: p.cfg.BaseKernelPath, InitrdPath: p.cfg.BaseInitrdPath,
				}
				if failure == "legacy-only" {
					runnerConfig.KernelPath, runnerConfig.InitrdPath = kernel, ""
				}
				if failure == "private-root" {
					runnerConfig.InstancesDir = t.TempDir()
				}
				runner := vmrunner.NewServer(p.log, runnerConfig)
				server := &http.Server{Handler: runner.Handler()}
				if failure == "private-initrd" {
					// A helper refusal of the rollback initrd must abort preparation,
					// even when the new kernel and the source pair are accepted.
					server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, _ := io.ReadAll(r.Body)
						var req vmrunner.StartRequest
						if err := json.Unmarshal(body, &req); err != nil {
							http.Error(w, err.Error(), http.StatusBadRequest)
							return
						}
						if filepath.Base(req.InitrdPath) == "old-initrd" {
							http.Error(w, "private rollback initrd rejected", http.StatusForbidden)
							return
						}
						r.Body = io.NopCloser(strings.NewReader(string(body)))
						runner.Handler().ServeHTTP(w, r)
					})
				}
				if failure == "old-runner" {
					server.Handler = http.NotFoundHandler()
				}
				t.Cleanup(func() { server.Close() })
				go server.Serve(listener)
				client := vmrunner.NewClient(socket)
				p.vmRunner = upgradeTestRunner{cleanupRunner: cleanupRunner{verify: func() error { return nil }}, bootClient: client}
				upgraded, err := p.UpgradeKernel(ctx, inst.Name, false)
				if failure == "hash" || failure == "database" || failure == "symlink" || failure == "disk-symlink" || failure == "old-runner" || strings.HasPrefix(failure, "escape-") || strings.HasPrefix(failure, "private-") {
					if err == nil {
						t.Fatal("unsafe upgrade accepted")
					}
					if strings.HasPrefix(failure, "escape-") && !strings.Contains(err.Error(), "outside images") {
						t.Fatalf("wrong rejection for escaping boot symlink: %v", err)
					}
					if failure != "database" {
						after, _ := diskDigest(ctx, inst.RootFSPath)
						if after != before {
							t.Fatal("failed preparation changed live disk")
						}
						if strings.HasPrefix(failure, "private-") {
							stored, err := st.GetInstance(ctx, inst.Name)
							if err != nil || stored.KernelPath != inst.KernelPath || stored.InitrdPath != inst.InitrdPath || stored.State != inst.State {
								t.Fatalf("rejected private boot pair changed instance: %+v %v", stored, err)
							}
							if _, err := p.readUpgrade(inst.Name); !os.IsNotExist(err) {
								t.Fatalf("rejected private boot pair published a journal: %v", err)
							}
							stages, err := filepath.Glob(filepath.Join(cfg.InstancesDir(), inst.Name, "upgrade-*"))
							if err != nil || len(stages) != 0 {
								t.Fatalf("rejected private boot copies were not cleaned up: %v %v", stages, err)
							}
						}
						return
					}
					// Recreate the provisioner to model loss of all in-memory state.
					p = &Provisioner{cfg: p.cfg, store: st, log: p.log, vmRunner: p.vmRunner}
					if _, err := p.Start(ctx, inst.Name); err == nil || !strings.Contains(err.Error(), "--rollback") {
						t.Fatalf("partial commit not fenced from start: %v", err)
					}
					if _, err := p.CreateBackup(ctx, inst.Name); kind == model.KindVM && (err == nil || !strings.Contains(err.Error(), "--rollback")) {
						t.Fatalf("partial commit not fenced from backup: %v", err)
					}
					if _, err := p.UpgradeKernel(ctx, inst.Name, true); err == nil || !strings.Contains(err.Error(), "upgrade DB failure") {
						t.Fatalf("rollback database failure: %v", err)
					}
					if _, err := p.Start(ctx, inst.Name); err == nil || !strings.Contains(err.Error(), "--rollback") {
						t.Fatalf("failed rollback cleared fence: %v", err)
					}
					if _, err := db.Exec(`DROP TRIGGER fail_upgrade`); err != nil {
						t.Fatal(err)
					}
					// A crash during the replacement copy leaves this reserved file
					// inside the otherwise single-file disk subvolume.
					if err := os.WriteFile(filepath.Join(filepath.Dir(inst.RootFSPath), ".upgrade-disk-orphan"), []byte("partial copy"), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if upgraded.KernelPath == kernel || upgraded.KernelPath == inst.KernelPath {
						t.Fatal("upgrade did not pin a private kernel")
					}
					if upgraded.KindName() != kind || upgraded.ImageName() != inst.ImageName() || upgraded.ID != inst.ID {
						t.Fatal("upgrade changed instance identity or image selection")
					}
					if _, err := client.ResolveBootArtifacts(ctx, vmrunner.StartRequest{Name: inst.Name, KernelPath: upgraded.KernelPath, InitrdPath: upgraded.InitrdPath}); err != nil {
						t.Fatalf("runner rejected upgraded boot pair: %v", err)
					}
					for path, want := range map[string]string{"/usr/lib/modules/new/new.ko": "new module", "/home/user/data": "keep me"} {
						out, err := exec.Command("debugfs", "-R", "cat "+path, inst.RootFSPath).Output()
						if err != nil || string(out) != want {
							t.Fatalf("guest %s = %q, %v", path, out, err)
						}
					}
					// Changing the default after upgrade must not redirect the next boot.
					p.cfg.BaseKernelPath = "/different/default"
					p.applyConfiguredBootArtifacts(&upgraded)
					data, err := os.ReadFile(upgraded.KernelPath)
					if err != nil || string(data) != "new kernel" {
						t.Fatalf("pinned kernel: %q %v", data, err)
					}
				}
				data, err := os.ReadFile(neighbor.KernelPath)
				if err != nil || string(data) != "old kernel" {
					t.Fatalf("neighbor's kernel changed: %q %v", data, err)
				}
				for i := 0; i < 2; i++ {
					rolled, err := p.UpgradeKernel(ctx, inst.Name, true)
					if err != nil {
						t.Fatal(err)
					}
					after, _ := diskDigest(ctx, inst.RootFSPath)
					data, err := os.ReadFile(rolled.KernelPath)
					if after != before || err != nil || string(data) != "old kernel" {
						t.Fatalf("rollback did not restore exact pair: disk=%v kernel=%q err=%v", after == before, data, err)
					}
					if rolled.KindName() != kind || rolled.ImageName() != inst.ImageName() || rolled.ID != inst.ID {
						t.Fatal("rollback changed instance identity or image selection")
					}
					if _, err := client.ResolveBootArtifacts(ctx, vmrunner.StartRequest{Name: inst.Name, KernelPath: rolled.KernelPath, InitrdPath: rolled.InitrdPath}); err != nil {
						t.Fatalf("runner rejected rollback boot pair: %v", err)
					}
					if inst.InitrdPath != "" {
						data, err := os.ReadFile(rolled.InitrdPath)
						if err != nil || string(data) != "old initrd" {
							t.Fatalf("rollback initrd: %q %v", data, err)
						}
					}
					if err := p.upgradeFence(inst.Name); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}
