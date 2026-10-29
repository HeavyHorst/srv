package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"srv/internal/config"
	"srv/internal/model"
)

// SRV_TEST_BTRFS_DIR must name a disposable writable btrfs directory; this test
// creates and removes only its own uniquely named subtree there.
func TestSnapshotBundleBtrfsIntegration(t *testing.T) {
	base := os.Getenv("SRV_TEST_BTRFS_DIR")
	if base == "" {
		t.Skip("set SRV_TEST_BTRFS_DIR on a disposable btrfs filesystem")
	}
	dir, err := os.MkdirTemp(base, "srv-bundle-test-")
	if err != nil {
		t.Fatal(err)
	}
	var volumes []string
	t.Cleanup(func() {
		for i := len(volumes) - 1; i >= 0; i-- {
			if output, err := exec.Command("btrfs", "subvolume", "delete", volumes[i]).CombinedOutput(); err != nil {
				t.Errorf("cleanup: %v: %s", err, output)
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	create := func(path string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command("btrfs", "subvolume", "create", path).CombinedOutput(); err != nil {
			t.Fatalf("create: %v: %s", err, output)
		}
		volumes = append(volumes, path)
	}
	root := filepath.Join(dir, "data")
	create(root)
	archive := filepath.Join(root, ".snapshots")
	create(archive)
	disks := []string{"instances/vm/disk", "instances/orb/disk", "templates/owner/template/disk", "backups/vm/backup/disk", "image-bases/" + strings.Repeat("a", 64) + "/disk"}
	for _, disk := range disks {
		create(filepath.Join(root, disk))
		if err := os.WriteFile(filepath.Join(root, disk, "rootfs.img"), []byte(disk), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	legacyObjects := []string{"instances/legacy", "templates/owner/legacy", "backups/vm/legacy"}
	for _, object := range legacyObjects {
		if err := os.MkdirAll(filepath.Join(root, object), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, object, "rootfs.img"), []byte(object), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/run/nonexistent/firecracker.sock", filepath.Join(root, "instances/vm/firecracker.sock")); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		paths, err := inventorySnapshotDisks(root, snapshotSubvolume)
		if err != nil {
			t.Fatal(err)
		}
		info, err := createSnapshotBundle(context.Background(), root, archive, paths, time.Now().UTC(), createReadonlyBtrfsSnapshot, syncPathFilesystem, snapshotUUID)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := os.ReadFile(filepath.Join(info.Path, "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		var m snapshotManifest
		if err := json.Unmarshal(payload, &m); err != nil {
			t.Fatal(err)
		}
		for _, component := range m.Components {
			path := filepath.Join(info.Path, component.Path)
			volumes = append(volumes, path)
			if err := ensureBtrfsSubvolume(path); err != nil {
				t.Fatal(err)
			}
			id, err := snapshotUUID(context.Background(), path)
			if err != nil || id != component.SnapshotUUID || component.SourceUUID == id {
				t.Fatalf("snapshot UUID mismatch: %q %v", id, err)
			}
			if output, err := exec.Command("btrfs", "property", "get", "-ts", path, "ro").CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "ro=true" {
				t.Fatalf("not readonly: %s %v", output, err)
			}
			if component.Source == "." {
				entries, err := os.ReadDir(filepath.Join(path, ".snapshots"))
				if err != nil || len(entries) != 0 {
					t.Fatalf("recursive archive capture: %v %v", entries, err)
				}
				for _, object := range legacyObjects {
					data, err := os.ReadFile(filepath.Join(path, object, "rootfs.img"))
					if err != nil || string(data) != object {
						t.Fatalf("missing legacy disk: %s %v", object, err)
					}
				}
				continue
			}
			data, err := os.ReadFile(filepath.Join(path, "rootfs.img"))
			if err != nil || string(data) != component.Source {
				t.Fatalf("disk mismatch: %q %v", data, err)
			}
		}
	}
}

func TestSnapshotInventory(t *testing.T) {
	for _, scenario := range []string{"complete", "missing-disk", "plain-disk", "missing-image", "unknown", "nested", "symlink", "archive-plain", "base-stage", "base-legacy"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			disks := []string{"backups/vm/backup/disk", "image-bases/" + strings.Repeat("a", 64) + "/disk", "instances/orb/disk", "instances/vm/disk", "templates/owner/template/disk"}
			subvols := map[string]bool{".snapshots": true}
			for _, disk := range disks {
				subvols[disk] = true
				if err := os.MkdirAll(filepath.Join(root, disk), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, disk, "rootfs.img"), []byte("disk"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Join(root, ".snapshots/old/unknown"), 0o700); err != nil {
				t.Fatal(err)
			}
			subvols[".snapshots/old/unknown"] = true
			switch scenario {
			case "missing-disk":
				if err := os.RemoveAll(filepath.Join(root, disks[0])); err != nil {
					t.Fatal(err)
				}
			case "plain-disk":
				delete(subvols, disks[0])
			case "missing-image":
				if err := os.Remove(filepath.Join(root, disks[0], "rootfs.img")); err != nil {
					t.Fatal(err)
				}
			case "unknown", "nested":
				path := "unknown"
				if scenario == "nested" {
					path = disks[0] + "/nested"
				}
				subvols[path] = true
				if err := os.MkdirAll(filepath.Join(root, path), 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("vm", filepath.Join(root, "instances/link")); err != nil {
					t.Fatal(err)
				}
			case "archive-plain":
				delete(subvols, ".snapshots")
			case "base-stage":
				if err := os.Mkdir(filepath.Join(root, "image-bases", ".base-pending"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "base-legacy":
				baseDisk := filepath.Join(root, disks[1])
				if err := os.RemoveAll(baseDisk); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(filepath.Dir(baseDisk), "rootfs.img"), []byte("legacy base"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := inventorySnapshotDisks(root, func(path string) (bool, error) {
				if _, err := os.Lstat(path); err != nil {
					return false, err
				}
				rel, err := filepath.Rel(root, path)
				return subvols[rel], err
			})
			if scenario != "complete" {
				if err == nil {
					t.Fatalf("inventory accepted %s: %v", scenario, got)
				}
			} else if err != nil || !reflect.DeepEqual(got, disks) {
				t.Fatalf("inventory = %v, %v", got, err)
			}
		})
	}
}

func TestSnapshotLegacyInventory(t *testing.T) {
	for _, object := range []string{"instances/vm", "instances/disk", "templates/owner/template", "backups/vm/backup"} {
		for _, scenario := range []string{"legacy", "canonical", "dual", "missing", "symlink-image", "directory-image", "empty-image", "journal", "stage", "disk-symlink"} {
			t.Run(object+"/"+scenario, func(t *testing.T) {
				root := t.TempDir()
				dir := filepath.Join(root, object)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				legacy := filepath.Join(dir, "rootfs.img")
				if err := os.WriteFile(legacy, []byte("legacy"), 0o600); err != nil {
					t.Fatal(err)
				}
				// Runtime socket symlinks, including dangling links, are not storage.
				if err := os.Symlink("/run/nonexistent/firecracker.sock", filepath.Join(dir, "firecracker.sock")); err != nil {
					t.Fatal(err)
				}
				disk := filepath.Join(dir, "disk")
				wantImage := legacy
				switch scenario {
				case "canonical", "dual":
					if err := os.Mkdir(disk, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(disk, "rootfs.img"), []byte("canonical"), 0o600); err != nil {
						t.Fatal(err)
					}
					if scenario == "canonical" {
						if err := os.Remove(legacy); err != nil {
							t.Fatal(err)
						}
						wantImage = filepath.Join(disk, "rootfs.img")
					}
				case "missing", "symlink-image", "directory-image", "empty-image":
					if err := os.Remove(legacy); err != nil {
						t.Fatal(err)
					}
					switch scenario {
					case "symlink-image":
						if err := os.Symlink("firecracker.sock", legacy); err != nil {
							t.Fatal(err)
						}
					case "directory-image":
						if err := os.Mkdir(legacy, 0o700); err != nil {
							t.Fatal(err)
						}
					case "empty-image":
						if err := os.WriteFile(legacy, nil, 0o600); err != nil {
							t.Fatal(err)
						}
					}
				case "journal":
					if err := os.WriteFile(filepath.Join(dir, "disk-migration.json"), []byte("{}"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "stage":
					if err := os.Mkdir(filepath.Join(dir, ".disk-migration-pending"), 0o700); err != nil {
						t.Fatal(err)
					}
				case "disk-symlink":
					if err := os.Symlink("/missing", disk); err != nil {
						t.Fatal(err)
					}
				}
				probe := func(path string) (bool, error) { return path == disk && scenario != "disk-symlink", nil }
				paths, err := inventorySnapshotDisks(root, probe)
				image, imageErr := snapshotDiskImage(dir, true, probe)
				if scenario == "legacy" || scenario == "canonical" {
					if err != nil || imageErr != nil || image != wantImage {
						t.Fatalf("inventory %v, image %q: %v, %v", paths, image, err, imageErr)
					}
					if scenario == "legacy" && len(paths) != 0 {
						t.Fatal("legacy file must be captured in root")
					}
					if scenario == "canonical" && !reflect.DeepEqual(paths, []string{object + "/disk"}) {
						t.Fatal(paths)
					}
				} else if err == nil || imageErr == nil {
					t.Fatalf("accepted invalid layout: %v %v", err, imageErr)
				}
			})
		}
	}
}

func TestSnapshotBundlePublication(t *testing.T) {
	for _, fail := range []string{"", "root", "disk-000000", "sync-before", "sync-stage", "sync-published", "identity", "source-changed"} {
		t.Run("failure="+fail, func(t *testing.T) {
			root, archive := t.TempDir(), t.TempDir()
			created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			final := filepath.Join(archive, created.Format("20060102T150405.000000000Z"))
			var calls []string
			identityCalls := make(map[string]int)
			info, err := createSnapshotBundle(context.Background(), root, archive, []string{"instances/vm/disk"}, created,
				func(_ context.Context, source, target string) error {
					if _, err := os.Stat(final); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("published before all components")
					}
					calls = append(calls, filepath.Base(target))
					if filepath.Base(target) == fail {
						return errors.New("snapshot failure")
					}
					return os.Mkdir(target, 0o700)
				}, func(path string) error {
					if (fail == "sync-before" && path == root) || (fail == "sync-stage" && strings.Contains(path, ".incomplete-")) || (fail == "sync-published" && path == archive) {
						return errors.New("sync failure")
					}
					return nil
				}, func(_ context.Context, path string) (string, error) {
					identityCalls[path]++
					if fail == "identity" {
						return "", errors.New("identity failure")
					}
					if fail == "source-changed" && identityCalls[path] > 1 {
						return "changed", nil
					}
					if strings.Contains(path, ".incomplete-") {
						return "snapshot-" + filepath.Base(path), nil
					}
					return "source-" + filepath.Base(path), nil
				})
			if fail != "" {
				if err == nil || info.ID != "" {
					t.Fatal("failure reported success")
				}
				if fail != "sync-published" {
					if _, err := os.Stat(final); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("incomplete bundle published")
					}
					stages, _ := filepath.Glob(filepath.Join(archive, ".incomplete-*"))
					if len(stages) != 1 {
						t.Fatalf("stages: %v", stages)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, []string{"root", "disk-000000"}) {
				t.Fatal(calls)
			}
			payload, err := os.ReadFile(filepath.Join(info.Path, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			var manifest snapshotManifest
			if err := json.Unmarshal(payload, &manifest); err != nil {
				t.Fatal(err)
			}
			want := []snapshotComponent{{Source: ".", Path: "root", SourceUUID: "source-" + filepath.Base(root), SnapshotUUID: "snapshot-root"}, {Source: "instances/vm/disk", Path: "disk-000000", SourceUUID: "source-disk", SnapshotUUID: "snapshot-disk-000000"}}
			if manifest.Version != 1 || manifest.ID != info.ID || !reflect.DeepEqual(manifest.Components, want) {
				t.Fatalf("manifest: %+v", manifest)
			}
			for _, c := range manifest.Components {
				if _, err := os.Stat(filepath.Join(info.Path, c.Path)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestAuthorizeSnapshotRequiresAdmin(t *testing.T) {
	app := &App{cfg: config.Config{AdminUsers: []string{"ops@example.com"}}}

	allowed, reason := app.authorize(model.Actor{UserLogin: "alice@example.com"}, "snapshot")
	if allowed {
		t.Fatal("authorize(snapshot) unexpectedly allowed non-admin user")
	}
	if reason != "alice@example.com is not in SRV_ADMIN_USERS" {
		t.Fatalf("authorize(snapshot) reason = %q", reason)
	}

	allowed, reason = app.authorize(model.Actor{UserLogin: "ops@example.com"}, "snapshot")
	if !allowed {
		t.Fatal("authorize(snapshot) denied admin user")
	}
	if reason != "ops@example.com allowed to run snapshot as admin" {
		t.Fatalf("authorize(snapshot) reason = %q", reason)
	}
}

func TestAuthorizeSnapshotRequiresAllowlistMembershipWhenEnabled(t *testing.T) {
	app := &App{cfg: config.Config{AllowedUsers: []string{"alice@example.com"}, AdminUsers: []string{"ops@example.com"}}}

	allowed, reason := app.authorize(model.Actor{UserLogin: "ops@example.com"}, "snapshot")
	if allowed {
		t.Fatal("authorize(snapshot) unexpectedly allowed admin outside SRV_ALLOWED_USERS")
	}
	if reason != "ops@example.com is not in SRV_ALLOWED_USERS" {
		t.Fatalf("authorize(snapshot) reason = %q", reason)
	}
}

func TestCommandGateBlocksNewCommandsDuringSnapshotBarrier(t *testing.T) {
	var app App

	first, err := app.beginCommand()
	if err != nil {
		t.Fatalf("beginCommand(first): %v", err)
	}
	snapshot, err := app.beginCommand()
	if err != nil {
		first.Release()
		t.Fatalf("beginCommand(snapshot): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	promoteDone := make(chan error, 1)
	go func() {
		promoteDone <- snapshot.PromoteToSnapshot(ctx)
	}()

	waitForSnapshotBarrier(t, &app)

	if lease, err := app.beginCommand(); !errors.Is(err, errSnapshotInProgress) {
		if lease != nil {
			lease.Release()
		}
		t.Fatalf("beginCommand() during snapshot barrier error = %v, want %v", err, errSnapshotInProgress)
	}
	select {
	case err := <-promoteDone:
		t.Fatalf("PromoteToSnapshot() returned early: %v", err)
	default:
	}

	first.Release()
	select {
	case err := <-promoteDone:
		if err != nil {
			t.Fatalf("PromoteToSnapshot(): %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PromoteToSnapshot() did not finish after in-flight command drained")
	}

	if lease, err := app.beginCommand(); !errors.Is(err, errSnapshotInProgress) {
		if lease != nil {
			lease.Release()
		}
		t.Fatalf("beginCommand() after snapshot promotion error = %v, want %v", err, errSnapshotInProgress)
	}

	snapshot.Release()
	next, err := app.beginCommand()
	if err != nil {
		t.Fatalf("beginCommand() after snapshot release: %v", err)
	}
	next.Release()
}

func TestPromoteToSnapshotContextCancelRestoresGate(t *testing.T) {
	var app App

	first, err := app.beginCommand()
	if err != nil {
		t.Fatalf("beginCommand(first): %v", err)
	}
	snapshot, err := app.beginCommand()
	if err != nil {
		first.Release()
		t.Fatalf("beginCommand(snapshot): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- snapshot.PromoteToSnapshot(ctx)
	}()

	waitForSnapshotBarrier(t, &app)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("PromoteToSnapshot() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("PromoteToSnapshot() did not return after context cancellation")
	}

	next, err := app.beginCommand()
	if err != nil {
		t.Fatalf("beginCommand() after cancellation: %v", err)
	}
	next.Release()
	snapshot.Release()
	first.Release()
}

func waitForSnapshotBarrier(t *testing.T, app *App) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		lease, err := app.beginCommand()
		if errors.Is(err, errSnapshotInProgress) {
			return
		}
		if err != nil {
			t.Fatalf("beginCommand() while waiting for snapshot barrier: %v", err)
		}
		lease.Release()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("snapshot barrier never became active")
}
