package service

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"srv/internal/model"
	"srv/internal/provision"
	"srv/internal/vmrunner"
)

// Keep the real vmrunner.Client transport and its strict wire validation;
// only the privileged runner is replaced. No journal means hot, not proof
// that the caller's local rootfs exists.
func serviceTestHotColdDiskSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "srv-cold-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "runner.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q vmrunner.ColdRequest
		if r.URL.Path != "/vm/cold-disk-v1" || json.NewDecoder(r.Body).Decode(&q) != nil || q.Action != "status" || q.Name == "" || q.InstanceID == "" {
			t.Errorf("unexpected runner request: %s %+v", r.URL.Path, q)
			http.Error(w, "unexpected runner request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(vmrunner.ColdStatus{Phase: "hot", Remote: false, JournalPresent: false})
	})}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); <-done })
	return socket
}

func TestColdDiskMutationsFailClosedWithoutRunner(t *testing.T) {
	for _, args := range [][]string{
		{"resize", "alpha", "--cpus", "4"},
		{"fsck", "alpha"},
		{"migration", "prepare", "alpha", "destination", "token"},
	} {
		t.Run(args[0], func(t *testing.T) {
			ctx := context.Background()
			cfg := loadServiceTestConfig(t, nil)
			cfg.VMRunnerSocketPath = filepath.Join(t.TempDir(), "absent.sock")
			st := newServiceTestStore(t)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			p, err := provision.New(cfg, logger, st)
			if err != nil {
				t.Fatal(err)
			}
			a := &App{cfg: cfg, store: st, log: logger, provisioner: p}
			inst := serviceTestInstance("alpha", model.StateStopped, time.Now().UTC())
			inst.RootFSPath = filepath.Join(t.TempDir(), "rootfs.img")
			if err := os.WriteFile(inst.RootFSPath, []byte("rootfs"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateInstance(ctx, inst); err != nil {
				t.Fatal(err)
			}
			before, err := st.GetInstance(ctx, inst.Name)
			if err != nil {
				t.Fatal(err)
			}
			result, err := a.dispatch(ctx, model.Actor{UserLogin: inst.CreatedByUser}, commandRequest{args: args})
			if err == nil || !strings.Contains(err.Error(), "call vm runner") || result.exitCode == 0 {
				t.Fatalf("unavailable authoritative runner must reject mutation: %+v %v", result, err)
			}
			after, err := st.GetInstance(ctx, inst.Name)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("failed mutation changed instance: before=%+v after=%+v err=%v", before, after, err)
			}
			data, err := os.ReadFile(inst.RootFSPath)
			if err != nil || string(data) != "rootfs" {
				t.Fatalf("failed mutation changed rootfs: %q %v", data, err)
			}
		})
	}
}

func TestSnapshotRejectsAllColdMarkers(t *testing.T) {
	for _, marker := range []string{`{"phase":"hot","remote":true}`, `{"phase":"cold","remote":true}`, "invalid"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "cold-disk.json"), []byte(marker), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := snapshotDiskImage(dir, true, func(string) (bool, error) { t.Fatal("must reject before probing disk"); return false, nil })
		if err == nil || !strings.Contains(err.Error(), "cold disk marker") {
			t.Fatalf("marker %s: %v", marker, err)
		}
	}
}
