package provision

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"modernc.org/sqlite"

	"srv/internal/model"
)

func TestRestoreRollbackPreservesDestinationWhenOriginalIsMissing(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "rootfs.img")
	if err := os.WriteFile(dest, []byte("restored disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := rollbackCommittedRestoreFiles([]replacedRestoreFile{{
		dest: dest, original: filepath.Join(dir, "missing-original"), hadFile: true,
	}})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback error = %v, want missing original", err)
	}
	payload, err := os.ReadFile(dest)
	if err != nil || string(payload) != "restored disk" {
		t.Fatalf("destination after failed rollback = %q, %v", payload, err)
	}
}

func TestRestoreRollbackDoesNotRemoveConflictingDirectory(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "rootfs.img")
	original := filepath.Join(dir, ".srv-restore-old-original")
	if err := os.WriteFile(original, []byte("live disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := rollbackCommittedRestoreFiles([]replacedRestoreFile{{dest: dest, original: original, hadFile: true}}); err == nil {
		t.Fatal("rollback unexpectedly removed a conflicting directory")
	}
	if info, err := os.Stat(dest); err != nil || !info.IsDir() {
		t.Fatalf("conflicting directory not preserved: %v", err)
	}
	payload, err := os.ReadFile(original)
	if err != nil || string(payload) != "live disk" {
		t.Fatalf("recovery copy = %q, %v", payload, err)
	}
}

func TestRestoreRollbackRestoresOriginalInodeAndRemovesNewFiles(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "rootfs.img")
	newDest := filepath.Join(dir, "serial.log")
	if err := os.WriteFile(dest, []byte("live disk"), 0o640); err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	var staged []stagedRestoreFile
	for _, path := range []string{dest, newDest} {
		file, err := prepareStagedRestoreFileFromReader(strings.NewReader("backup"), path, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		staged = append(staged, file)
	}
	replaced, err := commitStagedRestoreFiles(staged)
	if err != nil {
		t.Fatal(err)
	}
	if err := rollbackCommittedRestoreFiles(replaced); err != nil {
		t.Fatal(err)
	}
	if err := cleanupReplacedRestoreFiles(replaced); err != nil {
		t.Fatal(err)
	}
	restoredInfo, err := os.Stat(dest)
	if err != nil || !os.SameFile(originalInfo, restoredInfo) || restoredInfo.Mode().Perm() != 0o640 {
		t.Fatalf("original inode and permissions not restored: %v", err)
	}
	if _, err := os.Stat(newDest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new destination survived rollback: %v", err)
	}
}

func TestRestoreCommitErrorsCleanUpOnlyAfterRollback(t *testing.T) {
	for _, failure := range []string{"destination-directory", "missing-stage"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "serial.log")
			blocked := filepath.Join(dir, "rootfs.img")
			if err := os.WriteFile(dest, []byte("live serial"), 0o600); err != nil {
				t.Fatal(err)
			}
			if failure == "destination-directory" {
				if err := os.Mkdir(blocked, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(blocked, []byte("live disk"), 0o600); err != nil {
				t.Fatal(err)
			}
			stage, err := prepareStagedRestoreFileFromReader(strings.NewReader("backup serial"), dest, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = commitStagedRestoreFiles([]stagedRestoreFile{stage, {dest: blocked, tmp: filepath.Join(dir, "missing-stage")}})
			if err == nil {
				t.Fatal("commit unexpectedly succeeded")
			}
			payload, err := os.ReadFile(dest)
			if err != nil || string(payload) != "live serial" {
				t.Fatalf("earlier destination = %q, %v", payload, err)
			}
			if failure == "missing-stage" {
				payload, err := os.ReadFile(blocked)
				if err != nil || string(payload) != "live disk" {
					t.Fatalf("failed destination = %q, %v", payload, err)
				}
			}
			copies, err := filepath.Glob(filepath.Join(dir, ".srv-restore-old-*"))
			if err != nil || len(copies) != 0 {
				t.Fatalf("rollback copy residue = %v, %v", copies, err)
			}
		})
	}
}

func TestRestoreCommitFailureKeepsCopiesWhenEarlierRollbackFails(t *testing.T) {
	dir := t.TempDir()
	stageDir := filepath.Join(dir, "stage")
	if err := os.Mkdir(stageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, "blocker"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "rootfs.img")
	if err := os.WriteFile(dest, []byte("live disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A nonempty directory substituted for a stage makes the first rollback
	// fail deterministically, without permissions, races, or syscall hooks.
	_, err := commitStagedRestoreFiles([]stagedRestoreFile{
		{dest: filepath.Join(dir, "new-destination"), tmp: stageDir},
		{dest: dest, tmp: filepath.Join(dir, "missing-stage")},
	})
	if err == nil || !strings.Contains(err.Error(), "remove restored destination") {
		t.Fatalf("commit/rollback error = %v", err)
	}
	copies, err := filepath.Glob(filepath.Join(dir, ".srv-restore-old-*"))
	if err != nil || len(copies) != 1 {
		t.Fatalf("failed rollback must keep recovery copies: %v, %v", copies, err)
	}
	payload, err := os.ReadFile(copies[0])
	if err != nil || string(payload) != "live disk" {
		t.Fatalf("recovery copy = %q, %v", payload, err)
	}
}

func TestRestoreBackupDatabaseFailurePreservesRecovery(t *testing.T) {
	for _, obstructRollback := range []bool{false, true} {
		name := "rollback-succeeds"
		if obstructRollback {
			name = "rollback-fails"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg := loadProvisionTestConfig(t, nil)
			inst := provisionTestInstance(cfg, "restore-db", model.StateStopped, time.Now().UTC())
			// The real SQLite UPDATE invokes this test-only failure injection after
			// all files are committed. No production filesystem or DB seams needed.
			function := "restore_failure_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			if err := sqlite.RegisterScalarFunction(function, 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
				if obstructRollback {
					if err := os.Remove(inst.SerialLogPath); err != nil {
						return nil, err
					}
					if err := os.Mkdir(inst.SerialLogPath, 0o700); err != nil {
						return nil, err
					}
					if err := os.WriteFile(filepath.Join(inst.SerialLogPath, "blocker"), []byte("keep"), 0o600); err != nil {
						return nil, err
					}
				}
				return nil, errors.New("simulated restore database failure")
			}); err != nil {
				t.Fatal(err)
			}
			st := newProvisionTestStore(t, cfg)
			p := &Provisioner{cfg: cfg, store: st}
			oldClone := reflinkCloneFile
			t.Cleanup(func() { reflinkCloneFile = oldClone })
			reflinkCloneFile = func(_ context.Context, src, dest string) error {
				payload, err := os.ReadFile(src)
				if err != nil {
					return err
				}
				return os.WriteFile(dest, payload, 0o600)
			}
			if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{inst.RootFSPath, inst.SerialLogPath, inst.LogPath} {
				if err := os.WriteFile(path, []byte("backup"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.CreateInstance(ctx, inst); err != nil {
				t.Fatal(err)
			}
			backup, err := p.CreateBackup(ctx, inst.Name)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{inst.RootFSPath, inst.SerialLogPath, inst.LogPath} {
				if err := os.WriteFile(path, []byte("live"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			db, err := sql.Open("sqlite", cfg.DatabasePath())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`CREATE TRIGGER fail_restore BEFORE UPDATE ON instances BEGIN SELECT ` + function + `(); END`); err != nil {
				t.Fatal(err)
			}
			_, _, err = p.RestoreBackup(ctx, inst.Name, backup.ID)
			if err == nil || !strings.Contains(err.Error(), "simulated restore database failure") {
				t.Fatalf("RestoreBackup error = %v", err)
			}
			if strings.Contains(err.Error(), "file rollback failed") != obstructRollback {
				t.Fatalf("RestoreBackup rollback result = %v", err)
			}
			for _, path := range []string{inst.RootFSPath, inst.SerialLogPath, inst.LogPath} {
				if obstructRollback && path == inst.SerialLogPath {
					continue
				}
				payload, err := os.ReadFile(path)
				if err != nil || string(payload) != "live" {
					t.Fatalf("destination %s = %q, %v", path, payload, err)
				}
			}
			copies, err := filepath.Glob(filepath.Join(filepath.Dir(inst.SerialLogPath), ".srv-restore-old-*"))
			if err != nil {
				t.Fatal(err)
			}
			if obstructRollback {
				if len(copies) != 1 {
					t.Fatalf("recovery copy must survive failed rollback, found %v", copies)
				}
				payload, err := os.ReadFile(copies[0])
				if err != nil || string(payload) != "live" {
					t.Fatalf("recovery copy = %q, %v", payload, err)
				}
			} else if len(copies) != 0 {
				t.Fatalf("successful rollback left recovery copies: %v", copies)
			}
			stored, err := st.GetInstance(ctx, inst.Name)
			if err != nil || !stored.UpdatedAt.Equal(inst.UpdatedAt) {
				t.Fatalf("failed restore changed database: %#v, %v", stored, err)
			}
		})
	}
}
