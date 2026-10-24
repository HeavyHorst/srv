package service

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"srv/internal/config"
	"srv/internal/model"
	"srv/internal/provision"
	"srv/internal/store"
)

func TestLegacyOrbInspectDoesNotAdoptBootstrapCounter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	i := serviceTestInstance("legacy-orb", model.StateReady, time.Now().UTC())
	i.ID, i.Image = uuid.NewString(), "amp-orb"
	// Build the pre-counter representation using public store operations:
	// UpdateInstance does not initialize a bootstrap counter.
	i.Kind = model.KindVM
	if err := st.CreateInstance(ctx, i); err != nil {
		t.Fatal(err)
	}
	i.Kind = model.KindOrb
	if err := st.UpdateInstance(ctx, i); err != nil {
		t.Fatal(err)
	}
	a := &App{store: st, cfg: loadServiceTestConfig(t, nil), backendID: "backend-one"}
	actor := model.Actor{UserLogin: i.CreatedByUser}
	result, err := a.dispatch(ctx, actor, commandRequest{args: []string{"inspect", i.Name}, format: outputFormatJSON})
	if err != nil || result.exitCode != 0 {
		t.Fatal("legacy diagnostics refused", result, err)
	}
	var payload struct {
		Instance map[string]any `json:"instance"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Instance["id"] != i.ID || payload.Instance["kind"] != model.KindOrb || payload.Instance["bootstrap_generation"] != nil {
		t.Fatal("counter fabricated or identity lost", payload)
	}
	id := uuid.NewString()
	args := []string{"orb", "bootstrap", i.Name, "--expect", id, i.ID, i.CreatedAt.Format(time.RFC3339Nano), a.backendID, i.Image}
	session := newServiceTestSession(ctx, `{"token":"synthetic","generation":1}`, false, args)
	if code, err := a.handleOrbBootstrapSession(session, actor, args); err == nil || code == 0 {
		t.Fatal("legacy bootstrap admitted", code, err)
	}
	if _, err := st.BootstrapGeneration(ctx, i.ID, i.CreatedAt); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("missing counter initialized", err)
	}
	if _, err := st.GetLifecycleRequest(ctx, id, actor.UserLogin); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("legacy request admitted", err)
	}
	// Only ErrNoRows is diagnostic-compatible; schema/I/O errors are not
	// silently converted into an unknown generation.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE orb_bootstrap`); err != nil {
		t.Fatal(err)
	}
	if result, err := a.dispatch(ctx, actor, commandRequest{args: []string{"inspect", i.Name}, format: outputFormatJSON}); err == nil || result.exitCode == 0 {
		t.Fatal("database error hidden", result, err)
	}
}

func TestOrbBootstrapDecodesProxyBypass(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	i := serviceTestInstance("legacy-orb", model.StateReady, time.Now().UTC())
	i.ID, i.Kind, i.Image = uuid.NewString(), model.KindOrb, "amp-orb"
	if err := st.CreateInstance(ctx, i); err != nil {
		t.Fatal(err)
	}
	a := &App{store: st, cfg: loadServiceTestConfig(t, nil), backendID: "backend-one"}
	actor := model.Actor{UserLogin: i.CreatedByUser}
	args := []string{"orb", "bootstrap", i.Name, "--expect", uuid.NewString(), i.ID, i.CreatedAt.Format(time.RFC3339Nano), a.backendID, i.Image}
	// The relay decodes stdin with DisallowUnknownFields: the optional
	// proxy_bypass field must pass decoding (and fail later, like any legacy
	// bootstrap, at admission), while unknown fields fail at decoding.
	for _, scenario := range []struct{ payload, want string }{
		{`{"token":"synthetic","generation":1,"proxy_bypass":".corp.example"}`, "anything-but-decode"},
		{`{"token":"synthetic","generation":1,"no_proxy":".corp.example"}`, "invalid orb bootstrap request"},
		{`{"token":"synthetic","generation":1,"extra":true}`, "invalid orb bootstrap request"},
	} {
		session := newServiceTestSession(ctx, scenario.payload, false, args)
		code, err := a.handleOrbBootstrapSession(session, actor, args)
		if err == nil || code == 0 {
			t.Fatalf("legacy bootstrap admitted for %s", scenario.payload)
		}
		if scenario.want == "anything-but-decode" && err.Error() == "invalid orb bootstrap request" {
			t.Fatalf("proxy_bypass rejected at decode: %v", err)
		}
		if scenario.want != "anything-but-decode" && err.Error() != scenario.want {
			t.Fatalf("decode error = %q, want %q", err.Error(), scenario.want)
		}
	}
}

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

func TestDeleteProofQuiescesGatewaysBeforeReceipt(t *testing.T) {
	for _, scenario := range []string{"success", "backup-preflight", "late-dial", "cancelled-drain"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			cfg := loadServiceTestConfig(t, nil)
			st := newServiceTestStore(t)
			dir, err := os.MkdirTemp("", "srv-gateway-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, "helpers.sock")
			ln, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			// Only privileged runtime/network helpers are substituted. Deletion,
			// SQLite, disk removal and TCP gateway handling are the real stack.
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/vm/stop", "/vm/cleanup-proof-v1", "/network/cleanup", "/network/cleanup-proof-v1":
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected helper request: %s", r.URL.Path)
					http.Error(w, "unexpected", 400)
				}
			})}
			go server.Serve(ln)
			defer server.Close()
			cfg.VMRunnerSocketPath, cfg.NetHelperSocketPath, cfg.OutboundInterface = socket, socket, "fixture0"
			manager := newAmpConnectGatewayManager(config.Config{AmpConnectGatewayPort: 0}, nil)
			defer manager.Close()
			peers := make(chan net.Conn, 3)
			dialStarted, dialCancelled, releaseDial := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseDial) }) }
			defer release()
			manager.dial = func(ctx context.Context, _, address string) (net.Conn, error) {
				upstream, peer := net.Pipe()
				peers <- peer
				if address == "late.example:443" {
					close(dialStarted)
					<-ctx.Done()
					close(dialCancelled)
					<-releaseDial // Deliberately return a socket after cancellation.
				}
				return upstream, nil
			}
			a := &App{cfg: cfg, store: st, log: manager.log, backendID: "backend", ampConnectGateway: manager}
			a.provisioner, err = provision.New(cfg, a.log, st)
			if err != nil {
				t.Fatal(err)
			}
			var instances []model.Instance
			for _, name := range []string{"target", "neighbor"} {
				i := serviceTestInstance(name, model.StateReady, time.Now().UTC())
				i.ID, i.Kind, i.Image = uuid.NewString(), model.KindOrb, "amp-orb"
				i.StartRequestID, i.StartBootID = uuid.NewString(), "boot"
				i.HostAddr, i.GuestAddr = "127.0.0.1", "127.0.0.1"
				i.RootFSPath = filepath.Join(cfg.InstancesDir(), name, "rootfs.img")
				if err := os.MkdirAll(filepath.Dir(i.RootFSPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(i.RootFSPath, []byte(name), 0600); err != nil {
					t.Fatal(err)
				}
				if err := st.CreateInstance(ctx, i); err != nil {
					t.Fatal(err)
				}
				instances = append(instances, i)
			}
			a.syncManagedGatewaysBestEffort()
			i := instances[0]
			addr := manager.listeners[i.Name].ln.Addr().String()
			target := connectGateway(t, addr, "active.example:443", true)
			defer target.Close()
			targetPeer := <-peers
			defer targetPeer.Close()
			neighbor := connectGateway(t, manager.listeners["neighbor"].ln.Addr().String(), "active.example:443", true)
			defer neighbor.Close()
			neighborPeer := <-peers
			defer neighborPeer.Close()
			assertGatewayTraffic(t, target, targetPeer)
			if scenario == "backup-preflight" {
				if err := os.MkdirAll(filepath.Join(cfg.BackupsDir(), i.Name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			late := scenario == "late-dial" || scenario == "cancelled-drain"
			var lateGuest, latePeer net.Conn
			if late {
				lateGuest = connectGateway(t, addr, "late.example:443", false)
				defer lateGuest.Close()
				latePeer = <-peers
				defer latePeer.Close()
				<-dialStarted
			}
			id := uuid.NewString()
			actor := model.Actor{UserLogin: i.CreatedByUser}
			args := []string{"delete-proof", id, i.Name, i.ID, i.CreatedAt.Format(time.RFC3339Nano), a.backendID, i.Image}
			deleteCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := a.cmdDeleteProof(deleteCtx, actor, args); done <- err }()
			if late {
				select {
				case <-dialCancelled:
				case <-deleteCtx.Done():
					t.Fatal("gateway dial was not cancelled")
				}
				if _, err := st.CleanupReceipt(ctx, id, actor.UserLogin); !errors.Is(err, sql.ErrNoRows) {
					t.Fatal("receipt before pending dial drained", err)
				}
				if err := st.CheckCleanupFence(ctx, i.ID); err == nil {
					t.Fatal("missing in-flight cleanup fence")
				}
				select {
				case err := <-done:
					t.Fatal("delete returned before pending handler drained", err)
				default:
				}
				if scenario == "cancelled-drain" {
					cancel()
				} else {
					release()
				}
			}
			err = <-done
			failed := scenario == "backup-preflight" || scenario == "cancelled-drain"
			if (err != nil) != failed {
				t.Fatal("unexpected delete result", err)
			}
			release()
			if failed {
				if _, err := st.CleanupReceipt(ctx, id, actor.UserLogin); !errors.Is(err, sql.ErrNoRows) {
					t.Fatal("failed cleanup published receipt", err)
				}
				if _, err := os.Stat(i.RootFSPath); err != nil {
					t.Fatal("failure removed disk", err)
				}
				fence := st.CheckCleanupFence(ctx, i.ID)
				if (fence != nil) != (scenario == "cancelled-drain") {
					t.Fatal("wrong cleanup fence", fence)
				}
			} else {
				proof, err := st.CleanupReceipt(ctx, id, actor.UserLogin)
				if err != nil || proof.Version != 3 || !proof.GatewaysAbsent {
					t.Fatal("missing gateway proof", proof, err)
				}
				if _, err := os.Stat(i.RootFSPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("disk retained", err)
				}
				if _, err := a.cmdDeleteProof(ctx, actor, args); err != nil {
					t.Fatal("receipt replay", err)
				}
			}
			a.syncManagedGatewaysBestEffort()
			if scenario == "backup-preflight" {
				assertGatewayTraffic(t, target, targetPeer)
			} else {
				assertGatewayClosed(t, target)
				assertGatewayClosed(t, targetPeer)
				if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
					conn.Close()
					t.Fatal("deleted/fenced gateway reopened")
				}
				if late {
					assertGatewayClosed(t, lateGuest)
					assertGatewayClosed(t, latePeer)
				}
			}
			assertGatewayTraffic(t, neighbor, neighborPeer)
		})
	}
}

func connectGateway(t *testing.T, address, authority string, acknowledge bool) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority); err != nil {
		t.Fatal(err)
	}
	if acknowledge {
		response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("CONNECT: %v %v", response, err)
		}
	}
	return conn
}

func assertGatewayTraffic(t *testing.T, guest, peer net.Conn) {
	t.Helper()
	guest.SetDeadline(time.Now().Add(time.Second))
	peer.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(guest, "outbound"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	if _, err := io.ReadFull(peer, buf); err != nil || string(buf) != "outbound" {
		t.Fatalf("outbound: %q %v", buf, err)
	}
	if _, err := io.WriteString(peer, "inbound!"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(guest, buf); err != nil || string(buf) != "inbound!" {
		t.Fatalf("inbound: %q %v", buf, err)
	}
	guest.SetDeadline(time.Time{})
	peer.SetDeadline(time.Time{})
}

func assertGatewayClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, err := conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("gateway still forwards data")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("gateway socket remained open")
	}
}
