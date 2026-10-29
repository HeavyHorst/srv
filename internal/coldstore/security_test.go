package coldstore

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"srv/internal/storage"
)

func TestPrivateKeyPermissions(t *testing.T) {
	for _, mode := range []os.FileMode{0600, 0400, 0644, 0640, 0604, 0610, 0601} {
		lookup := func(p string) (os.FileInfo, error) {
			if p == "/keys/id" {
				return credentialInfo{mode: mode}, nil
			}
			return credentialInfo{mode: os.ModeDir | 0755}, nil
		}
		if err := validateCredential("/keys/id", lookup, true); (err == nil) != (mode&0077 == 0) {
			t.Fatalf("key mode %o: %v", mode, err)
		}
		if mode == 0644 {
			if err := validateCredential("/keys/id", lookup, false); err != nil {
				t.Fatal("0644 known_hosts rejected", err)
			}
		}
	}
}

func TestStorageAncestorTrust(t *testing.T) {
	for _, path := range []string{"/", "/storage", "/storage/root"} {
		for _, bad := range []credentialInfo{{mode: os.ModeDir | 0755, uid: 1000}, {mode: os.ModeDir | 0775}, {mode: os.ModeDir | os.ModeSticky | 0777}, {mode: os.ModeSymlink | 0755}} {
			lookup := func(p string) (os.FileInfo, error) {
				if p == path {
					return bad, nil
				}
				return credentialInfo{mode: os.ModeDir | 0755}, nil
			}
			if err := trustedRoot("/storage/root", lookup); err == nil {
				t.Fatalf("accepted untrusted ancestor %s: %+v", path, bad)
			}
		}
	}
	lookup := func(string) (os.FileInfo, error) { return credentialInfo{mode: os.ModeDir | 0755}, nil }
	if err := trustedRoot("/storage/root", lookup); err != nil {
		t.Fatal(err)
	}
}

func TestConfigValidation(t *testing.T) {
	if err := (Config{Timeout: -time.Second}).Validate(); err != nil {
		t.Fatal("disabled config rejected", err)
	}
	for _, c := range []Config{{Host: "host", Timeout: -time.Second}, {Host: "bad;host"}, {Host: "host", IdentityFile: "relative"}} {
		if c.Validate() == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	if os.Geteuid() != 0 {
		return // Remaining checks exercise actual root-owned credentials.
	}
	dir, err := os.MkdirTemp("/root", "coldstore-config-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	c := Config{Host: "host", IdentityFile: filepath.Join(dir, "key"), KnownHostsFile: filepath.Join(dir, "hosts")}
	for _, p := range []string{c.IdentityFile, c.KnownHostsFile} {
		if err := os.WriteFile(p, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := (Client{Config: c}).sshArgs(request{Operation: "stat", Object: Object{ID: testID}}); err == nil {
		t.Fatal("accepted real 0644 private key")
	}
	if err := os.Chmod(c.IdentityFile, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, name := range []string{"ssh", "btrfs"} {
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("missing %s not diagnosed: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDiscardAbsentFencesAndRejectsUnsafeObjects(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned namespace")
	}
	dir, err := os.MkdirTemp("/root", "coldstore-discard-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	old := storage.RequireBtrfs
	storage.RequireBtrfs = func(string) error { return nil }
	t.Cleanup(func() { storage.RequireBtrfs = old })
	ctx := context.Background()
	for _, ids := range [][]string{nil, {"../bad"}, {testID, testID}} {
		if DiscardObjects(ctx, dir, ids) == nil {
			t.Fatalf("accepted invalid discard set %v", ids)
		}
	}
	if err := DiscardObjects(ctx, dir, []string{testID}); err != nil {
		t.Fatal(err)
	}
	put, _ := encodeRequest(request{Operation: "put", Object: testObject()})
	if err := Serve(ctx, dir, put, strings.NewReader(""), io.Discard); err != errObjectDeleted {
		t.Fatalf("discard did not fence delayed upload: %v", err)
	}
	if err := os.Symlink(dir, filepath.Join(dir, "objects", testID)); err != nil {
		t.Fatal(err)
	}
	if err := DiscardObjects(ctx, dir, []string{testID}); err == nil {
		t.Fatal("discard accepted symlink object")
	}
}
