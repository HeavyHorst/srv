package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"srv/internal/model"
	"srv/internal/provision"
)

func TestFailedSaveStateStopExplicitRecovery(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	dir, err := os.MkdirTemp("", "srv-recover-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "runner.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var completed atomic.Bool
	var stops, recoveries atomic.Int32
	helperEntered, releaseHelper := make(chan struct{}), make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vm/start-result":
			if completed.Load() {
				io.WriteString(w, `{"outcome":"absent"}`)
			} else {
				fmt.Fprintf(w, `{"outcome":"running","pid":%d}`, os.Getpid())
			}
		case "/vm/stop-state-v1":
			stops.Add(1)
			http.Error(w, "insufficient disk space for full RAM state plus 256 MiB headroom", http.StatusInternalServerError)
		case "/vm/stop-recovery-v1":
			call := recoveries.Add(1)
			var q map[string]any
			if json.NewDecoder(r.Body).Decode(&q) != nil || q["save_state"] != true || q["require_saved_state"] != true || q["expected_memory_bytes"] != float64(128<<20) {
				http.Error(w, "missing strict proof prerequisite", 500)
				return
			}
			completed.Store(true)
			if call == 1 {
				http.Error(w, "lost completion reply", 500)
				return
			}
			if call == 2 {
				close(helperEntered)
				<-releaseHelper
			}
			io.WriteString(w, `{}`)
		default:
			io.WriteString(w, `{}`)
		}
	})}
	go server.Serve(listener)
	defer server.Close()
	cfg := loadServiceTestConfig(t, nil)
	cfg.AdminUsers = nil
	cfg.VMRunnerSocketPath, cfg.NetHelperSocketPath, cfg.OutboundInterface = socket, socket, "fixture0"
	a := &App{store: st, backendID: "backend-one", cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.provisioner, err = provision.New(cfg, a.log, st)
	if err != nil {
		t.Fatal(err)
	}
	inst := serviceTestInstance("recover-orb", model.StateReady, time.Now().UTC())
	inst.ID, inst.Kind, inst.Image, inst.CreatedByUser = uuid.NewString(), model.KindOrb, "amp-orb", "owner"
	inst.StartRequestID, inst.StartBootID, inst.FirecrackerPID, inst.MemoryMiB = uuid.NewString(), "original-host-boot", os.Getpid(), 128
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	actor := model.Actor{UserLogin: "owner"}
	oldID := uuid.NewString()
	identity := []string{inst.ID, inst.CreatedAt.Format(time.RFC3339Nano), a.backendID, inst.Image}
	stopArgs := append([]string{"stop", inst.Name, "--save-state", "--expect", oldID}, identity...)
	if _, err := a.dispatch(ctx, actor, commandRequest{args: stopArgs}); err == nil || stops.Load() != 1 {
		t.Fatal("original disk-full stop:", err)
	}
	old, err := st.GetLifecycleRequest(ctx, oldID, actor.UserLogin)
	if err != nil || old.Phase != "executing" {
		t.Fatal("original fence lost", old, err)
	}
	if err := st.InterruptLifecycleRequests(ctx); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"operation-reconcile-stop", uuid.NewString(), oldID, inst.Name, "--save-state", "--expect"}, identity...)
	args = append(args, "--attempt", inst.StartRequestID, inst.StartBootID, "--generation", "0")
	a.cfg.AdminUsers = []string{"other-owner"}
	if _, err := a.dispatch(ctx, model.Actor{UserLogin: "other-owner"}, commandRequest{args: args}); err == nil {
		t.Fatal("another actor recovered the original owner's request")
	}
	a.cfg.AdminUsers = nil
	for _, index := range []int{6, 8, 9, 11, 12, 14} {
		bad := append([]string(nil), args...)
		if index == 6 || index == 11 {
			bad[index] = uuid.NewString()
		} else if index == 14 {
			bad[index] = "1"
		} else {
			bad[index] = "wrong"
		}
		if _, err := a.dispatch(ctx, actor, commandRequest{args: bad}); err == nil {
			t.Fatalf("binding index %d accepted", index)
		}
	}
	if recoveries.Load() != 0 {
		t.Fatal("binding failure reached helper")
	}
	if _, err := a.dispatch(ctx, actor, commandRequest{args: args}); err == nil || !strings.Contains(err.Error(), "vm runner request failed: 500") {
		t.Fatal("expected recovery to reach runner before lost reply:", err)
	}
	stoppedAfterLostReply, err := st.GetInstance(ctx, inst.Name)
	if err != nil {
		t.Fatal(err)
	}
	stoppedAfterLostReply.State, stoppedAfterLostReply.FirecrackerPID = model.StateStopped, 0
	if err := st.UpdateInstance(ctx, stoppedAfterLostReply); err != nil {
		t.Fatal(err)
	}
	if _, err := a.provisioner.Start(ctx, inst.Name); err == nil {
		t.Fatal("pending recovery permitted a new start")
	}
	if err := st.InterruptLifecycleRequests(ctx); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() { _, err := a.dispatch(ctx, actor, commandRequest{args: args}); results <- err }()
	select {
	case <-helperEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not enter helper")
	}
	go func() { _, err := a.dispatch(ctx, actor, commandRequest{args: args}); results <- err }()
	close(releaseHelper)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal("concurrent recovery:", err)
		}
	}
	old, err = st.GetLifecycleRequest(ctx, oldID, actor.UserLogin)
	current, getErr := st.GetInstance(ctx, inst.Name)
	if err != nil || getErr != nil || old.Phase != "recovered" || current.State != model.StateStopped || current.FirecrackerPID != 0 {
		t.Fatal("recovery did not publish linked completion", old, current, err, getErr)
	}
	if _, err := a.dispatch(ctx, actor, commandRequest{args: args}); err != nil {
		t.Fatal("recovery retry:", err)
	}
	if stops.Load() != 1 || recoveries.Load() != 2 {
		t.Fatal("retry replayed effects", stops.Load(), recoveries.Load())
	}
	if _, err := a.dispatch(ctx, actor, commandRequest{args: stopArgs}); err == nil {
		t.Fatal("original stop replayed")
	}
	if _, err := a.cmdOperationResult(ctx, actor, []string{"operation-result", oldID}); err != nil {
		t.Fatal(err)
	}
	bootstrap := orbOperationRequest(actor, "bootstrap", inst.Name, uuid.NewString(), append(identity, "1"))
	if err := st.AdmitBootstrap(ctx, bootstrap, inst.ID, inst.CreatedAt, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := a.cmdOperationResult(ctx, actor, []string{"operation-result", oldID}); err == nil {
		t.Fatal("new bootstrap accepted old recovery proof")
	}
	current.StartRequestID = uuid.NewString()
	if err := st.UpdateInstance(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := a.cmdOperationResult(ctx, actor, []string{"operation-result", oldID}); err == nil {
		t.Fatal("successor accepted old recovery proof")
	}
	if _, err := a.dispatch(ctx, actor, commandRequest{args: args}); err == nil || errors.Is(err, context.Canceled) {
		t.Fatal("successor recovery was not rejected", err)
	}
}
