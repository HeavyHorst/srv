package provision

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"srv/internal/host"
	"srv/internal/model"
	"srv/internal/storage"
)

func TestImportCleanupPreservesInheritedLocks(t *testing.T) {
	for _, phase := range []string{"early", "deferred", "reused-incarnation"} {
		t.Run(phase, func(t *testing.T) {
			cfg := loadProvisionTestConfig(t, nil)
			st := newProvisionTestStore(t, cfg)
			p := &Provisioner{cfg: cfg, store: st, vmRunner: noopVMRunner{}, readFilesystemUsage: host.DefaultReadFilesystemUsage}
			parentLock, err := os.CreateTemp(t.TempDir(), "parent-lock")
			if err != nil {
				t.Fatal(err)
			}
			defer parentLock.Close()
			ctx, cancel := context.WithCancel(storage.WithInheritedLock(context.Background(), parentLock))
			defer cancel()
			inst := provisionTestInstance(cfg, "cleanup", model.StateFailed, time.Now().UTC())
			inst.RootFSSizeBytes = 6
			instanceDir := filepath.Join(cfg.InstancesDir(), inst.Name)
			if err := os.MkdirAll(cfg.InstancesDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			if phase == "reused-incarnation" {
				if err := st.CreateInstance(ctx, inst); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(inst.RootFSPath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(inst.RootFSPath, []byte("stale"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			// Observe the context at the actual production cleanup boundary, then
			// run RemoveTree on real directories. No Btrfs host is needed here.
			oldRemove := removeTree
			t.Cleanup(func() { removeTree = oldRemove })
			calls := 0
			removeTree = func(cleanupCtx context.Context, path string) error {
				calls++
				if path != instanceDir {
					t.Errorf("cleanup path = %q, want %q", path, instanceDir)
				}
				if err := cleanupCtx.Err(); err != nil {
					t.Errorf("cleanup inherited cancellation: %v", err)
				}
				deadline, ok := cleanupCtx.Deadline()
				if remaining := time.Until(deadline); !ok || remaining <= 0 || remaining > 30*time.Second {
					t.Errorf("cleanup is not bounded: deadline %v, present %v", deadline, ok)
				}
				cmd := exec.Command("true")
				storage.InheritLocks(cleanupCtx, cmd)
				if len(cmd.ExtraFiles) != 2 || cmd.ExtraFiles[0] != parentLock {
					t.Errorf("cleanup lost parent/payload locks: %v", cmd.ExtraFiles)
				}
				for _, f := range cmd.ExtraFiles {
					if _, err := f.Stat(); err != nil {
						t.Errorf("lock released before cleanup: %v", err)
					}
				}
				return oldRemove(cleanupCtx, path)
			}

			manifest := portableManifestFromInstance(time.Now().UTC(), inst)
			files := map[string][]byte{backupRootFSName: []byte("rootfs")}
			failure := "injected subvolume failure"
			var progress func(ImportProgress)
			if phase == "deferred" {
				// Cancel after reserving the row, then reject the incomplete archive.
				manifest.Files.SerialLog = backupSerialLogName
				failure = "missing serial log"
				progress = func(ImportProgress) { cancel() }
			} else {
				create := storage.CreateSubvolume
				t.Cleanup(func() { storage.CreateSubvolume = create })
				storage.CreateSubvolume = func(context.Context, string) error {
					cancel()
					return errors.New(failure)
				}
			}
			artifact := portableArtifactTestStream(t, manifest, files)
			if _, _, err := p.ImportInstance(ctx, model.Actor{}, bytes.NewReader(artifact), progress); err == nil || !strings.Contains(err.Error(), failure) {
				t.Fatalf("ImportInstance error = %v, want %q", err, failure)
			}
			wantCalls := 1
			if phase == "reused-incarnation" {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("cleanup calls = %d, want %d", calls, wantCalls)
			}
			if _, err := os.Stat(instanceDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("instance directory survived cleanup: %v", err)
			}
			if _, found, err := st.FindInstance(context.Background(), inst.Name); err != nil || found {
				t.Fatalf("instance reservation survived cleanup: found %v, error %v", found, err)
			}
		})
	}
}
