package coldstore

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"srv/internal/storage"
)

const testID = "0e4071d6-9e11-4d1a-b827-e5dbd305ef3b"

func testObject() Object {
	return Object{ID: testID, UUID: testID, SHA256: strings.Repeat("a", 64), Size: 42}
}

func TestProtocol(t *testing.T) {
	for _, r := range []request{{Operation: "put", Object: testObject()}, {Operation: "stat", Object: Object{ID: testID}}, {Operation: "get", Object: Object{ID: testID}}, {Operation: "delete", Object: Object{ID: testID}}} {
		encoded, err := encodeRequest(r)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseRequest(encoded)
		if err != nil || got != r {
			t.Fatalf("roundtrip: %#v %v", got, err)
		}
		for _, suffix := range []string{" ", ";id", "\n", " extra"} {
			if _, err := parseRequest(encoded + suffix); err == nil {
				t.Fatalf("accepted suffix %q", suffix)
			}
		}
	}
	for _, s := range []string{"", "sh -c id", protocol + " ../disk", protocol + " " + strings.Repeat("a", maxMetadata*2)} {
		if _, err := parseRequest(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
	for _, json := range []string{`{"operation":"stat","object":{"id":"` + testID + `"},"path":"/etc"}`, `{"operation":"exec","object":{"id":"` + testID + `"}}`, `{} {}`} {
		if _, err := parseRequest(protocol + " " + base64.RawURLEncoding.EncodeToString([]byte(json))); err == nil {
			t.Fatalf("accepted %s", json)
		}
	}
}

func TestObjectValidation(t *testing.T) {
	for _, change := range []func(*Object){
		func(o *Object) { o.ID = "../../etc" }, func(o *Object) { o.ID = strings.ToUpper(o.ID) }, func(o *Object) { o.UUID = "" }, func(o *Object) { o.Size = 0 }, func(o *Object) { o.SHA256 = "zz" }, func(o *Object) { o.Parent = o.ID }, func(o *Object) { o.Parent = "bad" },
	} {
		o := testObject()
		change(&o)
		if o.validate() == nil {
			t.Fatalf("accepted %#v", o)
		}
	}
}

func TestSSHArguments(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-owned credential trust chain requires root")
	}
	dir, err := os.MkdirTemp("/root", "coldstore-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	key, hosts := filepath.Join(dir, "key"), filepath.Join(dir, "hosts")
	for _, p := range []string{key, hosts} {
		if err := os.WriteFile(p, []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := Client{Config: Config{Host: "root@example.com", IdentityFile: key, KnownHostsFile: hosts}}
	r := request{Operation: "stat", Object: Object{ID: testID}}
	args, err := c.sshArgs(r)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, required := range []string{"BatchMode=yes", "StrictHostKeyChecking=yes", "IdentitiesOnly=yes", "ConnectTimeout=15", "UserKnownHostsFile=" + hosts, "-F /dev/null", "IdentityAgent=none"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing %s", required)
		}
	}
	for _, host := range []string{"-oProxyCommand=id", "a;id", "a b", "a\nb", "user@host", "$(id)"} {
		c.Config.Host = host
		if _, err := c.sshArgs(r); err == nil {
			t.Fatalf("accepted host %q", host)
		}
	}
	c.Config.Host = "example.com"
	link := filepath.Join(dir, "link")
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	c.Config.IdentityFile = link
	if _, err := c.sshArgs(r); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestBoundedMetadata(t *testing.T) {
	var out limitBuffer
	n, err := out.Write([]byte(strings.Repeat("x", maxMetadata*4)))
	if err != nil || n != maxMetadata*4 || out.Len() != maxMetadata || !out.overflow {
		t.Fatalf("invalid bounded write: %d %v", n, err)
	}
	if err := decode(strings.NewReader(strings.Repeat("x", maxMetadata+1)), new(Object)); err == nil {
		t.Fatal("accepted oversized metadata")
	}
}

func TestProcessHelper(t *testing.T) {
	if os.Getenv("COLDSTORE_HELPER") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "fail":
		fmt.Fprintln(os.Stderr, "intentional failure")
		os.Exit(7)
	case "wait":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "write":
		fmt.Print("stream")
		os.Exit(0)
	case "read":
		b, _ := io.ReadAll(os.Stdin)
		if string(b) != "stream" {
			os.Exit(9)
		}
		os.Exit(0)
	}
	os.Exit(10)
}

func TestPipeline(t *testing.T) {
	t.Setenv("COLDSTORE_HELPER", "1")
	args := func(mode string) []string { return []string{"-test.run=^TestProcessHelper$", "--", mode} }
	for _, pair := range [][2]string{{"fail", "wait"}, {"wait", "fail"}, {"write", "read"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		start := time.Now()
		err := pipeline(ctx, os.Args[0], args(pair[0]), os.Args[0], args(pair[1]))
		cancel()
		if pair[0] == "write" {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "intentional failure") {
			t.Fatalf("missing failure: %v", err)
		}
		if time.Since(start) > 4*time.Second {
			t.Fatal("failed to cancel sibling promptly")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := pipeline(ctx, os.Args[0], args("wait"), os.Args[0], args("wait")); err == nil {
		t.Fatal("deadline ignored")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := pipeline(ctx2, "/nonexistent-coldstore-test", nil, os.Args[0], args("wait")); err == nil {
		t.Fatal("startup error ignored")
	}
}

func TestServeRejectsBeforeIO(t *testing.T) {
	err := Serve(context.Background(), "/does/not/exist", "sh -c id", strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("wrong error: %v", err)
	}
	ctx, cancel := bounded(context.Background(), time.Millisecond)
	defer cancel()
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal(ctx.Err())
	}
}

func TestLocalContractsRejectBeforeIO(t *testing.T) {
	ctx := context.Background()
	bad := testObject()
	bad.ID = "../escape"
	for name, call := range map[string]func() error{
		"receive": func() error { return Receive(ctx, "/nonexistent", bad, nil) },
		"stat":    func() error { _, err := LocalStat(ctx, "/nonexistent", bad.ID); return err },
		"clone":   func() error { return Clone(ctx, "/nonexistent", bad, "/nonexistent/dest") },
		"remove":  func() error { return Remove(ctx, "/nonexistent", bad.ID) },
		"send":    func() error { return Send(ctx, bad, "/nonexistent", "", nil) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil || !strings.Contains(err.Error(), "invalid") {
				t.Fatalf("did not reject invalid identity before IO: %v", err)
			}
		})
	}
	for _, withParent := range []bool{false, true} {
		o, parentPath := testObject(), "/nonexistent/parent"
		if withParent {
			o.Parent = "bb51d8e4-6044-4871-b231-dfd805be04aa"
			parentPath = ""
		}
		if err := Send(ctx, o, "/nonexistent", parentPath, nil); err == nil || !strings.Contains(err.Error(), "supplied together") {
			t.Fatalf("did not reject parent mismatch before IO: %v", err)
		}
	}
	// No SSH configuration or executable should be needed to reject a bad
	// local source. No subprocess should be reached on this ordinary directory.
	t.Setenv("PATH", "")
	if err := Send(ctx, testObject(), t.TempDir(), "", io.Discard); err == nil || strings.Contains(err.Error(), "executable file") {
		t.Fatalf("source was not validated before starting btrfs: %v", err)
	}
}

func TestSenderRootValidationDoesNotWaitForReceivers(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("private store validation requires root")
	}
	oldRequire := storage.RequireBtrfs
	t.Cleanup(func() { storage.RequireBtrfs = oldRequire })
	storage.RequireBtrfs = storage.ValidatePath
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	release := make(chan struct{})
	stop := sync.OnceFunc(func() { close(release) })
	defer stop()
	results := make(chan error, 2)
	var roots []string
	for i := 0; i < 2; i++ {
		root, err := os.MkdirTemp("/root", "srv-receiver-test-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		roots = append(roots, root)
		ready := make(chan struct{})
		go func() {
			results <- withServer(ctx, root, func(server) error {
				close(ready) // Both receiver namespaces are locked before senders run.
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}()
		select {
		case <-ready:
		case err := <-results:
			t.Fatal("receiver failed before locking", err)
		}
	}
	validated := make(chan error, 1)
	go func() {
		for _, root := range roots {
			if err := ValidateRoot(root); err != nil {
				validated <- err
				return
			}
		}
		validated <- nil
	}()
	select {
	case err := <-validated:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("senders waited on incoming stream locks")
	}
	// Release and join before restoring filesystem seams or deleting roots.
	stop()
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestSendArguments(t *testing.T) {
	for _, tc := range []struct {
		parent string
		want   []string
	}{
		{"", []string{"send", "/snap shot"}},
		{"/base path", []string{"send", "-p", "/base path", "/snap shot"}},
	} {
		if got := sendArgs("/snap shot", tc.parent); !slices.Equal(got, tc.want) {
			t.Fatalf("send args: %q, want %q", got, tc.want)
		}
	}
}

type credentialInfo struct {
	os.FileInfo
	mode os.FileMode
	uid  uint32
}

func (s credentialInfo) Mode() os.FileMode { return s.mode }
func (s credentialInfo) IsDir() bool       { return s.mode.IsDir() }
func (s credentialInfo) Sys() any          { return &syscall.Stat_t{Uid: s.uid} }

func TestCredentialTrustChain(t *testing.T) {
	for _, tc := range []struct {
		path string
		mode os.FileMode
		uid  uint32
	}{
		{"", 0, 0}, {"/keys/id", 0600, 1000}, {"/keys/id", 0620, 0}, {"/keys/id", 0602, 0},
		{"/keys/id", os.ModeSymlink | 0600, 0}, {"/keys", os.ModeDir | 0700, 1000},
		{"/keys", os.ModeDir | 0770, 0}, {"/keys", os.ModeDir | os.ModeSticky | 0777, 0},
		{"/keys", os.ModeSymlink | 0700, 0}, {"/", os.ModeDir | 0777, 0},
	} {
		lookup := func(p string) (os.FileInfo, error) {
			s := credentialInfo{mode: os.ModeDir | 0755}
			if p == "/keys/id" {
				s.mode = 0600
			}
			if p == tc.path {
				s.mode, s.uid = tc.mode, tc.uid
			}
			return s, nil
		}
		err := validateCredential("/keys/id", lookup)
		if (err == nil) != (tc.path == "") {
			t.Fatalf("case %+v: %v", tc, err)
		}
	}
}

func TestDeletionRecovery(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-owned stage recovery requires root")
	}
	root := t.TempDir()
	s := server{root: root, objects: filepath.Join(root, "objects")}
	if err := os.Mkdir(s.objects, 0700); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, ".stage-delete-"+testID)
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	// Crash after the disk was removed: the incomplete manifest-only object
	// is outside the published namespace and deletion remains retryable.
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.objects); err != nil {
		t.Fatal(err)
	}
	if err := s.delete(context.Background(), testID); err == nil {
		t.Fatal("reported absence before successful directory sync")
	}
	if _, err := os.Stat(stage); err != nil {
		t.Fatal("destroyed tombstone before syncing rename parents", err)
	}
	if err := os.Mkdir(s.objects, 0700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(root, ".stage-foreign")
	if err := os.Mkdir(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.delete(context.Background(), testID); err == nil {
		t.Fatal("accepted foreign stage")
	}
	if _, err := os.Stat(stage); err != nil {
		t.Fatal("cleanup began before all stages were validated", err)
	}
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	if err := s.delete(context.Background(), testID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tombstone remains: %v", err)
	}
	if err := s.delete(context.Background(), testID); err != nil {
		t.Fatal("repeated delete", err)
	}
	if err := os.Symlink(s.objects, stage); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverStages(context.Background()); err == nil {
		t.Fatal("accepted symlink stage")
	}
}

func TestDeleteAbsentFencesDelayedPut(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-owned permanent deletion records require root")
	}
	// Exercise the forced-command dispatch and its real namespace flock. No
	// btrfs operation should be reached for either deleting absence or a fenced
	// upload, so only the filesystem preflight is replaced in this unit test.
	old := storage.RequireBtrfs
	storage.RequireBtrfs = func(string) error { return nil }
	t.Cleanup(func() { storage.RequireBtrfs = old })
	root, err := os.MkdirTemp("/root", "coldstore-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	ctx := context.Background()
	del, err := encodeRequest(request{Operation: "delete", Object: Object{ID: testID}})
	if err != nil {
		t.Fatal(err)
	}
	// Construct the upload before deletion, but delay its server dispatch until
	// after Delete has acknowledged: the old code would accept this upload.
	put, err := encodeRequest(request{Operation: "put", Object: testObject()})
	if err != nil {
		t.Fatal(err)
	}
	if err := Serve(ctx, root, del, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := Serve(ctx, root, put, strings.NewReader(""), io.Discard); !errors.Is(err, errObjectDeleted) {
		t.Fatalf("delayed put was not fenced: %v", err)
	}
	if err := Serve(ctx, root, del, nil, io.Discard); err != nil {
		t.Fatal("retry after lost acknowledgement", err)
	}
	// Local operations share the wire protocol's lock and permanent fences.
	if err := Remove(ctx, root, testID); err != nil {
		t.Fatal("local retry after remote delete", err)
	}
	if _, err := LocalStat(ctx, root, testID); !errors.Is(err, errObjectDeleted) {
		t.Fatalf("local stat ignored remote deletion: %v", err)
	}
	if err := Receive(ctx, root, testObject(), nil); !errors.Is(err, errObjectDeleted) {
		t.Fatalf("local receive ignored remote deletion: %v", err)
	}
	if err := Clone(ctx, root, testObject(), filepath.Join(root, "clone")); !errors.Is(err, errObjectDeleted) {
		t.Fatalf("local clone ignored remote deletion: %v", err)
	}
	s := server{root: root, objects: filepath.Join(root, "objects")}
	if err := s.recoverStages(ctx); err != nil {
		t.Fatal(err)
	}
	if deleted, err := s.deletionRecord(testID, false); err != nil || !deleted {
		t.Fatalf("recovery removed permanent record: %v %v", deleted, err)
	}
	if _, err := os.Stat(s.path(testID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected published object: %v", err)
	}
}

func TestInterruptedDeletionRecordPublication(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-owned permanent deletion records require root")
	}
	for _, failAt := range []string{"record", "directory", "root"} {
		t.Run(failAt, func(t *testing.T) {
			root := t.TempDir()
			s := server{root: root, objects: filepath.Join(root, "objects")}
			if err := os.Mkdir(s.objects, 0700); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "deleted")
			path := filepath.Join(dir, testID)
			failPath := map[string]string{"record": path, "directory": dir, "root": root}[failAt]
			s.syncDeletionPath = func(p string) error {
				if p == failPath {
					return syscall.ENOSPC
				}
				return storage.SyncPath(p)
			}
			ctx := context.Background()
			if err := s.delete(ctx, testID); !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("acknowledged failed tombstone publication: %v", err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if before.Size() != 0 {
				t.Fatal("record must contain only its UUID filename")
			}
			if err := s.put(ctx, testObject(), strings.NewReader("")); !errors.Is(err, errObjectDeleted) {
				t.Fatalf("interrupted publication did not fail closed: %v", err)
			}
			// Simulate a fresh invocation after the interrupted create/sync. It
			// must finish syncing the existing record, not delete or replace it.
			s = server{root: root, objects: s.objects}
			if err := s.delete(ctx, testID); err != nil {
				t.Fatal("retry", err)
			}
			if err := s.delete(ctx, testID); err != nil {
				t.Fatal("lost-ack retry", err)
			}
			after, err := os.Stat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("permanent record changed: %v", err)
			}
			if _, err := s.stat(ctx, testID); !errors.Is(err, errObjectDeleted) {
				t.Fatalf("stat did not respect permanent record: %v", err)
			}
		})
	}
}
