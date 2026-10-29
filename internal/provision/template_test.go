package provision

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"srv/internal/host"
	"srv/internal/model"
	"srv/internal/storage"
	"tailscale.com/client/tailscale/v2"
)

func templateTestTools(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", os.Getenv("PATH")+":/usr/sbin:/sbin")
	for _, tool := range []string{"mkfs.ext4", "debugfs", "e2fsck"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	old := reflinkCloneFile
	t.Cleanup(func() { reflinkCloneFile = old })
	reflinkCloneFile = func(ctx context.Context, src, dest string) error {
		return exec.CommandContext(ctx, "cp", "--sparse=always", src, dest).Run()
	}
}

func templateTestImage(t *testing.T, image string, files map[string]string) {
	t.Helper()
	root := t.TempDir()
	for path, value := range files {
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(image), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(image)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(32 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", "-d", root, image).CombinedOutput(); err != nil {
		t.Fatalf("mkfs: %v %s", err, out)
	}
}

func TestProjectTemplateLifecycleAndIdentityCleanup(t *testing.T) {
	templateTestTools(t)
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), vmRunner: noopVMRunner{}}
	inst := provisionTestInstance(cfg, "source", model.StateStopped, time.Now())
	actor := model.Actor{UserLogin: inst.CreatedByUser}
	removed := []string{"etc/machine-id", "var/lib/dbus/machine-id", "var/lib/tailscale/tailscaled.state", "var/lib/tailscale/ssh/ssh_host_ed25519_key", "var/lib/tailscale/My Notes Grüße", "var/lib/srv/bootstrap.done", "var/lib/systemd/random-seed", "root/.config/amp/secrets.json", "root/.local/share/amp/session.json", "home/user/.local/share/amp/device-id.json", "home/user/.local/share/amp/secrets.json"}
	kept := []string{"home/user/.amp/bin/amp", "root/.amp/bin/amp", "home/user/.config/amp/settings.json", "home/user/.config/amp/skills/custom/SKILL.md", "home/user/.local/share/amp/prepared-state", "home/user/My Notes Grüße", "home/user/project/.agents/setup", "home/user/project/code.go", "etc/hostname"}
	files := map[string]string{}
	for _, path := range append(removed, kept...) {
		files[path] = "payload-" + path
	}
	templateTestImage(t, inst.RootFSPath, files)
	source, err := os.ReadFile(inst.RootFSPath)
	if err != nil {
		t.Fatal(err)
	}
	before := sha256.Sum256(source)
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if err := p.CreateTemplate(ctx, inst.Name, "prepared", actor); err != nil {
		t.Fatal(err)
	}
	dir, _, err := p.readTemplate("prepared", actor)
	if err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(dir, "disk", "rootfs.img")
	for _, path := range removed {
		out, _ := exec.Command("debugfs", "-R", "stat \"/"+path+"\"", disk).CombinedOutput()
		if !strings.Contains(string(out), "File not found") {
			t.Errorf("identity retained at %s: %s", path, out)
		}
	}
	for _, path := range kept {
		out, err := exec.Command("debugfs", "-R", "cat \"/"+path+"\"", disk).Output()
		if err != nil || string(out) != files[path] {
			t.Errorf("prepared file lost %s: %s (%v)", path, out, err)
		}
	}
	source, err = os.ReadFile(inst.RootFSPath)
	if err != nil || sha256.Sum256(source) != before {
		t.Fatal("source modified", err)
	}
	snapshot, err := os.ReadFile(disk)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.CreateTemplate(ctx, inst.Name, "prepared", actor); err != nil {
		t.Fatal("retry", err)
	}
	again, err := os.ReadFile(disk)
	if err != nil || sha256.Sum256(again) != sha256.Sum256(snapshot) {
		t.Fatal("retry changed snapshot")
	}
	if err := os.Rename(disk, disk+".missing"); err != nil {
		t.Fatal(err)
	}
	if err := p.CreateTemplate(ctx, inst.Name, "prepared", actor); err == nil {
		t.Fatal("retry reported success for a missing published disk")
	}
	if err := os.Rename(disk+".missing", disk); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+".missing"); err != nil {
		t.Fatal(err)
	}
	if err := p.VerifyTemplate(ctx, inst.Name, "prepared", actor); err == nil {
		t.Fatal("verify recreated a missing published template directory")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("verify created a directory")
	}
	if err := os.Rename(dir+".missing", dir); err != nil {
		t.Fatal(err)
	}
	bob := model.Actor{UserLogin: "bob@example.com"}
	if err := p.CreateTemplate(ctx, inst.Name, "other", bob); err == nil {
		t.Fatal("foreign source accepted")
	}
	if _, _, err := p.readTemplate("prepared", bob); err == nil {
		t.Fatal("foreign template visible")
	}
	if err := p.DeleteTemplate(ctx, "prepared", bob); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(disk); err != nil {
		t.Fatal("foreign delete removed template")
	}
	other := provisionTestInstance(cfg, "other", model.StateStopped, time.Now())
	if err := st.CreateInstance(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := p.CreateTemplate(ctx, other.Name, "prepared", actor); err == nil {
		t.Fatal("source collision accepted")
	}
	if _, err := p.PrepareMigration(ctx, other.Name, "destination", "transfer"); err != nil {
		t.Fatal(err)
	}
	if err := p.CreateTemplate(ctx, other.Name, "fenced", actor); err == nil || !strings.Contains(err.Error(), "fenced") {
		t.Fatalf("migration fence: %v", err)
	}
	inst.State = model.StateReady
	if err := st.UpdateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if err := p.CreateTemplate(ctx, inst.Name, "running", actor); err == nil {
		t.Fatal("running source accepted")
	}
	inst.State = model.StateDeleted
	if err := st.UpdateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(inst.RootFSPath)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.readTemplate("prepared", actor); err != nil {
		t.Fatal("source deletion lost template", err)
	}
	if err := p.DeleteTemplate(ctx, "prepared", actor); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteTemplate(ctx, "prepared", actor); err != nil {
		t.Fatal("repeat delete", err)
	}
	if _, _, err := p.readTemplate("../prepared", actor); err == nil {
		t.Fatal("invalid ID accepted")
	}
}

func TestTemplateSanitizerFailsClosed(t *testing.T) {
	templateTestTools(t)
	image := filepath.Join(t.TempDir(), "rootfs.img")
	templateTestImage(t, image, map[string]string{"etc/machine-id": "identity", "var/lib/tailscale/bad\"name": "secret"})
	if err := sanitizeTemplateDisk(context.Background(), image); err == nil {
		t.Fatal("unsafe directory entry accepted")
	}
	image = filepath.Join(t.TempDir(), "rootfs.img")
	templateTestImage(t, image, map[string]string{"etc/machine-id": "identity", "root/placeholder": "x"})
	if out, err := exec.Command("debugfs", "-w", "-R", "symlink /root/.config /etc", image).CombinedOutput(); err != nil {
		t.Fatalf("symlink: %s %v", out, err)
	}
	if err := sanitizeTemplateDisk(context.Background(), image); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	bad := filepath.Join(t.TempDir(), "bad.img")
	if err := os.WriteFile(bad, []byte("not ext4"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := templateDisk{ctx: context.Background(), path: bad}
	if _, err := d.command("ls -p", "/"); err == nil {
		t.Fatal("debugfs error with zero exit code accepted")
	}
}

func TestCreateUsesOwnerTemplateWithFreshInstanceIdentity(t *testing.T) {
	templateTestTools(t)
	ctx := context.Background()
	kernel := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := loadProvisionTestConfig(t, map[string]string{
		"SRV_BASE_KERNEL": kernel, "SRV_GUEST_AUTH_TAGS": "tag:microvm",
		"TS_TAILNET": "example.com", "TS_CLIENT_SECRET": "test-secret",
	})
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, store: st, tsClient: &tailscale.Client{}, networkHelper: noopNetworkHelper{}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		readHostMemoryBytes: func() (int64, error) { return 64 << 30, nil },
		readFilesystemUsage: func(string) (host.FilesystemUsage, error) {
			return host.FilesystemUsage{Total: 128 << 30, Used: 1 << 30, Available: 127 << 30}, nil
		},
	}
	inst := provisionTestInstance(cfg, "source", model.StateStopped, time.Now())
	actor := model.Actor{UserLogin: inst.CreatedByUser}
	templateTestImage(t, inst.RootFSPath, map[string]string{"etc/machine-id": "old", "home/user/project/prepared": "prepared"})
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if err := p.CreateTemplate(ctx, inst.Name, "prepared", actor); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(ctx, "foreign", model.Actor{UserLogin: "bob"}, CreateOptions{Template: "prepared"}); err == nil {
		t.Fatal("foreign create accepted")
	}
	oldLoop := loopDevicesForPath
	t.Cleanup(func() { loopDevicesForPath = oldLoop })
	loopDevicesForPath = func(string) (string, error) { return "", nil }
	dir, _, err := p.readTemplate("prepared", actor)
	if err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(dir, "disk", "rootfs.img")
	snapshotDisk := storage.SnapshotSubvolume
	t.Cleanup(func() { storage.SnapshotSubvolume = snapshotDisk })
	stop := errors.New("stop test after observing clone, before network and boot")
	observed := false
	storage.SnapshotSubvolume = func(ctx context.Context, src, dest string, readonly bool) error {
		if src != filepath.Dir(disk) {
			t.Fatalf("cloned %q, want template %q", src, disk)
		}
		if readonly {
			t.Fatal("new instance snapshot must be writable")
		}
		if err := snapshotDisk(ctx, src, dest, readonly); err != nil {
			return err
		}
		if dest == filepath.Join(cfg.InstancesDir(), "fresh", "disk") {
			dest = filepath.Join(dest, "rootfs.img")
			observed = true
			out, err := exec.Command("debugfs", "-R", "cat /home/user/project/prepared", dest).Output()
			if err != nil || string(out) != "prepared" {
				t.Fatalf("prepared project missing: %q %v", out, err)
			}
			out, _ = exec.Command("debugfs", "-R", "stat /etc/machine-id", dest).CombinedOutput()
			if !strings.Contains(string(out), "File not found") {
				t.Fatal("old machine identity cloned")
			}
			return stop
		}
		return nil
	}
	fresh, err := p.Create(ctx, "fresh", actor, CreateOptions{Template: "prepared", RootFSSizeBytes: 32 << 20})
	if !errors.Is(err, stop) || !observed {
		t.Fatalf("clone not reached: %v", err)
	}
	if fresh.ID == "" || fresh.ID == inst.ID || fresh.GuestMAC == inst.GuestMAC || fresh.NetworkCIDR == inst.NetworkCIDR || fresh.TailnetNodeID != "" {
		t.Fatalf("identity reused: %+v", fresh)
	}
	if fresh.KernelPath != kernel || fresh.Image != "arch" {
		t.Fatalf("image contract changed: %+v", fresh)
	}
	// A different catalog identity cannot consume this template, even if its
	// rootfs is otherwise bootable. No external services are reached.
	mismatch := inst
	mismatch.Image = "debian"
	if err := st.UpdateInstance(ctx, mismatch); err != nil {
		t.Fatal(err)
	}
	storage.SnapshotSubvolume = snapshotDisk
	if err := p.CreateTemplate(ctx, inst.Name, "debian", actor); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(ctx, "wrong-image", actor, CreateOptions{Template: "debian"}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("image mismatch accepted: %v", err)
	}
}

func TestTemplateCancellationCleansStaging(t *testing.T) {
	templateTestTools(t)
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	inst := provisionTestInstance(cfg, "source", model.StateStopped, time.Now())
	actor := model.Actor{UserLogin: inst.CreatedByUser}
	templateTestImage(t, inst.RootFSPath, map[string]string{"etc/machine-id": "original"})
	if err := st.CreateInstance(context.Background(), inst); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "e2fsck"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := p.CreateTemplate(ctx, inst.Name, "cancelled", actor)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("filesystem check did not respect cancellation: %v (%s)", err, time.Since(start))
	}
	dir, err := p.templatePath("cancelled", actor)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancelled snapshot retained staging/publication: %v %v", entries, err)
	}
}

func TestDeleteTemplateResumesUnpublishedCleanup(t *testing.T) {
	for _, manifestRemoved := range []bool{false, true} {
		name := "manifest-retained"
		if manifestRemoved {
			name = "manifest-removed"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg := loadProvisionTestConfig(t, nil)
			p := &Provisioner{cfg: cfg}
			actor := model.Actor{UserLogin: "alice@example.com"}
			dir, err := p.templatePath("prepared", actor)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			manifest := []byte(`{"Version":2,"ID":"prepared","Owner":"alice@example.com","SourceID":"source-id","Image":"debian-13"}`)
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0400); err != nil {
				t.Fatal(err)
			}
			// A published sibling must not be reclaimed with unpublished trash.
			retained := filepath.Join(filepath.Dir(dir), "retained")
			if err := os.Mkdir(retained, 0700); err != nil {
				t.Fatal(err)
			}
			oldRemove := removePathAll
			t.Cleanup(func() { removePathAll = oldRemove })
			removePathAll = func(path string) error {
				if manifestRemoved {
					if err := os.Remove(filepath.Join(path, "manifest.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
						return err
					}
				}
				return os.ErrPermission
			}
			if err := p.DeleteTemplate(ctx, "prepared", actor); !errors.Is(err, os.ErrPermission) {
				t.Fatalf("initial deletion: %v", err)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed deletion left template published", err)
			}
			if err := p.DeleteTemplate(ctx, "prepared", actor); !errors.Is(err, os.ErrPermission) {
				t.Fatalf("retry hid outstanding cleanup failure: %v", err)
			}
			removePathAll = oldRemove
			if err := p.DeleteTemplate(ctx, "prepared", model.Actor{UserLogin: "bob@example.com"}); err != nil {
				t.Fatal(err)
			}
			trash, err := filepath.Glob(filepath.Join(filepath.Dir(dir), ".deleted-*"))
			if err != nil || len(trash) != 1 {
				t.Fatal("foreign deletion affected owner's trash", trash, err)
			}
			if err := p.DeleteTemplate(ctx, "prepared", actor); err != nil {
				t.Fatal("resume deletion", err)
			}
			if _, err := os.Stat(trash[0]); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("retry left unpublished trash", err)
			}
			if _, err := os.Stat(retained); err != nil {
				t.Fatal("retry removed published sibling", err)
			}
		})
	}
}
