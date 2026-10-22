package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"
	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"

	"srv/internal/config"
	"srv/internal/front"
	"srv/internal/host"
	"srv/internal/model"
	"srv/internal/provision"
)

func TestForwardedCommandsPreserveUserIsolation(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	for _, name := range []string{"alice", "bob"} {
		inst := serviceTestInstance(name, model.StateStopped, time.Now().UTC())
		inst.CreatedByUser = name + "@example.com"
		if err := st.CreateInstance(ctx, inst); err != nil {
			t.Fatal(err)
		}
	}
	who := apitype.WhoIsResponse{Node: &tailcfg.Node{StableID: "front-node", Tags: []string{"tag:srv-front"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(who)
	}))
	defer server.Close()
	app := &App{
		cfg:   config.Config{TrustedFrontends: []string{"front-node"}, AllowedUsers: []string{"alice@example.com"}},
		store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		localAPI: &local.Client{Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
		}},
	}
	for _, tc := range []struct {
		name, node, user string
		command          []string
		wantError        string
	}{
		{name: "trusted tagged frontend", node: "front-node", user: "alice@example.com", command: []string{"--json", "list"}},
		{name: "spoofed frontend", node: "other-node", user: "alice@example.com", command: []string{"--json", "list"}, wantError: "trusted frontend"},
		{name: "backend allowlist still applies", node: "front-node", user: "bob@example.com", command: []string{"--json", "list"}, wantError: "not authorized"},
		{name: "admin not inherited", node: "front-node", user: "alice@example.com", command: []string{"--json", "status"}, wantError: "SRV_ADMIN_USERS"},
		{name: "nested forwarding", node: "front-node", user: "alice@example.com", command: []string{"forward", "anything", "list"}, wantError: "nested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			who.Node.StableID = tailcfg.StableNodeID(tc.node)
			payload, _ := json.Marshal(model.Actor{UserLogin: tc.user, NodeName: "user-laptop"})
			args := append([]string{"forward", base64.RawURLEncoding.EncodeToString(payload)}, tc.command...)
			sess := newServiceTestSession(ctx, "", false, args)
			app.handleSession(sess)
			if tc.wantError != "" {
				if sess.exitCode == 0 || !strings.Contains(sess.stderr.String(), tc.wantError) || sess.stdout.Len() != 0 {
					t.Fatalf("exit=%d stdout=%s stderr=%s", sess.exitCode, sess.stdout.String(), sess.stderr.String())
				}
				return
			}
			var result struct {
				Instances []instanceSummaryJSON `json:"instances"`
			}
			if err := json.Unmarshal(sess.stdout.Bytes(), &result); err != nil || sess.exitCode != 0 {
				t.Fatalf("exit=%d stdout=%s stderr=%s decode=%v", sess.exitCode, sess.stdout.String(), sess.stderr.String(), err)
			}
			if len(result.Instances) != 1 || result.Instances[0].Name != "alice" {
				t.Fatalf("forwarded user saw other user's instances: %+v", result.Instances)
			}
		})
	}
}

// Exercise the real frontend protocol against two SSH backends, SQLite stores,
// and portable rootfs streams. Only the tailnet identity lookup is substituted;
// no hypervisor is needed to migrate an already stopped VM.
func TestFrontendColdMigrationOverSSH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	who := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(apitype.WhoIsResponse{Node: &tailcfg.Node{StableID: "front-node", Tags: []string{"tag:srv-front"}}})
	}))
	defer who.Close()
	localAPI := &local.Client{Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", who.Listener.Addr().String())
	}}
	// Both guests stay stopped. Exercise real delete orchestration against
	// disposable helper endpoints, never the orb's host services.
	helperDir, err := os.MkdirTemp("", "srv-migration-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(helperDir) })
	helperSocket := filepath.Join(helperDir, "helper.sock")
	helperListener, err := net.Listen("unix", helperSocket)
	if err != nil {
		t.Fatal(err)
	}
	helper := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vm/stop", "/network/cleanup":
			_, _ = io.WriteString(w, "{}")
		default:
			t.Errorf("unexpected helper operation: %s", r.URL.Path)
			http.Error(w, "unexpected operation", 400)
		}
	})}
	helperDone := make(chan struct{})
	go func() { defer close(helperDone); _ = helper.Serve(helperListener) }()
	t.Cleanup(func() { _ = helper.Close(); <-helperDone })
	var hosts []front.Host
	var apps []*App
	for _, name := range []string{"a", "b"} {
		cfg := loadServiceTestConfig(t, nil)
		cfg.VMRunnerSocketPath, cfg.NetHelperSocketPath, cfg.OutboundInterface = helperSocket, helperSocket, "fixture0"
		cfg.Hostname = name
		cfg.TrustedFrontends = []string{"front-node"}
		cfg.AdminUsers = []string{"ops@example.com"}
		cfg.BaseKernelPath = filepath.Join(cfg.DataDirAbs(), "kernel")
		cfg.BaseRootFSPath = filepath.Join(cfg.DataDirAbs(), "base.img")
		cfg.BaseInitrdPath = ""
		if err := os.MkdirAll(cfg.InstancesDir(), 0700); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{cfg.BaseKernelPath, cfg.BaseRootFSPath} {
			if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		st := newServiceTestStore(t)
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p, err := provision.New(cfg, logger, st)
		if err != nil {
			t.Fatal(err)
		}
		app := &App{cfg: cfg, store: st, provisioner: p, log: logger, localAPI: localAPI}
		apps = append(apps, app)
		signer, err := ensureHostSigner(cfg.HostKeyPath())
		if err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := &gssh.Server{HostSigners: []gssh.Signer{signer}, Handler: app.handleSession}
		done := make(chan error, 1)
		go func() { done <- server.Serve(listener) }()
		t.Cleanup(func() { _ = server.Close(); _ = listener.Close(); <-done })
		hosts = append(hosts, front.Host{Name: name, Address: listener.Addr().String(), HostKey: string(ssh.MarshalAuthorizedKey(signer.PublicKey()))})
	}
	source := serviceTestInstance("demo", model.StateStopped, time.Now().UTC())
	source.CreatedByUser = "alice@example.com"
	source.RootFSPath = filepath.Join(apps[0].cfg.InstancesDir(), source.Name, "rootfs.img")
	source.SerialLogPath = filepath.Join(filepath.Dir(source.RootFSPath), "serial.log")
	source.LogPath = filepath.Join(filepath.Dir(source.RootFSPath), "firecracker.log")
	payload := bytes.Repeat([]byte("portable-rootfs\x00\xff"), 4096)
	source.RootFSSizeBytes = int64(len(payload))
	if err := os.MkdirAll(filepath.Dir(source.RootFSPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source.RootFSPath, payload, 0600); err != nil {
		t.Fatal(err)
	}
	if err := apps[0].store.CreateInstance(ctx, source); err != nil {
		t.Fatal(err)
	}
	router := front.Router{Hosts: hosts, Transport: front.SSHTransport{Dial: (&net.Dialer{}).DialContext}}
	actor := model.Actor{UserLogin: source.CreatedByUser}
	var out, stderr bytes.Buffer
	if err := router.Run(ctx, actor, []string{"node", "list"}, nil, &out, &stderr); err == nil || !strings.Contains(stderr.String(), "SRV_ADMIN_USERS") {
		t.Fatalf("node stats bypassed backend authorization: %v %s", err, &stderr)
	}
	out.Reset()
	stderr.Reset()
	if err := router.Run(ctx, model.Actor{UserLogin: "ops@example.com"}, []string{"--json", "node", "list"}, nil, &out, &stderr); err != nil {
		t.Fatalf("node stats: %v %s", err, &stderr)
	}
	var nodes struct {
		Hosts []struct {
			Host string               `json:"host"`
			Data host.CapacitySummary `json:"data"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(out.Bytes(), &nodes); err != nil || len(nodes.Hosts) != 2 || nodes.Hosts[0].Host != "a" || nodes.Hosts[0].Data.Instances.Total != 1 || nodes.Hosts[1].Data.Instances.Total != 0 || len(nodes.Hosts[0].Data.Capacity) != 3 {
		t.Fatalf("wrong per-host stats: %s %v", &out, err)
	}
	out.Reset()
	stderr.Reset()
	if err := router.Run(ctx, actor, []string{"migrate", "a/demo", "--to", "b"}, nil, &out, &stderr); err != nil {
		t.Fatalf("migration: %v\nstdout=%s\nstderr=%s", err, &out, &stderr)
	}
	dest, err := apps[1].store.GetInstance(ctx, "demo")
	if err != nil || dest.State != model.StateStopped || dest.CreatedByUser != source.CreatedByUser {
		t.Fatalf("destination: %+v %v", dest, err)
	}
	got, err := os.ReadFile(dest.RootFSPath)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("destination payload mismatch: %v", err)
	}
	migration, err := apps[0].store.GetMigration(ctx, source.ID)
	if err != nil || migration.Phase != "completed" || migration.Target != "b" {
		t.Fatalf("source migration: %+v %v", migration, err)
	}
	out.Reset()
	stderr.Reset()
	if err := router.Run(ctx, actor, []string{"start", "a/demo"}, nil, &out, &stderr); err == nil || !strings.Contains(stderr.String(), "fenced") {
		t.Fatalf("source restarted: %v %s", err, &stderr)
	}
	out.Reset()
	stderr.Reset()
	if err := router.Run(ctx, actor, []string{"--json", "inspect", "demo"}, nil, &out, &stderr); err != nil {
		t.Fatalf("short name must resolve to destination: %v %s", err, &stderr)
	}
	var inspected inspectResponseJSON
	if err := json.Unmarshal(out.Bytes(), &inspected); err != nil || inspected.Instance.RootFSPath != dest.RootFSPath {
		t.Fatalf("wrong short-name destination: %s %v", &out, err)
	}
	out.Reset()
	stderr.Reset()
	if err := router.Run(ctx, actor, []string{"delete", "a/demo"}, nil, &out, &stderr); err != nil {
		t.Fatalf("delete completed source: %v %s", err, &stderr)
	}
	deleted, err := apps[0].store.GetInstance(ctx, source.Name)
	if err != nil || deleted.State != model.StateDeleted {
		t.Fatalf("expected deleted source tombstone: %+v %v", deleted, err)
	}
	// A return import must not depend on a local rootfs template.
	if err := os.Remove(apps[0].cfg.BaseRootFSPath); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if err := router.Run(ctx, actor, []string{"migrate", "b/demo", "--to", "a"}, nil, &out, &stderr); err != nil {
		t.Fatalf("return migration: %v %s", err, &stderr)
	}
	returned, err := apps[0].store.GetInstance(ctx, source.Name)
	if err != nil || returned.State != model.StateStopped || returned.CreatedByUser != source.CreatedByUser {
		t.Fatalf("returned instance: %+v %v", returned, err)
	}
	got, err = os.ReadFile(returned.RootFSPath)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("return payload mismatch: %v", err)
	}
	if _, err := apps[0].store.GetMigration(ctx, returned.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("obsolete source fence survived return: %v", err)
	}
	reverse, err := apps[1].store.GetMigration(ctx, dest.ID)
	if err != nil || reverse.Phase != "completed" || reverse.Token == migration.Token {
		t.Fatalf("return source journal: %+v %v", reverse, err)
	}
	receipt, err := apps[0].store.GetImportReceipt(ctx, returned.ID)
	if err != nil || receipt.Phase != "imported" || receipt.Token != reverse.Token || receipt.SourceInstanceID != dest.ID {
		t.Fatalf("return receipt: %+v %v", receipt, err)
	}
	out.Reset()
	stderr.Reset()
	if err := router.Run(ctx, actor, []string{"--json", "inspect", "demo"}, nil, &out, &stderr); err != nil {
		t.Fatalf("return short-name lookup: %v %s", err, &stderr)
	}
	if err := json.Unmarshal(out.Bytes(), &inspected); err != nil || inspected.Instance.RootFSPath != returned.RootFSPath || inspected.Instance.MigrationPhase != "" {
		t.Fatalf("wrong return short-name destination: %s %v", &out, err)
	}
}
