package provision

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"srv/internal/host"
	"srv/internal/model"
	"srv/internal/store"
)

// The import receipt is the durable claim that the rootfs is usable. While the
// archive is still open it must stay importing, and the final rootfs path must
// not exist; only EOF may commit the file and publish the imported state.
func TestImportDoesNotPublishReceiptBeforeStagingCompletes(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{
		cfg:                 cfg,
		log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:               st,
		readFilesystemBytes: host.DefaultReadFilesystemBytes,
	}
	if err := os.MkdirAll(cfg.InstancesDir(), 0o770); err != nil {
		t.Fatal(err)
	}

	rootfsPayload := []byte("imported rootfs payload")
	manifest := portableManifest{
		Version:    portableManifestVersion,
		ExportedAt: time.Date(2026, 4, 5, 13, 0, 0, 0, time.UTC),
		Instance: portableInstance{
			Name:            "import-ordering",
			CreatedAt:       time.Date(2026, 4, 4, 13, 0, 0, 0, time.UTC),
			RootFSSizeBytes: int64(len(rootfsPayload)),
		},
		Files: backupFiles{RootFS: backupRootFSName},
	}

	gated := startGatedImport(t, p, st, manifest, rootfsPayload)
	inst := gated.instance
	if inst.State != model.StateProvisioning {
		t.Fatalf("instance state while staging = %q, want %q", inst.State, model.StateProvisioning)
	}
	receipt, err := st.GetImportReceipt(ctx, inst.ID)
	if err != nil || receipt.Phase != "importing" {
		t.Fatalf("receipt while staging = %+v, %v", receipt, err)
	}
	if _, err := os.Stat(inst.RootFSPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rootfs became visible before commit: %v", err)
	}

	gated.release()
	if err := waitImport(t, gated); err != nil {
		t.Fatalf("import after end-of-archive: %v", err)
	}
	final, err := st.GetInstance(ctx, manifest.Instance.Name)
	if err != nil || final.State != model.StateStopped {
		t.Fatalf("final instance = %+v, %v", final, err)
	}
	receipt, err = st.GetImportReceipt(ctx, final.ID)
	if err != nil || receipt.Phase != "imported" {
		t.Fatalf("final receipt = %+v, %v", receipt, err)
	}
	payload, err := os.ReadFile(final.RootFSPath)
	if err != nil || string(payload) != string(rootfsPayload) {
		t.Fatalf("final rootfs = %q, %v", payload, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(final.RootFSPath), ".srv-restore-*")); len(leftovers) != 0 {
		t.Fatalf("staged files left behind: %v", leftovers)
	}
}

// A failure while committing the staged file must not leave a durable imported
// claim after the import's own cleanup. This observes the post-cleanup state;
// the pre-finalize ordering is covered by the streaming gate above and code
// review, not by this failure path.
func TestImportCommitFailureDoesNotPublishImportedReceipt(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{
		cfg:                 cfg,
		log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:               st,
		readFilesystemBytes: host.DefaultReadFilesystemBytes,
	}
	if err := os.MkdirAll(cfg.InstancesDir(), 0o770); err != nil {
		t.Fatal(err)
	}
	rootfsPayload := []byte("imported rootfs payload")
	manifest := portableManifest{
		Version:    portableManifestVersion,
		ExportedAt: time.Date(2026, 4, 5, 13, 0, 0, 0, time.UTC),
		Instance: portableInstance{
			Name:            "import-commit-failure",
			CreatedAt:       time.Date(2026, 4, 4, 13, 0, 0, 0, time.UTC),
			RootFSSizeBytes: int64(len(rootfsPayload)),
		},
		Files: backupFiles{RootFS: backupRootFSName},
	}

	gated := startGatedImport(t, p, st, manifest, rootfsPayload)
	// A directory at the final destination makes the staged rename fail after
	// the stream has been fully staged.
	if err := os.Mkdir(gated.instance.RootFSPath, 0o755); err != nil {
		t.Fatal(err)
	}
	gated.release()
	if err := waitImport(t, gated); err == nil {
		t.Fatal("import succeeded despite a blocked commit destination")
	}
	if found, exists, err := st.FindInstance(ctx, manifest.Instance.Name); err != nil || exists {
		t.Fatalf("failed import left a visible instance: found=%v exists=%v err=%v", found, exists, err)
	}
	if receipt, err := st.GetImportReceipt(ctx, gated.instance.ID); err == nil && receipt.Phase == "imported" {
		t.Fatalf("failed import published an imported receipt: %+v", receipt)
	}
}

type gatedImport struct {
	instance model.Instance
	release  func()
	done     chan error
	finished chan struct{}
}

// startGatedImport pauses the archive after the rootfs entry has been staged
// but before end-of-archive. Cleanup always releases and joins both workers.
func startGatedImport(t *testing.T, p *Provisioner, st *store.Store, manifest portableManifest, rootfsPayload []byte) *gatedImport {
	t.Helper()
	pr, pw := io.Pipe()
	staged := make(chan struct{})
	allowFinish := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowFinish) }) }
	done := make(chan error, 1)
	finished := make(chan struct{})
	writerFinished := make(chan struct{})

	go func() {
		defer close(writerFinished)
		tw, zw, err := newPortableArtifactTarWriter(pw)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		payload, err := json.Marshal(manifest)
		if err == nil {
			err = writeTarBytes(tw, backupManifestName, payload, 0o640, manifest.ExportedAt)
		}
		if err == nil {
			err = writeTarBytes(tw, backupRootFSName, rootfsPayload, 0o660, manifest.ExportedAt)
		}
		if err == nil {
			err = tw.Flush()
		}
		if err == nil {
			err = zw.Flush()
		}
		if err != nil {
			_ = closePortableArtifactTarWriter(tw, zw)
			_ = pw.CloseWithError(err)
			return
		}
		close(staged)
		<-allowFinish
		if err := closePortableArtifactTarWriter(tw, zw); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()
	go func() {
		defer close(finished)
		_, _, err := p.ImportInstance(context.Background(), model.Actor{UserLogin: "alice@example.com", NodeName: "workstation"}, pr)
		done <- err
	}()
	t.Cleanup(func() {
		release()
		_ = pr.CloseWithError(errors.New("test cleanup"))
		_ = pw.Close()
		select {
		case <-writerFinished:
		case <-time.After(2 * time.Second):
			t.Errorf("import writer did not stop")
		}
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Errorf("import reader did not stop")
		}
	})

	select {
	case <-staged:
	case err := <-done:
		t.Fatalf("import failed before staging: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the rootfs entry to be written")
	}

	var inst model.Instance
	deadline := time.Now().Add(5 * time.Second)
	for {
		if found, exists, err := st.FindInstance(context.Background(), manifest.Instance.Name); err == nil && exists {
			inst = found
			matches, _ := filepath.Glob(filepath.Join(filepath.Dir(inst.RootFSPath), ".srv-restore-*"))
			if len(matches) == 1 {
				if info, statErr := os.Stat(matches[0]); statErr == nil && info.Size() == int64(len(rootfsPayload)) {
					return &gatedImport{instance: inst, release: release, done: done, finished: finished}
				}
			}
		}
		select {
		case err := <-done:
			t.Fatalf("import finished before staging the rootfs: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("rootfs was not staged before end-of-archive")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitImport(t *testing.T, gated *gatedImport) error {
	t.Helper()
	select {
	case err := <-gated.done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("import did not finish after end-of-archive")
		return nil
	}
}

// The cleanup fence and the deleting state cover the window between admission
// and the tombstone. While gateways are still draining and the files still
// exist, no receipt may be published.
func TestDeleteWithProofKeepsFenceUntilFilesAreGone(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	inst := provisionTestInstance(cfg, "delete-ordering", model.StateStopped, time.Date(2026, 4, 5, 13, 0, 0, 0, time.UTC))
	inst.Kind, inst.Image, inst.TailnetNodeID = model.KindOrb, "amp-orb", ""
	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.RootFSPath, []byte("retained data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdmitLifecycleRequest(ctx, store.LifecycleRequest{ID: "request", Actor: "owner", Command: "delete-proof", Name: inst.Name, Arguments: "bound"}); err != nil {
		t.Fatal(err)
	}
	p := &Provisioner{
		cfg:           cfg,
		store:         st,
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		vmRunner:      cleanupRunner{verify: func() error { return nil }},
		networkHelper: cleanupNetwork{verify: func() error { return nil }},
	}

	admitted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, err := p.DeleteWithProof(ctx, inst, "request", "owner", "backend-one", func(context.Context, model.Instance) error {
			close(admitted)
			<-release
			return nil
		})
		done <- err
	}()
	t.Cleanup(func() {
		unblock()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Errorf("DeleteWithProof worker did not stop")
		}
	})

	select {
	case <-admitted:
	case err := <-done:
		t.Fatalf("DeleteWithProof failed before admission: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cleanup admission")
	}

	if err := st.CheckCleanupFence(ctx, inst.ID); err == nil {
		t.Fatal("no cleanup fence after admission")
	}
	stored, err := st.GetInstance(ctx, inst.Name)
	if err != nil || stored.State != model.StateDeleting {
		t.Fatalf("instance after admission = %+v, %v", stored, err)
	}
	if _, err := os.Stat(inst.RootFSPath); err != nil {
		t.Fatalf("files removed before gateway quiescence: %v", err)
	}
	if _, err := st.CleanupReceipt(ctx, "request", "owner"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("receipt published before file removal: %v", err)
	}
	request, err := st.GetLifecycleRequest(ctx, "request", "owner")
	if err != nil || request.Phase != "executing" {
		t.Fatalf("request after admission = %+v, %v", request, err)
	}

	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DeleteWithProof: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DeleteWithProof did not finish after gateway release")
	}
	if _, err := st.CleanupReceipt(ctx, "request", "owner"); err != nil {
		t.Fatalf("receipt missing after success: %v", err)
	}
	stored, err = st.GetInstance(ctx, inst.Name)
	if err != nil || stored.State != model.StateDeleted || stored.DeletedAt == nil {
		t.Fatalf("instance after success = %+v, %v", stored, err)
	}
	if _, err := os.Stat(inst.RootFSPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rootfs remains after receipt: %v", err)
	}
	if err := st.CheckCleanupFence(ctx, inst.ID); err != nil {
		t.Fatalf("fence remains after receipt: %v", err)
	}
}

// A crash that bypasses restore rollback can leave a mixed destination set.
// Restore commits serial, firecracker log, then rootfs, so a reachable crash
// prefix is serial restored with the rootfs still live. Retrying restore must
// converge every file and the instance record. This retry cleans only its own
// staging/rollback artifacts; an earlier crash's leftovers are not modeled.
func TestRestoreRetryConvergesAfterPartialReplacement(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	oldReflink := reflinkCloneFile
	t.Cleanup(func() { reflinkCloneFile = oldReflink })
	reflinkCloneFile = func(_ context.Context, src, dest string) error {
		payload, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, payload, 0o644)
	}

	inst := provisionTestInstance(cfg, "restore-ordering", model.StateStopped, time.Date(2026, 4, 5, 13, 0, 0, 0, time.UTC))
	if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.RootFSPath, []byte("backup-rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.SerialLogPath, []byte("backup-serial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	backup, err := p.CreateBackup(ctx, inst.Name)
	if err != nil {
		t.Fatal(err)
	}

	// Change live metadata after the backup so the retry's record update is
	// observable independently of RestoreBackup's return value.
	inst.MemoryMiB += 512
	inst.VCPUCount++
	inst.UpdatedAt = inst.UpdatedAt.Add(time.Minute)
	if err := st.UpdateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.RootFSPath, []byte("live-rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	backupSerial, err := os.ReadFile(filepath.Join(backup.Path, backupSerialLogName))
	if err != nil {
		t.Fatal(err)
	}
	// Reachable crash prefix: serial was replaced, rootfs was not.
	if err := os.WriteFile(inst.SerialLogPath, backupSerial, 0o644); err != nil {
		t.Fatal(err)
	}

	restored, _, err := p.RestoreBackup(ctx, inst.Name, backup.ID)
	if err != nil {
		t.Fatalf("retry restore: %v", err)
	}
	rootfs, err := os.ReadFile(inst.RootFSPath)
	if err != nil || string(rootfs) != "backup-rootfs" {
		t.Fatalf("rootfs after retry = %q, %v", rootfs, err)
	}
	serial, err := os.ReadFile(inst.SerialLogPath)
	if err != nil || string(serial) != "backup-serial\n" {
		t.Fatalf("serial after retry = %q, %v", serial, err)
	}
	stored, err := st.GetInstance(ctx, inst.Name)
	if err != nil || stored.State != model.StateStopped {
		t.Fatalf("record after retry = %+v, %v", stored, err)
	}
	if stored.MemoryMiB != backup.MemoryMiB || stored.VCPUCount != backup.VCPUCount || stored.RootFSSizeBytes != backup.RootFSSizeBytes {
		t.Fatalf("record sizing = memory %d vcpu %d rootfs %d, want %d/%d/%d",
			stored.MemoryMiB, stored.VCPUCount, stored.RootFSSizeBytes,
			backup.MemoryMiB, backup.VCPUCount, backup.RootFSSizeBytes)
	}
	if restored.MemoryMiB != backup.MemoryMiB {
		t.Fatalf("restored memory = %d, want %d", restored.MemoryMiB, backup.MemoryMiB)
	}
	for _, pattern := range []string{".srv-restore-*", ".srv-restore-old-*"} {
		if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(inst.RootFSPath), pattern)); len(leftovers) != 0 {
			t.Fatalf("restore leftovers after retry (%s): %v", pattern, leftovers)
		}
	}
}

// An imported receipt without its rootfs must fail before the runner is called,
// not boot an empty instance.
func TestImportedReceiptWithoutRootFSDoesNotStart(t *testing.T) {
	ctx := context.Background()
	cfg := loadProvisionTestConfig(t, nil)
	st := newProvisionTestStore(t, cfg)
	inst := provisionTestInstance(cfg, "imported-missing", model.StateStopped, time.Date(2026, 4, 5, 13, 0, 0, 0, time.UTC))
	inst.Kind = model.KindOrb
	inst.InitrdPath = ""
	if err := os.MkdirAll(cfg.ImagesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.KernelPath, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if err := st.PutImportReceipt(ctx, model.ImportReceipt{InstanceID: inst.ID, Phase: "imported"}); err != nil {
		t.Fatal(err)
	}
	runner := &recordingVMRunner{}
	p := &Provisioner{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), vmRunner: runner, networkHelper: noopNetworkHelper{}}

	_, err := p.Start(ctx, inst.Name)
	if err == nil || !strings.Contains(err.Error(), "required file") {
		t.Fatalf("Start() error = %v, want a missing rootfs", err)
	}
	if len(runner.started) != 0 {
		t.Fatalf("runner launched %d times without a rootfs", len(runner.started))
	}
	stored, err := st.GetInstance(ctx, inst.Name)
	if err != nil || stored.State != model.StateStopped {
		t.Fatalf("instance changed after refused start: %+v, %v", stored, err)
	}
}
