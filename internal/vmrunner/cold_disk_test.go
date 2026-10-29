package vmrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"srv/internal/coldstore"
	"srv/internal/storage"
)

// Only the btrfs kernel boundary and remote transport are substituted. These
// tests use the public Unix RPC, real journals, renames, contents and fsyncs.
type coldFixture struct {
	t                            *testing.T
	s                            *Server
	c                            *Client
	q                            ColdRequest
	disk                         string
	ids                          map[uint64]storage.SubvolumeIdentity
	objects                      map[string][]byte
	failPut, failGet, failDelete bool
	putWait                      func(context.Context) error
}

func newColdFixture(t *testing.T) *coldFixture {
	t.Helper()
	s, root, _ := newRunnerShutdownTestServer(t)
	cfg := s.config
	cfg.CgroupRoot = root
	cfg.ColdStorage = coldstore.Config{Host: "storage.example"}
	s = NewServer(nil, cfg)
	f := &coldFixture{t: t, s: s, q: ColdRequest{Name: "demo", InstanceID: uuid.NewString()}, ids: make(map[uint64]storage.SubvolumeIdentity), objects: make(map[string][]byte)}
	f.disk = filepath.Join(s.config.InstancesDir, "demo", "disk")
	oldInspect, oldSnapshot, oldDelete, oldReadonly, oldRemote := storage.InspectSubvolume, storage.SnapshotSubvolume, storage.DeleteSubvolume, storage.RequireReadonly, newColdRemote
	oldLoop := coldNoLoop
	oldPayload, oldFS, oldPrivate := storage.AcquirePayload, storage.FilesystemUUID, coldPrivateDir
	t.Cleanup(func() {
		storage.InspectSubvolume, storage.SnapshotSubvolume, storage.DeleteSubvolume, storage.RequireReadonly, newColdRemote = oldInspect, oldSnapshot, oldDelete, oldReadonly, oldRemote
		coldNoLoop = oldLoop
		storage.AcquirePayload, storage.FilesystemUUID, coldPrivateDir = oldPayload, oldFS, oldPrivate
	})
	storage.FilesystemUUID = func(string) (string, error) { return "fixture-filesystem", nil }
	coldPrivateDir = func(path string, _ bool) error { return storage.ValidatePath(path) }
	storage.AcquirePayload = func(ctx context.Context, dataDir string) (context.Context, func(), error) {
		file, err := os.OpenFile(filepath.Join(dataDir, ".test-payload-lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return ctx, nil, err
		}
		for {
			if err := ctx.Err(); err != nil {
				file.Close()
				return ctx, nil, err
			}
			if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
				return storage.WithInheritedLock(ctx, file), func() { file.Close() }, nil
			} else if !errors.Is(err, unix.EWOULDBLOCK) {
				file.Close()
				return ctx, nil, err
			}
			time.Sleep(time.Millisecond)
		}
	}
	coldNoLoop = func(context.Context, string) error { return nil }
	storage.InspectSubvolume = func(ctx context.Context, path string) (storage.SubvolumeIdentity, error) {
		st, err := os.Stat(path)
		if err != nil {
			return storage.SubvolumeIdentity{}, err
		}
		id, ok := f.ids[st.Sys().(*syscall.Stat_t).Ino]
		if !ok {
			return id, errors.New("not a fake subvolume")
		}
		return id, nil
	}
	storage.RequireReadonly = func(context.Context, string) error { return nil }
	storage.SnapshotSubvolume = func(ctx context.Context, source, dest string, readonly bool) error {
		id, err := storage.InspectSubvolume(ctx, source)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(source, "rootfs.img"))
		if err != nil {
			return err
		}
		f.volume(dest, data, id.UUID, "")
		return nil
	}
	storage.DeleteSubvolume = func(ctx context.Context, path string, want storage.SubvolumeIdentity) error {
		got, err := storage.InspectSubvolume(ctx, path)
		if err != nil {
			return err
		}
		if got != want {
			return errors.New("identity mismatch")
		}
		return os.RemoveAll(path)
	}
	newColdRemote = func(coldstore.Config) coldRemote { return f }
	f.volume(f.disk, []byte("unique guest data, not the base"), "", "")
	f.c = receiptHTTPClient(t, s.Handler())
	return f
}

func (f *coldFixture) volume(path string, data []byte, parent, received string) storage.SubvolumeIdentity {
	f.t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "rootfs.img"), data, 0600); err != nil {
		f.t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		f.t.Fatal(err)
	}
	ino := st.Sys().(*syscall.Stat_t).Ino
	id := storage.SubvolumeIdentity{UUID: uuid.NewString(), ID: fmt.Sprint(ino), ParentUUID: parent, ReceivedUUID: received}
	f.ids[ino] = id
	return id
}

func (f *coldFixture) Put(ctx context.Context, o coldstore.Object, path, parent string) error {
	b, err := os.ReadFile(filepath.Join(path, "rootfs.img"))
	if err != nil {
		return err
	}
	f.objects[o.ID] = b
	if f.putWait != nil {
		if err := f.putWait(ctx); err != nil {
			return err
		}
	}
	if f.failPut {
		return errors.New("lost upload acknowledgement")
	}
	return nil
}
func (f *coldFixture) Get(ctx context.Context, o coldstore.Object, dest, parent string) (string, error) {
	if f.failGet {
		return "", errors.New("remote unavailable")
	}
	b, ok := f.objects[o.ID]
	if !ok {
		return "", os.ErrNotExist
	}
	path := filepath.Join(dest, o.ID, "disk")
	f.volume(path, b, "", o.UUID)
	return path, nil
}
func (f *coldFixture) Delete(ctx context.Context, id string) error {
	if f.failDelete {
		return errors.New("remote unavailable")
	}
	delete(f.objects, id)
	return nil
}
func (f *coldFixture) call(action string) (ColdStatus, error) {
	q := f.q
	q.Action = action
	if action == "offload" {
		q.ExpectedDisk, _ = storage.InspectSubvolume(context.Background(), f.disk)
	}
	return f.c.ColdDisk(context.Background(), q)
}
func (f *coldFixture) must(action, phase string) {
	f.t.Helper()
	status, err := f.call(action)
	if err != nil || status.Phase != phase {
		f.t.Fatalf("%s: %+v %v", action, status, err)
	}
}
func (f *coldFixture) contents() string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.disk, "rootfs.img"))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func TestColdDiskRoundtripAndNextGeneration(t *testing.T) {
	f := newColdFixture(t)
	want := f.contents()
	f.must("offload", "cold")
	if _, err := os.Stat(f.disk); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("offload retained live disk", err)
	}
	if err := f.s.coldStartFence(f.q); err == nil {
		t.Fatal("cold disk admitted start")
	}
	f.failGet = true
	if _, err := f.call("hydrate"); err == nil {
		t.Fatal("unreachable remote hydrated")
	}
	if _, err := os.Stat(f.disk); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed hydration published disk", err)
	}
	f.failGet = false
	f.must("hydrate", "hot")
	if got := f.contents(); got != want {
		t.Fatalf("restored %q want %q", got, want)
	}
	if err := f.s.coldStartFence(f.q); err != nil {
		t.Fatal(err)
	}
	// A second stop must offload the current content, never reuse the old copy.
	if err := os.WriteFile(filepath.Join(f.disk, "rootfs.img"), []byte("second generation changes"), 0600); err != nil {
		t.Fatal(err)
	}
	f.must("offload", "cold")
	if len(f.objects) != 1 {
		t.Fatalf("old instance generations retained: %d", len(f.objects))
	}
	f.must("hydrate", "hot")
	if f.contents() != "second generation changes" {
		t.Fatal("restored stale generation")
	}
	f.must("purge", "hot")
	if len(f.objects) != 0 {
		t.Fatal("purge left remote object")
	}
	if _, err := os.Stat(f.s.coldMarker(f.q.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("purge left fence", err)
	}
}

func TestColdDiskUploadFailureRetainsLocalAndTracksRemote(t *testing.T) {
	f := newColdFixture(t)
	want := f.contents()
	f.failPut = true
	if _, err := f.call("offload"); err == nil {
		t.Fatal("lost ack reported success")
	}
	if f.contents() != want {
		t.Fatal("upload failure changed local disk")
	}
	status, err := f.call("status")
	if err != nil || status.Phase != "hot" || !status.Remote {
		t.Fatal(status, err)
	}
	f.failGet = true
	f.must("hydrate", "hot")
	if err := f.s.coldStartFence(f.q); err != nil {
		t.Fatal(err)
	}
	f.failDelete = true
	if _, err := f.call("purge"); err == nil {
		t.Fatal("remote absence assumed after error")
	}
	if _, err := os.Stat(f.s.coldMarker(f.q.Name)); err != nil {
		t.Fatal("lost remote ownership", err)
	}
	// Local loss must not promote an old remote generation to authority.
	if err := os.RemoveAll(f.disk); err != nil {
		t.Fatal(err)
	}
	f.failGet = false
	if _, err := f.call("hydrate"); err == nil {
		t.Fatal("used stale remote fallback")
	}
}

func TestColdDiskRecoveryPublicationAndPurge(t *testing.T) {
	for _, point := range []string{"eviction-intent", "receive-candidate", "published", "purge-marker"} {
		t.Run(point, func(t *testing.T) {
			f := newColdFixture(t)
			want := f.contents()
			f.must("offload", "cold")
			r, err := f.s.readCold(f.q)
			if err != nil {
				t.Fatal(err)
			}
			switch point {
			case "eviction-intent":
				// The live disk was removed, but the final cold phase was not synced.
				r.Phase = "evicting"
			case "receive-candidate", "published":
				r.Phase = "restoring"
				path := filepath.Join(f.s.coldStageDir(f.q.Name), "candidate")
				r.Candidate = f.volume(path, []byte(want), "", "")
				if point == "published" {
					if err := os.Rename(path, f.disk); err != nil {
						t.Fatal(err)
					}
				}
			case "purge-marker":
				f.must("hydrate", "hot")
				r, err = f.s.readCold(f.q)
				if err != nil {
					t.Fatal(err)
				}
				delete(f.objects, r.Object.ID)
				r.Remote = false
				r.Phase = "purged"
			}
			if err := f.s.writeCold(f.q, r); err != nil {
				t.Fatal(err)
			}
			if point == "purge-marker" {
				if err := os.Remove(f.s.coldMarker(f.q.Name)); err != nil {
					t.Fatal(err)
				}
			}
			f.must("hydrate", "hot")
			if f.contents() != want {
				t.Fatal("recovery lost disk contents")
			}
		})
	}
}

func TestColdDiskIncarnationAndLocalIdentityFence(t *testing.T) {
	f := newColdFixture(t)
	old := f.q
	old.Action = "offload"
	old.ExpectedDisk = storage.SubvolumeIdentity{UUID: uuid.NewString(), ID: "old-subvolume"}
	if _, err := f.c.ColdDisk(context.Background(), old); err == nil {
		t.Fatal("old disk incarnation evicted replacement disk")
	}
	if len(f.objects) != 0 || f.contents() != "unique guest data, not the base" {
		t.Fatal("rejected stale offload changed local or remote data")
	}
	f.must("offload", "cold")
	other := f.q
	other.InstanceID = uuid.NewString()
	other.Action = "delete"
	if _, err := f.c.ColdDisk(context.Background(), other); err == nil {
		t.Fatal("different incarnation deleted remote")
	}
	f.must("hydrate", "hot")
	if err := os.RemoveAll(f.disk); err != nil {
		t.Fatal(err)
	}
	f.volume(f.disk, []byte("unrelated replacement"), "", "")
	if _, err := f.call("hydrate"); err == nil {
		t.Fatal("accepted unrelated local disk")
	}
	if err := f.s.coldStartFence(f.q); err == nil {
		t.Fatal("replacement disk admitted start")
	}
}

func TestColdDiskDeleteFencesLateOffload(t *testing.T) {
	for _, action := range []string{"delete", "fence-delete"} {
		t.Run(action, func(t *testing.T) {
			f := newColdFixture(t)
			late := f.q
			late.Action = "offload"
			late.ExpectedDisk, _ = storage.InspectSubvolume(context.Background(), f.disk)
			f.must(action, "hot")
			if _, err := f.c.ColdDisk(context.Background(), late); err == nil {
				t.Fatal("late offload recreated remote state after acknowledged deletion")
			}
			if len(f.objects) != 0 {
				t.Fatal("late offload uploaded deleted incarnation")
			}
			// The service can now remove the old directory and create a new incarnation.
			if err := os.RemoveAll(f.disk); err != nil {
				t.Fatal(err)
			}
			f.volume(f.disk, []byte("replacement instance"), "", "")
			f.q.InstanceID = uuid.NewString()
			f.must("offload", "cold")
		})
	}
}

func TestColdDiskDeletedIncarnationReplaysClosedStart(t *testing.T) {
	f := newColdFixture(t)
	req := receiptRequest(t)
	req.InstanceID = f.q.InstanceID
	ctx := context.Background()
	result, err := f.c.ResolveInstanceStart(ctx, resultRequest(req))
	if err != nil || result.Outcome != StartAbsent {
		t.Fatalf("close attempt: %+v %v", result, err)
	}
	f.must("fence-delete", "hot")
	result, err = f.c.StartInstanceVM(ctx, req)
	if err != nil || result.Outcome != StartAbsent {
		t.Fatalf("replay closed attempt after deletion: %+v %v", result, err)
	}
	req.RequestID = uuid.NewString()
	if _, err := f.c.StartInstanceVM(ctx, req); err == nil {
		t.Fatal("new start bypassed deletion fence")
	}
}

func TestColdDiskOldRunnerFailsWithUpgradeAdvice(t *testing.T) {
	c := receiptHTTPClient(t, http.NotFoundHandler())
	_, err := c.ColdDisk(context.Background(), ColdRequest{Name: "demo", InstanceID: uuid.NewString(), Action: "delete"})
	if err == nil || !strings.Contains(err.Error(), "upgrade srv-vm-runner") {
		t.Fatalf("old runner: %v", err)
	}
}

func TestColdDiskProofFenceRefusesHiddenJournal(t *testing.T) {
	f := newColdFixture(t)
	f.must("offload", "cold")
	if err := os.Remove(f.s.coldMarker(f.q.Name)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.call("fence-delete"); err == nil {
		t.Fatal("hidden root journal accepted by proof fence")
	}
	if len(f.objects) != 1 {
		t.Fatal("proof fence erased remote data")
	}
	f.must("hydrate", "hot")
}

func TestColdDiskSerializesLateStart(t *testing.T) {
	f := newColdFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.putWait = func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	offload := make(chan error, 1)
	go func() { _, err := f.call("offload"); offload <- err }()
	<-entered
	statusCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	statusReq := f.q
	statusReq.Action = "status"
	status, statusErr := f.c.ColdDisk(statusCtx, statusReq)
	cancel()
	if statusErr != nil || status.Phase != "offloading" || !status.Remote {
		close(release)
		<-offload
		t.Fatalf("status blocked or lost in-flight phase: %+v %v", status, statusErr)
	}
	req := receiptRequest(t)
	req.InstanceID = f.q.InstanceID
	started := make(chan error, 1)
	go func() { _, err := f.c.StartInstanceVM(context.Background(), req); started <- err }()
	select {
	case err := <-started:
		close(release)
		<-offload
		t.Fatalf("start crossed active transfer: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-offload; err != nil {
		t.Fatal(err)
	}
	if err := <-started; err == nil {
		t.Fatal("late start admitted after disk eviction")
	}
	f.must("hydrate", "hot")
}

func TestColdDiskSkipsBrokenUnrelatedBase(t *testing.T) {
	f := newColdFixture(t)
	broken := filepath.Join(filepath.Dir(f.s.config.InstancesDir), "image-bases", strings.Repeat("0", 64), "disk")
	if err := os.MkdirAll(broken, 0700); err != nil {
		t.Fatal(err)
	}
	want := f.contents()
	f.must("offload", "cold")
	f.must("hydrate", "hot")
	if f.contents() != want {
		t.Fatal("full-send fallback changed disk contents")
	}
}

func TestNativeMigrationSnapshotAndIncarnation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("private migration store trust requires root")
	}
	f := newColdFixture(t)
	root, err := os.MkdirTemp("/root", "srv-migration-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	f.s.config.MigrationStoreDir = root
	oldRequire, oldFS := storage.RequireBtrfs, storage.FilesystemUUID
	t.Cleanup(func() { storage.RequireBtrfs, storage.FilesystemUUID = oldRequire, oldFS })
	storage.RequireBtrfs = storage.ValidatePath
	storage.FilesystemUUID = func(string) (string, error) { return "same-filesystem", nil }
	ctx := context.Background()
	basePath := filepath.Join(filepath.Dir(f.s.config.InstancesDir), "image-bases", strings.Repeat("a", 64), "disk")
	base := f.volume(basePath, []byte("common base"), "", "")
	st, err := os.Stat(f.disk)
	if err != nil {
		t.Fatal(err)
	}
	ino := st.Sys().(*syscall.Stat_t).Ino
	id := f.ids[ino]
	id.ParentUUID = base.UUID
	f.ids[ino] = id
	q := MigrationDiskRequest{Action: "prepare", Name: f.q.Name, InstanceID: f.q.InstanceID, Token: uuid.NewString(), ExpectedDisk: id}
	check := q
	check.Action = "check"
	// An incoming transfer owns this namespace flock until its stream ends.
	// The real runner's sender/preflight path must not wait for that stream.
	receiveLock, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer receiveLock.Close()
	if err := syscall.Flock(int(receiveLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Second)
	_, err = f.c.MigrationDisk(checkCtx, check, nil, nil)
	cancel()
	if err != nil {
		t.Fatal("runner preflight waited for an incoming transfer", err)
	}
	if err := syscall.Flock(int(receiveLock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	// Publication's provenance must coexist with the real lifecycle scanner;
	// otherwise a migrated guest cannot start or migrate onward.
	if err := atomicColdJSON(f.s.migrationImportPath(q.Name), migrationDiskReceipt{InstanceID: q.InstanceID, LocalDisk: id}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.s.storageIdleLocked(q.Name); err != nil {
		t.Fatal("migration provenance blocked lifecycle admission", err)
	}
	prepared, err := f.c.MigrationDisk(ctx, q, nil, nil)
	if err != nil || prepared.Transfer.Base == nil || prepared.Transfer.Base.UUID != base.UUID || prepared.Transfer.Disk.Parent != base.UUID {
		t.Fatalf("prepare shared base: %+v %v", prepared, err)
	}
	retry, err := f.c.MigrationDisk(ctx, q, nil, nil)
	if err != nil || retry.Transfer.Disk != prepared.Transfer.Disk || *retry.Transfer.Base != *prepared.Transfer.Base {
		t.Fatalf("prepare retry changed snapshot: %+v %v", retry, err)
	}
	other := q
	other.InstanceID = uuid.NewString()
	if _, err := f.c.MigrationDisk(ctx, other, nil, nil); err == nil {
		t.Fatal("adopted another incarnation's prepared snapshot")
	}
	q.Action = "cleanup"
	if _, err := f.c.MigrationDisk(ctx, q, nil, nil); err != nil {
		t.Fatal(err)
	}
	if f.contents() != "unique guest data, not the base" {
		t.Fatal("cleanup changed live source disk")
	}
	if _, err := os.Stat(basePath); err != nil {
		t.Fatal("cleanup removed shared image base", err)
	}
}

func TestNativeMigrationFilesystemIntegration(t *testing.T) {
	source, other := os.Getenv("COLDSTORE_TEST_SOURCE"), os.Getenv("COLDSTORE_TEST_REMOTE")
	if source == "" || other == "" || os.Geteuid() != 0 {
		t.Skip("requires root and two disposable Btrfs filesystems via COLDSTORE_TEST_SOURCE/REMOTE")
	}
	root, err := os.MkdirTemp(source, "runner-filesystem-")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t.Cleanup(func() {
		if err := storage.RemoveTree(ctx, root); err != nil {
			t.Error(err)
		}
	})
	instances := filepath.Join(root, "instances")
	if err := storage.CreateSubvolume(ctx, instances); err != nil {
		t.Fatal(err)
	}
	s := NewServer(nil, ServerConfig{InstancesDir: instances, JailerBaseDir: filepath.Join(root, "jailer")})
	// The source directory and instances live in different subvolumes of one
	// filesystem. The other directory must belong to a separate filesystem.
	for _, tc := range []struct {
		store string
		code  int
	}{{source, http.StatusOK}, {other, http.StatusConflict}} {
		s.config.MigrationStoreDir = tc.store
		q := MigrationDiskRequest{Action: "check", Name: "filesystem-check", InstanceID: uuid.NewString(), Token: uuid.NewString()}
		metadata, err := json.Marshal(q)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/vm/migration-disk-v1", nil)
		req.Header.Set("X-Srv-Migration", string(metadata))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != tc.code || tc.code == http.StatusConflict && !strings.Contains(w.Body.String(), "same btrfs filesystem") {
			t.Fatalf("store %s: HTTP %d: %s", tc.store, w.Code, w.Body.String())
		}
	}
}
