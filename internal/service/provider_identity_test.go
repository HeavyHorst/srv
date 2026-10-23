package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
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
	"srv/internal/identity"
	"srv/internal/model"
)

func TestProviderDirectAndForwardedIsolation(t *testing.T) {
	ctx := context.Background()
	p := identity.Providers{"provider-node": "amp"}
	who := apitype.WhoIsResponse{Node: &tailcfg.Node{StableID: "provider-node", Tags: []string{"tag:provider"}}, UserProfile: &tailcfg.UserProfile{LoginName: "ops@example.com"}}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(who) }))
	defer api.Close()
	st := newServiceTestStore(t)
	for _, owner := range []string{"service:amp", "alice@example.com", "service:other"} {
		inst := serviceTestInstance(strings.ReplaceAll(owner, ":", "-"), model.StateStopped, time.Now())
		inst.CreatedByUser = owner
		if err := st.CreateInstance(ctx, inst); err != nil {
			t.Fatal(err)
		}
	}
	app := &App{cfg: config.Config{ProviderNodes: p, AllowedUsers: []string{"service:amp"}, AdminUsers: []string{"ops@example.com"}, TrustedFrontends: []string{"front-node"}}, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), localAPI: &local.Client{Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", api.Listener.Addr().String())
	}}}
	sess := newServiceTestSession(ctx, "", false, []string{"--json", "list"})
	actor, _, _, err := app.resolveCommandActor(ctx, sess, sess.Command())
	if err != nil || actor.UserLogin != "service:amp" {
		t.Fatalf("direct actor: %+v %v", actor, err)
	}
	app.handleSession(sess)
	if sess.exitCode != 0 || !strings.Contains(sess.stdout.String(), "service-amp") || strings.Contains(sess.stdout.String(), "alice") || strings.Contains(sess.stdout.String(), "service-other") {
		t.Fatalf("direct list: %s %s", &sess.stdout, &sess.stderr)
	}
	for _, command := range []string{"status", "snapshot", "integration", "pool"} {
		if ok, _ := app.authorize(actor, command); ok {
			t.Fatalf("provider authorized for %s", command)
		}
	}
	if app.isAdmin(actor) || app.canAccessInstance(actor, model.Instance{CreatedByUser: "alice@example.com"}) {
		t.Fatal("provider inherited human privileges")
	}
	who.Node.StableID = "unknown"
	if _, _, _, err := app.resolveCommandActor(ctx, sess, sess.Command()); err == nil {
		t.Fatal("unknown tagged node accepted")
	}
	who.Node.StableID = "provider-node"
	if _, _, _, err := app.resolveCommandActor(ctx, sess, []string{"forward", "payload", "list"}); err == nil {
		t.Fatal("provider allowed to forward arbitrary users")
	}
	// The same resolver used by srv-front authenticates the provider. Exercise
	// the actual SSH forwarding transport and backend authorization thereafter.
	frontActor, err := (front.Config{ProviderNodes: p}).ProviderNodes.Resolve(&who, "provider-peer", "root")
	if err != nil {
		t.Fatal(err)
	}
	who.Node.StableID = "front-node"
	key, err := ensureHostSigner(t.TempDir() + "/key")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &gssh.Server{HostSigners: []gssh.Signer{key}, Handler: app.handleSession}
	done := make(chan struct{})
	go func() { defer close(done); server.Serve(ln) }()
	defer func() { server.Close(); ln.Close(); <-done }()
	h := front.Host{Name: "backend", Address: ln.Addr().String(), HostKey: string(ssh.MarshalAuthorizedKey(key.PublicKey()))}
	transport := front.SSHTransport{Dial: (&net.Dialer{}).DialContext}
	for _, tc := range []struct {
		name  string
		actor model.Actor
		args  []string
		fail  bool
	}{
		{"owner list", frontActor, []string{"--json", "list"}, false},
		{"no admin", frontActor, []string{"status"}, true},
		{"no foreign instance", frontActor, []string{"inspect", "service-other"}, true},
		{"unconfigured forwarded owner", model.Actor{UserLogin: "service:unknown"}, []string{"list"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			err := transport.Run(ctx, h, tc.actor, tc.args, nil, &out, &stderr)
			if (err != nil) != tc.fail {
				t.Fatalf("err=%v stdout=%s stderr=%s", err, &out, &stderr)
			}
			if !tc.fail && (!strings.Contains(out.String(), "service-amp") || strings.Contains(out.String(), "alice") || strings.Contains(out.String(), "service-other")) {
				t.Fatalf("forwarded isolation: %s", &out)
			}
		})
	}
}
