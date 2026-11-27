package provision

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"srv/internal/model"
)

func TestProjectTemplateStripsAmpRuntimeIdentity(t *testing.T) {
	templateTestTools(t)
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), vmRunner: templateTestRunner{}}
	inst := provisionTestInstance(cfg, "source", model.StateStopped, time.Now())
	actor := model.Actor{UserLogin: inst.CreatedByUser}
	templateTestBootFiles(t, inst)
	files := map[string]string{"etc/machine-id": "source-machine"}
	var removed []string
	for _, home := range []string{"/root", "/home/user"} {
		for _, name := range []string{"headless.env", "headless-bootstrap.env"} {
			path := home + "/.config/amp/" + name
			files[path] = "AMP_THREAD_ID=source-thread\nAMP_API_KEY=source-token\n"
			removed = append(removed, path)
		}
	}
	kept := []string{
		"/etc/systemd/system/amp.slice",
		"/etc/systemd/system/amp-control.slice",
		"/etc/systemd/system/amp-workload.slice",
		"/etc/systemd/system/amp-headless.service",
		"/etc/systemd/system/amp-headless-env.service",
		"/etc/systemd/system/amp-headless-env.path",
		"/usr/local/bin/amp-run-workload",
		"/usr/local/bin/amp-headless-apply-env",
		"/usr/local/bin/amp-headless-update-binary",
		"/etc/sudoers.d/amp-systemd-run",
		"/root/.config/amp/settings.json",
		"/home/user/.config/amp/settings.json",
		"/home/user/project/prepared",
	}
	for _, path := range kept {
		files[path] = "prepared-" + path
	}
	// Seed a file in wants so mkfs creates the directory before debugfs adds
	// the same symlinks produced by systemctl enable in a running source Orb.
	files["/etc/systemd/system/multi-user.target.wants/keep"] = "keep"
	templateTestImage(t, inst.RootFSPath, files)
	const wants = "/etc/systemd/system/multi-user.target.wants"
	const executor = wants + "/amp-headless.service"
	const watcher = wants + "/amp-headless-env.path"
	sourceDisk := templateDisk{ctx: ctx, path: inst.RootFSPath}
	for _, path := range []string{executor, watcher} {
		target := "../" + filepath.Base(path)
		if _, err := sourceDisk.run("symlink \"" + path + "\" \"" + target + "\""); err != nil {
			t.Fatal(err)
		}
		stat, err := sourceDisk.command("stat", path)
		if err != nil || !strings.Contains(stat, "Type: symlink") || !strings.Contains(stat, `Fast link dest: "`+target+`"`) {
			t.Fatalf("fixture %s is not the expected symlink: %s (%v)", path, stat, err)
		}
	}
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
	clone := templateDisk{ctx: ctx, path: filepath.Join(dir, "disk", "rootfs.img")}
	for _, path := range append(removed, executor) {
		entries, err := clone.list(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.name == filepath.Base(path) {
				t.Errorf("source Amp runtime identity retained at %s", path)
			}
		}
	}
	for _, path := range kept {
		out, err := clone.command("cat", path)
		if err != nil || out != files[path] {
			t.Errorf("prepared file changed %s: %q (%v)", path, out, err)
		}
	}
	stat, err := clone.command("stat", watcher)
	if err != nil || !strings.Contains(stat, "Type: symlink") || !strings.Contains(stat, `Fast link dest: "../amp-headless-env.path"`) {
		t.Errorf("Amp environment watcher enablement changed: %s (%v)", stat, err)
	}
	source, err = os.ReadFile(inst.RootFSPath)
	if err != nil || sha256.Sum256(source) != before {
		t.Fatal("source disk modified", err)
	}
}
