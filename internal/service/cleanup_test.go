package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"srv/internal/model"
	"srv/internal/provision"
)

func TestBoundOrbMutationsRejectReplacementBeforeEffects(t *testing.T) {
	for _, change := range []string{"uuid", "created-at", "backend", "image", "legacy-vm"} {
		t.Run(change, func(t *testing.T) {
			st := newServiceTestStore(t)
			i := serviceTestInstance("owned", model.StateStopped, time.Date(2026, 1, 2, 0, 0, 0, 1, time.UTC))
			i.ID, i.Kind, i.Image = "12345678-1234-4234-8234-123456789abc", model.KindOrb, "amp-orb"
			identity := []string{i.ID, i.CreatedAt.Format(time.RFC3339Nano), "backend-one", i.Image}
			switch change {
			case "uuid":
				identity[0] = "22345678-1234-4234-8234-123456789abc"
			case "created-at":
				identity[1] = i.CreatedAt.Add(time.Nanosecond).Format(time.RFC3339Nano)
			case "backend":
				identity[2] = "backend-other"
			case "image":
				identity[3] = "other-image"
			case "legacy-vm":
				i.Kind = model.KindVM
			}
			if err := st.CreateInstance(context.Background(), i); err != nil {
				t.Fatal(err)
			}
			// Nil provisioner ensures every rejection is before runtime effects.
			a := &App{store: st, backendID: "backend-one"}
			actor := model.Actor{UserLogin: i.CreatedByUser}
			for _, operation := range []string{"start", "stop", "delete-proof"} {
				args := append([]string{operation, i.Name, "--expect", "32345678-1234-4234-8234-123456789abc"}, identity...)
				if operation == "delete-proof" {
					args = append([]string{operation, "32345678-1234-4234-8234-123456789abc", i.Name}, identity...)
				}
				if _, err := a.dispatch(context.Background(), actor, commandRequest{args: args, format: outputFormatJSON}); err == nil {
					t.Fatal(operation, "accepted replacement")
				}
			}
		})
	}
}

func TestOrbCancellationAdmissionOrders(t *testing.T) {
	for _, order := range []string{"cancel-first", "execute-first", "race"} {
		t.Run(order, func(t *testing.T) {
			ctx := context.Background()
			st := newServiceTestStore(t)
			i := serviceTestInstance("owned", model.StateReady, time.Now().UTC())
			i.ID, i.Kind, i.Image = uuid.NewString(), model.KindOrb, "amp-orb"
			if err := st.CreateInstance(ctx, i); err != nil {
				t.Fatal(err)
			}
			dir, err := os.MkdirTemp("", "srv-op-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			path := filepath.Join(dir, "helper.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "{}") })}
			go server.Serve(listener)
			defer server.Close()
			cfg := loadServiceTestConfig(t, nil)
			cfg.VMRunnerSocketPath, cfg.NetHelperSocketPath, cfg.OutboundInterface = path, path, "fixture0"
			a := &App{store: st, backendID: "backend-one", cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			a.provisioner, err = provision.New(cfg, a.log, st)
			if err != nil {
				t.Fatal(err)
			}
			actor := model.Actor{UserLogin: i.CreatedByUser}
			id := uuid.NewString()
			identity := []string{i.ID, i.CreatedAt.Format(time.RFC3339Nano), a.backendID, i.Image}
			stop := func() error {
				_, err := a.cmdStop(ctx, actor, append([]string{"stop", i.Name, "--expect", id}, identity...), outputFormatJSON)
				return err
			}
			cancel := func() error {
				_, err := a.cmdOperationResult(ctx, actor, append([]string{"operation-cancel", id, i.Name, "stop"}, identity...))
				return err
			}
			var stopErr error
			switch order {
			case "cancel-first":
				if err := cancel(); err != nil {
					t.Fatal(err)
				}
				stopErr = stop()
			case "execute-first":
				stopErr = stop()
				if err := cancel(); err != nil {
					t.Fatal(err)
				}
			case "race":
				unlock := a.lockInstance(i.Name)
				s, c := make(chan error, 1), make(chan error, 1)
				go func() { s <- stop() }()
				go func() { c <- cancel() }()
				unlock()
				stopErr = <-s
				if err := <-c; err != nil {
					t.Fatal(err)
				}
			}
			r, err := st.GetLifecycleRequest(ctx, id, actor.UserLogin)
			if err != nil {
				t.Fatal(err)
			}
			current, err := st.GetInstance(ctx, i.Name)
			if err != nil {
				t.Fatal(err)
			}
			if r.Phase == "cancelled" {
				if stopErr == nil || current.State != model.StateReady {
					t.Fatal("cancelled worker applied effects", stopErr, current.State)
				}
			} else if r.Phase != "done" || stopErr != nil || current.State != model.StateStopped {
				t.Fatal(r, stopErr, current.State)
			}
			if err := stop(); err == nil {
				t.Fatal("late duplicate replayed")
			}
			if _, err := a.cmdOperationResult(ctx, actor, []string{"operation-result", id}); err != nil {
				t.Fatal("stored result unreadable", err)
			}
		})
	}
}

func TestOrbBootstrapTerminalReceiptRequiresGenerationAck(t *testing.T) {
	for _, firstAck := range []string{"lost", "old-agent", "done"} {
		t.Run(firstAck, func(t *testing.T) {
			ctx := context.Background()
			st := newServiceTestStore(t)
			i := serviceTestInstance("owned", model.StateReady, time.Now().UTC())
			i.ID, i.Kind, i.Image = uuid.NewString(), model.KindOrb, "amp-orb"
			dir, err := os.MkdirTemp("", "srv-guest-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			i.RootFSPath = filepath.Join(dir, "rootfs")
			if err := st.CreateInstance(ctx, i); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(dir, "orb.vsock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			served := make(chan error, 1)
			go func() {
				for generation := 1; generation <= 2; generation++ {
					conn, err := listener.Accept()
					if err != nil {
						served <- err
						return
					}
					reader := bufio.NewReader(conn)
					line, _ := reader.ReadString('\n')
					if line != "CONNECT 4050\n" {
						conn.Close()
						served <- fmt.Errorf("handshake %q", line)
						return
					}
					io.WriteString(conn, "OK 4050\n")
					line, _ = reader.ReadString('\n')
					var request struct {
						Generation int64 `json:"generation"`
					}
					if json.Unmarshal([]byte(line), &request) != nil || request.Generation != int64(generation) {
						conn.Close()
						served <- fmt.Errorf("generation mismatch")
						return
					}
					if generation == 1 && firstAck == "old-agent" {
						io.WriteString(conn, "{\"ok\":true}\n")
					} else if generation != 1 || firstAck == "done" {
						fmt.Fprintf(conn, "{\"ok\":true,\"generation\":%d}\n", generation)
					}
					conn.Close()
				}
				served <- nil
			}()
			a := &App{store: st, backendID: "backend-one"}
			actor := model.Actor{UserLogin: i.CreatedByUser}
			ids := []string{uuid.NewString(), uuid.NewString()}
			call := func(id string, generation int) error {
				args := []string{"orb", "bootstrap", i.Name, "--expect", id, i.ID, i.CreatedAt.Format(time.RFC3339Nano), a.backendID, i.Image}
				session := newServiceTestSession(ctx, fmt.Sprintf(`{"token":"synthetic","generation":%d}`, generation), false, args)
				_, err := a.handleOrbBootstrapSession(session, actor, args)
				return err
			}
			if err := call(ids[0], 1); (err == nil) != (firstAck == "done") {
				t.Fatal(err)
			}
			if err := st.InterruptLifecycleRequests(ctx); err != nil {
				t.Fatal(err)
			}
			if err := call(ids[1], 2); err != nil {
				t.Fatal(err)
			}
			if err := <-served; err != nil {
				t.Fatal(err)
			}
			if err := call(uuid.NewString(), 1); err == nil {
				t.Fatal("late old generation readmitted")
			}
			if err := call(ids[1], 2); err == nil {
				t.Fatal("equal generation reexecuted")
			}
			for index, id := range ids {
				r, err := st.GetLifecycleRequest(ctx, id, actor.UserLogin)
				want := "done"
				if index == 0 && firstAck != "done" {
					want = "interrupted"
				}
				if err != nil || r.Phase != want {
					t.Fatal(r, want, err)
				}
				if _, err := a.cmdOperationResult(ctx, actor, []string{"operation-result", id}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
