package vmrunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"srv/internal/coldstore"
	"srv/internal/storage"
)

// Opt in on a disposable Btrfs host. Storage, SSH, runner handlers, journals and
// flocks are real; only the unused VM/cgroup fixture is substituted (no KVM).
func TestPayloadBtrfsRunnerIntegration(t *testing.T) {
	root := os.Getenv("SRV_TEST_DATA_ROOT")
	if root == "" {
		t.Skip("set SRV_TEST_DATA_ROOT and SRV_TEST_COLD_{HOST,KEY,KNOWN_HOSTS} for real runner storage integration")
	}
	if os.Geteuid() != 0 {
		t.Fatal("requires root on a disposable Btrfs test host")
	}
	if err := storage.RequireBtrfs(root); err != nil {
		t.Fatal(err)
	}
	data, err := os.MkdirTemp(root, "runner-payload-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := storage.RemoveTree(context.Background(), data); err != nil {
			t.Error("cleanup", err)
		}
	})
	for _, dir := range []string{"instances", "jailer", "migration", ".host-locks"} {
		if err := os.Mkdir(filepath.Join(data, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	lockPath := filepath.Join(data, ".host-locks", "payload.lock")
	if err := os.WriteFile(lockPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lockPath, 0660); err != nil {
		t.Fatal(err)
	}
	s, _, _ := newRunnerShutdownTestServer(t)
	s.config.InstancesDir, s.config.JailerBaseDir = filepath.Join(data, "instances"), filepath.Join(data, "jailer")
	s.config.MigrationStoreDir = filepath.Join(data, "migration")
	s.config.ColdStorage = coldstore.Config{
		Host: os.Getenv("SRV_TEST_COLD_HOST"), IdentityFile: os.Getenv("SRV_TEST_COLD_KEY"),
		KnownHostsFile: os.Getenv("SRV_TEST_COLD_KNOWN_HOSTS"), Timeout: time.Minute,
	}
	if s.config.ColdStorage.Host == "" {
		t.Fatal("SSH cold destination required")
	}
	if err := s.config.ColdStorage.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	name, instanceID := "kernel-roundtrip", uuid.NewString()
	dir := filepath.Join(s.config.InstancesDir, name)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(dir, "disk")
	if err := storage.CreateSubvolume(ctx, disk); err != nil {
		t.Fatal(err)
	}
	want := []byte(strings.Repeat("real-kernel-payload\n", 4096))
	if err := os.WriteFile(filepath.Join(disk, "rootfs.img"), want, 0600); err != nil {
		t.Fatal(err)
	}
	id, err := storage.ValidateDiskSubvolume(ctx, disk)
	if err != nil {
		t.Fatal(err)
	}
	cold := func(action string) (ColdStatus, int, string) {
		t.Helper()
		q := ColdRequest{Name: name, InstanceID: instanceID, Action: action, ExpectedDisk: id}
		wire, _ := json.Marshal(q)
		req := httptest.NewRequest("POST", "/vm/cold-disk-v1", bytes.NewReader(wire)).WithContext(ctx)
		w := httptest.NewRecorder()
		s.handleColdDisk(w, req)
		var status ColdStatus
		_ = json.Unmarshal(w.Body.Bytes(), &status)
		return status, w.Code, w.Body.String()
	}
	if got, code, body := cold("offload"); code != 200 || got.Phase != "cold" || !got.JournalPresent {
		t.Fatalf("offload: %+v code=%d %s", got, code, body)
	}
	if _, err := os.Stat(filepath.Join(disk, "rootfs.img")); !os.IsNotExist(err) {
		t.Fatalf("offload retained live disk: %v", err)
	}
	if err := os.Remove(s.coldMarker(name)); err != nil {
		t.Fatal(err)
	}
	host := s.config.ColdStorage.Host
	s.config.ColdStorage.Host = ""
	if got, code, body := cold("status"); code != 200 || got.Phase != "cold" || !got.JournalPresent || !got.Remote {
		t.Fatalf("hidden journal: %+v code=%d %s", got, code, body)
	}
	s.config.ColdStorage.Host = host
	r, err := s.readCold(ColdRequest{Name: name, InstanceID: instanceID})
	if err != nil {
		t.Fatal(err)
	}
	// Exercise immutable publication and an actual destination collision, not
	// only the normal hydration path into an empty receive directory.
	downloads := filepath.Join(data, "downloads")
	if err := os.Mkdir(downloads, 0700); err != nil {
		t.Fatal(err)
	}
	remote, err := s.coldRemote(r)
	if err != nil {
		t.Fatal(err)
	}
	received, err := remote.Get(ctx, r.Object, downloads, "")
	if err != nil {
		t.Fatal("download publication", err)
	}
	if _, err := remote.Get(ctx, r.Object, downloads, ""); !os.IsExist(err) {
		t.Fatalf("download collision did not refuse overwrite: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(received, "rootfs.img")); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("collision changed published disk: %v", err)
	}
	if err := storage.RequireReadonly(ctx, received); err != nil {
		t.Fatal("download no longer read-only", err)
	}
	t.Log("real download collision refused overwrite; published content and read-only flag preserved")
	size := r.Object.Size
	r.Object.Size = math.MaxInt64
	if err := s.writeCold(ColdRequest{Name: name, InstanceID: instanceID}, r); err != nil {
		t.Fatal(err)
	}
	if _, code, body := cold("hydrate"); code == 200 || !strings.Contains(body, "available disk") {
		t.Fatalf("oversized hydration not refused before receive: code=%d %s", code, body)
	}
	r.Object.Size = size
	if err := s.writeCold(ColdRequest{Name: name, InstanceID: instanceID}, r); err != nil {
		t.Fatal(err)
	}
	if got, code, body := cold("hydrate"); code != 200 || got.Phase != "hot" {
		t.Fatalf("hydrate: %+v code=%d %s", got, code, body)
	}
	actual, err := os.ReadFile(filepath.Join(disk, "rootfs.img"))
	if err != nil || !bytes.Equal(actual, want) {
		t.Fatalf("hydrated bytes differ: %v", err)
	}
	stageFS, err := storage.FilesystemUUID(s.coldStageDir(name))
	if err != nil {
		t.Fatal(err)
	}
	dataFS, _ := storage.FilesystemUUID(dir)
	if stageFS != dataFS {
		t.Fatal("staging escaped data filesystem")
	}
	if got, code, body := cold("purge"); code != 200 || got.JournalPresent || got.Remote {
		t.Fatalf("purge: %+v code=%d %s", got, code, body)
	}
	t.Log("real SSH cold offload/hydrate/purge preserved rootfs hash; hidden journal and pre-receive denial verified")

	// Exercise the actual native Receive handler, including waiting on the
	// same payload inode and checking space before consuming the body.
	snapshot := filepath.Join(data, "native-snapshot")
	if err := storage.SnapshotSubvolume(ctx, disk, snapshot, true); err != nil {
		t.Fatal(err)
	}
	snapID, _ := storage.ValidateDiskSubvolume(ctx, snapshot)
	token := uuid.NewString()
	object := coldstore.Object{ID: token, UUID: snapID.UUID, Size: int64(len(want)), SHA256: fmt.Sprintf("%x", sha256.Sum256(want))}
	var stream bytes.Buffer
	if err := coldstore.Send(ctx, object, snapshot, "", &stream); err != nil {
		t.Fatal(err)
	}
	q := MigrationDiskRequest{Name: "native-target", InstanceID: uuid.NewString(), Token: token, Action: "receive-disk", Transfer: MigrationDisk{Disk: object}}
	invoke := func(ctx context.Context, q MigrationDiskRequest, payload *bytes.Reader) *httptest.ResponseRecorder {
		t.Helper()
		wire, _ := json.Marshal(q)
		req := httptest.NewRequest("POST", "/vm/migration-disk-v1", payload).WithContext(ctx)
		req.Header.Set("X-Srv-Migration", string(wire))
		w := httptest.NewRecorder()
		s.handleMigrationDisk(w, req)
		return w
	}
	_, release, err := storage.AcquirePayload(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	payload := bytes.NewReader(stream.Bytes())
	w := invoke(waitCtx, q, payload)
	stop()
	check := q
	check.Action = "check"
	capability := invoke(ctx, check, bytes.NewReader(nil))
	release()
	if w.Code == 200 || payload.Len() != stream.Len() || capability.Code != 200 {
		t.Fatalf("serialization/capability: receive=%d unread=%d/%d check=%d", w.Code, payload.Len(), stream.Len(), capability.Code)
	}
	tooBig := q
	tooBig.Transfer.Disk.Size = math.MaxInt64
	payload = bytes.NewReader(stream.Bytes())
	if w := invoke(ctx, tooBig, payload); w.Code == 200 || payload.Len() != stream.Len() || !strings.Contains(w.Body.String(), "available disk") {
		t.Fatalf("native admission consumed rejected payload: %d %s", w.Code, w.Body.String())
	}
	if w := invoke(ctx, q, bytes.NewReader(stream.Bytes())); w.Code != 200 {
		t.Fatalf("native receive: %d %s", w.Code, w.Body.String())
	}
	stored, err := coldstore.LocalStat(ctx, s.config.MigrationStoreDir, token)
	if err != nil || stored != object {
		t.Fatalf("received object mismatch: %+v %v", stored, err)
	}
	if err := coldstore.Remove(ctx, s.config.MigrationStoreDir, token); err != nil {
		t.Fatal(err)
	}
	t.Log("real native receive serialized; capability stayed available; denied body untouched; stored identity/hash verified")
}
