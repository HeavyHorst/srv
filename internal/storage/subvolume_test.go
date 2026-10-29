package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestSubvolumeCommandsAndIdentity(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	oldCommand, oldRequire, oldInspect := Command, RequireBtrfs, InspectSubvolume
	t.Cleanup(func() { Command, RequireBtrfs, InspectSubvolume = oldCommand, oldRequire, oldInspect })
	RequireBtrfs = ValidatePath
	InspectSubvolume = func(context.Context, string) (SubvolumeIdentity, error) {
		return SubvolumeIdentity{UUID: "01234567-89ab-cdef-0123-456789abcdef", ID: "256"}, nil
	}
	var calls [][]string
	Command = func(ctx context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if reflect.DeepEqual(args[:2], []string{"property", "get"}) {
			return []byte("ro=false\n"), nil
		}
		return nil, nil
	}
	source := filepath.Join(dir, "disk")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	id, err := InspectSubvolume(ctx, source)
	if err != nil || id.ID != "256" {
		t.Fatalf("identity: %+v %v", id, err)
	}
	dest := filepath.Join(dir, "snapshot")
	if err := SnapshotSubvolume(ctx, source, dest, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls[len(calls)-1], []string{"subvolume", "snapshot", "-r", source, dest}) {
		t.Fatal(calls)
	}
	before := len(calls)
	if err := DeleteSubvolume(ctx, source, SubvolumeIdentity{UUID: "wrong", ID: "256"}); err == nil {
		t.Fatal("wrong identity deleted")
	}
	if len(calls) != before {
		t.Fatal("identity mismatch invoked delete", calls)
	}
	if err := DeleteSubvolume(ctx, source, id); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls[len(calls)-1], []string{"subvolume", "delete", source}) {
		t.Fatal(calls)
	}
	Command = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("permission denied") }
	if err := DeleteSubvolume(ctx, source, id); err == nil {
		t.Fatal("inspection failure accepted")
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("command failure removed source", err)
	}
}

func TestSubvolumeInfoABI(t *testing.T) {
	var info subvolumeInfo
	if unsafe.Sizeof(info) != 504 || unsafe.Offsetof(info.UUID) != 296 || getSubvolumeInfo != 0x81f8943c {
		t.Fatal("GET_SUBVOL_INFO layout does not match linux/btrfs.h")
	}
	var fs filesystemInfo
	if unsafe.Sizeof(fs) != 1024 || unsafe.Offsetof(fs.UUID) != 16 || getFilesystemInfo != 0x8400941f {
		t.Fatal("FS_INFO layout does not match linux/btrfs.h")
	}
}

func TestOpenNoSymlinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload")
	if err := os.WriteFile(path, []byte("original bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenNoSymlinks(path, unix.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 14)
	n, err := f.Read(buf)
	f.Close()
	if err != nil || string(buf[:n]) != "original bytes" {
		t.Fatalf("read: %q %v", buf[:n], err)
	}
	f, err = OpenNoSymlinks(path, unix.O_PATH)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	f.Close()
	if err != nil || !info.Mode().IsRegular() || info.Size() != 14 {
		t.Fatalf("O_PATH file: %v %v", info, err)
	}
	for _, target := range []string{path, dir} {
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		candidate := link
		if target == dir {
			candidate = filepath.Join(link, "payload")
		}
		for _, flags := range []int{unix.O_RDONLY, unix.O_PATH} {
			if f, err := OpenNoSymlinks(candidate, flags); err == nil {
				f.Close()
				t.Fatalf("followed symlink: %s (flags %d)", candidate, flags)
			}
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
	}
	for _, invalid := range []string{"relative", strings.Replace(path, "/payload", "/../payload", 1)} {
		if f, err := OpenNoSymlinks(invalid, unix.O_RDONLY); err == nil {
			f.Close()
			t.Fatalf("accepted unclean or relative path: %s", invalid)
		}
	}
}

func TestSubvolumeUnprivilegedIntegration(t *testing.T) {
	root := os.Getenv("SRV_TEST_BTRFS_DIR")
	if root == "" {
		t.Skip("set SRV_TEST_BTRFS_DIR to disposable writable btrfs storage")
	}
	if os.Geteuid() == 0 {
		t.Fatal("run as the unprivileged service UID to verify its permission contract")
	}
	dir, err := os.MkdirTemp(root, "storage-integration-")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t.Cleanup(func() {
		if err := RemoveTree(ctx, dir); err != nil {
			t.Error(err)
		}
	})
	disk := filepath.Join(dir, "disk")
	if err := CreateSubvolume(ctx, disk); err != nil {
		t.Fatal(err)
	}
	id, err := InspectSubvolume(ctx, disk)
	if err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(disk, "rootfs.img")
	if err := os.WriteFile(image, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	if containing, err := ContainingSubvolume(ctx, image); err != nil || containing != id {
		t.Fatalf("file subvolume identity: %+v %v", containing, err)
	}
	snapshot := filepath.Join(dir, "snapshot")
	if err := SnapshotSubvolume(ctx, disk, snapshot, true); err != nil {
		t.Fatal(err)
	}
	if err := RequireReadonly(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(image, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(snapshot, "rootfs.img")); err != nil || string(data) != "base" {
		t.Fatalf("snapshot contents: %q %v", data, err)
	}
	snapshotID, err := InspectSubvolume(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	wrongID := snapshotID
	wrongID.UUID = "wrong"
	if err := DeleteSubvolume(ctx, snapshot, wrongID); err == nil {
		t.Fatal("deleted snapshot with wrong identity")
	}
	if err := RequireReadonly(ctx, snapshot); err != nil {
		t.Fatal("identity refusal changed readonly snapshot", err)
	}
	// Deny removal at the parent, but still allow the snapshot owner to
	// change its flags. A failed delete must restore its readonly state.
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	deleteErr := DeleteSubvolume(ctx, snapshot, snapshotID)
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if deleteErr == nil {
		t.Fatal("deleted snapshot through unwritable parent")
	}
	if err := RequireReadonly(ctx, snapshot); err != nil {
		t.Fatal("failed deletion left snapshot writable", err)
	}
	retained := filepath.Join(dir, "retained")
	if err := SnapshotSubvolume(ctx, snapshot, retained, true); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSubvolume(ctx, snapshot, snapshotID); err != nil {
		t.Fatal("delete local readonly snapshot", err)
	}
	if _, err := os.Lstat(snapshot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted snapshot remains", err)
	}
	if err := RequireReadonly(ctx, retained); err != nil {
		t.Fatal("deletion changed retained snapshot", err)
	}
	if data, err := os.ReadFile(filepath.Join(retained, "rootfs.img")); err != nil || string(data) != "base" {
		t.Fatalf("retained snapshot contents: %q %v", data, err)
	}
	if err := DeleteSubvolume(ctx, disk, id); err != nil {
		t.Fatal(err)
	}
}

func TestStorageRejectsSymlinksAndNonBtrfs(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(filepath.Join(link, "child")); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	if err := CreateSubvolume(context.Background(), filepath.Join(link, "child")); err == nil {
		t.Fatal("symlink create accepted")
	}
	if err := RemoveTree(context.Background(), filepath.Join(link, "child")); err == nil {
		t.Fatal("symlink removal accepted")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	if uint64(fs.Type) != 0x9123683e && RequireBtrfs(dir) == nil {
		t.Fatal("non-btrfs backend accepted")
	}
}

func TestDiskValidationRejectsExtraFiles(t *testing.T) {
	dir := t.TempDir()
	old := InspectSubvolume
	t.Cleanup(func() { InspectSubvolume = old })
	InspectSubvolume = func(context.Context, string) (SubvolumeIdentity, error) {
		return SubvolumeIdentity{UUID: "disk", ID: "256"}, nil
	}
	if err := os.WriteFile(filepath.Join(dir, "rootfs.img"), []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateDiskSubvolume(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateDiskSubvolume(context.Background(), dir); err == nil {
		t.Fatal("runtime metadata accepted inside disk snapshot")
	}
}
