package coldstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"srv/internal/storage"
)

// Opt in as root with COLDSTORE_TEST_SOURCE and COLDSTORE_TEST_REMOTE pointing
// to writable directories on TWO distinct disposable btrfs filesystems. This
// test creates/deletes only private children; it does not mount or format disks.
// It exercises actual kernel send/receive, not SSH authentication or sshd setup.
func TestBtrfsTwoFilesystems(t *testing.T) {
	source, remote := os.Getenv("COLDSTORE_TEST_SOURCE"), os.Getenv("COLDSTORE_TEST_REMOTE")
	if source == "" || remote == "" {
		t.Skip("set COLDSTORE_TEST_SOURCE and COLDSTORE_TEST_REMOTE for privileged btrfs integration")
	}
	if os.Geteuid() != 0 {
		t.Fatal("integration requires root")
	}
	for _, p := range []string{source, remote} {
		if err := storage.RequireBtrfs(p); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := os.Stat(source)
	b, _ := os.Stat(remote)
	if a.Sys().(*syscall.Stat_t).Dev == b.Sys().(*syscall.Stat_t).Dev {
		t.Fatal("two different btrfs filesystems required")
	}
	local, err := os.MkdirTemp(source, "coldstore-test-")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(remote, "coldstore-test-")
	if err != nil {
		cleanup(local)
		t.Fatal(err)
	}
	defer cleanup(local)
	defer cleanup(root)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	work := filepath.Join(local, "work")
	if err := storage.CreateSubvolume(ctx, work); err != nil {
		t.Fatal(err)
	}
	makeSnapshot := func(name, contents, parent string) (Object, string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, "rootfs.img"), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(local, name)
		if err := storage.SnapshotSubvolume(ctx, work, p, true); err != nil {
			t.Fatal(err)
		}
		id, err := storage.InspectSubvolume(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return Object{ID: uuid.NewString(), UUID: id.UUID, Size: int64(len(contents)), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(contents))), Parent: parent}, p
	}
	invoke := func(r request, in io.Reader, out io.Writer) error {
		command, err := encodeRequest(r)
		if err != nil {
			return err
		}
		return Serve(ctx, root, command, in, out)
	}
	upload := func(o Object, p, parent string) {
		t.Helper()
		stream, err := os.CreateTemp(t.TempDir(), "stream")
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		args := []string{"send"}
		if parent != "" {
			args = append(args, "-p", parent)
		}
		args = append(args, p)
		if err := run(ctx, "btrfs", args, nil, stream); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		if err := invoke(request{Operation: "put", Object: o}, stream, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("local shared base migration", func(t *testing.T) {
		store, err := os.MkdirTemp(remote, "coldstore-local-test-")
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup(store)
		transfer := func(o Object, path, parent string) {
			t.Helper()
			stream, err := os.CreateTemp(t.TempDir(), "stream")
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if err := Send(ctx, o, path, parent, stream); err != nil {
				t.Fatal(err)
			}
			if _, err := stream.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			if err := Receive(ctx, store, o, stream); err != nil {
				t.Fatal(err)
			}
		}
		base, basePath := makeSnapshot("migration-base", "shared base", "")
		base.ID = base.UUID
		var baseStream bytes.Buffer
		if err := Send(ctx, base, basePath, "", &baseStream); err != nil {
			t.Fatal(err)
		}
		// Two first migrations can race to install the shared base. Exactly one
		// publishes it; both must succeed and consume their complete stream.
		results := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func() {
				in := bytes.NewReader(baseStream.Bytes())
				err := Receive(ctx, store, base, in)
				if err == nil && in.Len() != 0 {
					err = fmt.Errorf("receive left %d stream bytes unread", in.Len())
				}
				results <- err
			}()
		}
		for i := 0; i < 2; i++ {
			if err := <-results; err != nil {
				t.Error(err)
			}
		}
		if t.Failed() {
			t.FailNow()
		}
		wrongBase := base
		wrongBase.Size++
		if err := Receive(ctx, store, wrongBase, nil); err == nil || !strings.Contains(err.Error(), "different metadata") {
			t.Fatalf("receive accepted changed base metadata: %v", err)
		}
		for i := 0; i < 2; i++ {
			contents := fmt.Sprintf("migration child %d", i)
			o, path := makeSnapshot(fmt.Sprintf("migration-child-%d", i), contents, base.ID)
			if err := Send(ctx, o, path, work, io.Discard); err == nil || !strings.Contains(err.Error(), "read-only") {
				t.Fatalf("accepted writable send parent: %v", err)
			}
			transfer(o, path, basePath)
			if got, err := LocalStat(ctx, store, o.ID); err != nil || got != o {
				t.Fatalf("local stat: %+v %v", got, err)
			}
			if err := Remove(ctx, store, base.ID); err == nil {
				t.Fatal("removed referenced shared base")
			}
			dest := filepath.Join(store, fmt.Sprintf("writable-%d", i))
			wrong := o
			wrong.Size++
			if err := Clone(ctx, store, wrong, dest); err == nil {
				t.Fatal("cloned metadata mismatch")
			}
			if err := Clone(ctx, store, o, dest); err != nil {
				t.Fatal(err)
			}
			if err := Clone(ctx, store, o, dest); err == nil {
				t.Fatal("overwrote existing clone")
			}
			if _, err := LocalStat(ctx, store, o.ID); err != nil {
				t.Fatal("clone removed stored object", err)
			}
			if got, err := os.ReadFile(filepath.Join(dest, "rootfs.img")); err != nil || string(got) != contents {
				t.Fatalf("clone contents: %q %v", got, err)
			}
			if err := os.WriteFile(filepath.Join(dest, "rootfs.img"), []byte("writable clone"), 0600); err != nil {
				t.Fatal("clone not writable", err)
			}
			for retry := 0; retry < 2; retry++ {
				if err := Remove(ctx, store, o.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := LocalStat(ctx, store, o.ID); !errors.Is(err, errObjectDeleted) {
				t.Fatalf("removed object still visible: %v", err)
			}
			if err := Receive(ctx, store, o, strings.NewReader("")); !errors.Is(err, errObjectDeleted) {
				t.Fatalf("late receive not fenced: %v", err)
			}
			if got, err := LocalStat(ctx, store, base.ID); err != nil || got != base {
				t.Fatalf("shared base not retained: %+v %v", got, err)
			}
			if got, err := os.ReadFile(filepath.Join(dest, "rootfs.img")); err != nil || string(got) != "writable clone" {
				t.Fatalf("removal affected clone: %q %v", got, err)
			}
		}
		// A retained received base must be sendable back without substituting
		// its filesystem-local UUID for the original sending identity.
		var resend bytes.Buffer
		if err := Send(ctx, base, filepath.Join(store, "objects", base.ID, "disk"), "", &resend); err != nil {
			t.Fatal("resend received base", err)
		}
		if err := storage.RemoveTree(ctx, basePath); err != nil {
			t.Fatal(err)
		}
		restore := filepath.Join(local, "base-resend-store")
		if err := os.Mkdir(restore, 0700); err != nil {
			t.Fatal(err)
		}
		if err := Receive(ctx, restore, base, &resend); err != nil {
			t.Fatal("receive resent base", err)
		}
		if got, err := LocalStat(ctx, restore, base.ID); err != nil || got != base {
			t.Fatalf("resent base stat: %+v %v", got, err)
		}
		// A migrated writable guest has a different immediate parent. Snapshot
		// its subsequent writes and send them back against the retained original
		// base, not against the discarded per-migration disk object.
		guest := filepath.Join(store, "writable-1")
		changed := []byte("onward guest changes after first migration")
		if err := os.WriteFile(filepath.Join(guest, "rootfs.img"), changed, 0600); err != nil {
			t.Fatal(err)
		}
		snapshot := filepath.Join(store, "onward-snapshot")
		if err := storage.SnapshotSubvolume(ctx, guest, snapshot, true); err != nil {
			t.Fatal(err)
		}
		identity, err := storage.InspectSubvolume(ctx, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		onward := Object{ID: uuid.NewString(), UUID: identity.UUID, SHA256: fmt.Sprintf("%x", sha256.Sum256(changed)), Size: int64(len(changed)), Parent: base.ID}
		var delta bytes.Buffer
		if err := Send(ctx, onward, snapshot, filepath.Join(store, "objects", base.ID, "disk"), &delta); err != nil {
			t.Fatal("send modified migrated guest", err)
		}
		if err := Receive(ctx, restore, onward, &delta); err != nil {
			t.Fatal("receive modified migrated guest", err)
		}
		returned := filepath.Join(restore, "returned-guest")
		if err := Clone(ctx, restore, onward, returned); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(returned, "rootfs.img")); err != nil || !bytes.Equal(got, changed) {
			t.Fatalf("modified guest return contents: %q %v", got, err)
		}
	})
	first, p1 := makeSnapshot("first", "first disk", "")
	upload(first, p1, "")
	second, p2 := makeSnapshot("second", "second disk", first.ID)
	upload(second, p2, p1)
	if err := invoke(request{Operation: "delete", Object: Object{ID: first.ID}}, nil, io.Discard); err == nil {
		t.Fatal("deleted referenced parent")
	}
	// First test the normal runner path: the original base remains local while
	// the instance snapshot has been evicted. Then repeat with a received base.
	if err := storage.RemoveTree(ctx, p2); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		o      Object
		parent string
		retain bool
	}{
		{second, first.ID, false}, {first, "", true}, {second, "", false}, {second, first.ID, true},
	} {
		o := tc.o
		if o.ID == first.ID {
			// Do not let original and received base UUID candidates coexist in
			// the received-base case and mask a lookup/transaction-ID problem.
			if err := storage.RemoveTree(ctx, p1); err != nil {
				t.Fatal(err)
			}
		}
		stream, err := os.CreateTemp(t.TempDir(), "download")
		if err != nil {
			t.Fatal(err)
		}
		if err := invoke(request{Operation: "get", Object: Object{ID: o.ID}, Parent: tc.parent}, nil, stream); err != nil {
			stream.Close()
			t.Fatal(err)
		}
		if _, err := stream.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		dest, err := os.MkdirTemp(local, "receive-")
		if err != nil {
			t.Fatal(err)
		}
		if err := run(ctx, "btrfs", []string{"receive", dest}, stream, io.Discard); err != nil {
			stream.Close()
			t.Fatal(err)
		}
		stream.Close()
		disk, err := receivedDisk(ctx, dest, o)
		if err != nil {
			t.Fatal(err)
		}
		id, err := storage.InspectSubvolume(ctx, disk)
		if err != nil {
			t.Fatal(err)
		}
		if id.ReceivedUUID != o.UUID || id.UUID == o.UUID {
			t.Fatalf("re-sent disk identity: %+v, original %s", id, o.UUID)
		}
		if !tc.retain {
			if err := storage.RemoveTree(ctx, dest); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A checksum disagreement must prevent hydration, but not deletion. Change
	// the recorded checksum rather than clearing a received subvolume's ro flag
	// (which would also invalidate its received UUID on modern kernels).
	s := server{root: root, objects: filepath.Join(root, "objects")}
	bad := second
	bad.SHA256 = strings.Repeat("0", 64)
	metadata, err := json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.path(second.ID), "manifest.json"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyDisk(ctx, filepath.Join(s.path(second.ID), "disk"), bad, true); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("checksum disagreement not detected: %v", err)
	}
	if err := Clone(ctx, root, bad, filepath.Join(root, "corrupt-clone")); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("clone accepted checksum disagreement: %v", err)
	}
	if _, err := s.stat(ctx, second.ID); err != nil {
		t.Fatal("stat should check immutable identity, not rescan bytes", err)
	}
	if err := invoke(request{Operation: "delete", Object: Object{ID: second.ID}}, nil, io.Discard); err != nil {
		t.Fatal("cannot delete checksum-corrupt object", err)
	}
	third, p3 := makeSnapshot("third", "third disk", first.ID)
	// Full send still records an explicit dependency for recovery validation.
	upload(third, p3, "")
	if err := os.WriteFile(filepath.Join(s.path(first.ID), "manifest.json"), []byte("broken manifest"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := invoke(request{Operation: "delete", Object: Object{ID: third.ID}}, nil, io.Discard); err == nil {
		t.Fatal("normal delete guessed dependencies from corrupt manifest")
	}
	if err := DiscardObjects(ctx, root, []string{first.ID}); err == nil {
		t.Fatal("discard removed a retained object's parent")
	}
	for i := 0; i < 2; i++ {
		if err := DiscardObjects(ctx, root, []string{first.ID, third.ID}); err != nil {
			t.Fatal("explicit corrupt-object disposition/retry", err)
		}
	}
	for _, o := range []Object{first, second, third} {
		if err := invoke(request{Operation: "put", Object: o}, strings.NewReader(""), io.Discard); !errors.Is(err, errObjectDeleted) {
			t.Fatalf("late upload resurrected disposed object: %v", err)
		}
	}
}
