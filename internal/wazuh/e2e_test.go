package wazuh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gliderssh "github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// fakeAPI implements the Wazuh manager API endpoints the controller uses,
// with the response shapes and error codes of Wazuh 4.x.
type fakeAPI struct {
	mu     sync.Mutex
	agents map[string]*apiAgent
	next   int
	posts  int
	// connections records agents that connected with a valid key.
	connections int
	// dropPost creates the next agent but drops the response, like a
	// connection lost after authd added it. postDelay holds POSTs open so
	// concurrent controllers overlap.
	dropPost  bool
	postDelay time.Duration
}

type apiAgent struct {
	id, name, key, status string
	added                 time.Time
}

func newFakeAPI(t *testing.T) (*fakeAPI, *httptest.Server) {
	api := &fakeAPI{agents: map[string]*apiAgent{}, next: 1}
	srv := httptest.NewTLSServer(http.HandlerFunc(api.serve))
	t.Cleanup(srv.Close)
	return api, srv
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func items(affected []any, missing []string) map[string]any {
	failed := []any{}
	for range missing {
		failed = append(failed, map[string]any{"error": map[string]any{"code": apiErrAgentNotFound}})
	}
	errCode := 0
	if len(missing) > 0 {
		errCode = 1
	}
	return map[string]any{"data": map[string]any{"affected_items": affected, "failed_items": failed, "total_affected_items": len(affected)}, "error": errCode}
}

func (a *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/security/user/authenticate" {
		if u, p, ok := r.BasicAuth(); !ok || u != "srv-enroll" || p != "test-password" {
			reply(w, 401, map[string]any{"error": 6000, "detail": "Invalid credentials"})
			return
		}
		reply(w, 200, map[string]any{"data": map[string]any{"token": "tok"}, "error": 0})
		return
	}
	if r.Header.Get("Authorization") != "Bearer tok" {
		reply(w, 401, map[string]any{"error": 6000, "detail": "Unauthorized"})
		return
	}
	if r.Method == http.MethodPost && a.postDelay > 0 {
		time.Sleep(a.postDelay)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/agents":
		a.posts++
		var body struct{ Name, IP string }
		json.NewDecoder(r.Body).Decode(&body)
		for _, ag := range a.agents {
			if ag.name == body.Name {
				reply(w, 400, map[string]any{"title": "Bad Request", "detail": "Name is already used", "error": apiErrNameExists})
				return
			}
		}
		raw := make([]byte, 32)
		rand.Read(raw)
		id := fmt.Sprintf("%03d", a.next)
		a.next++
		key := base64.StdEncoding.EncodeToString([]byte(id + " " + body.Name + " any " + hex.EncodeToString(raw)))
		a.agents[id] = &apiAgent{id: id, name: body.Name, key: key, status: "never_connected", added: time.Now().UTC().Truncate(time.Second)}
		if a.dropPost {
			a.dropPost = false
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		reply(w, 200, map[string]any{"data": map[string]any{"id": id, "key": key}, "error": 0})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/key"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/agents/"), "/key")
		if ag, ok := a.agents[id]; ok {
			reply(w, 200, items([]any{map[string]any{"id": id, "key": ag.key}}, nil))
		} else {
			reply(w, 200, items([]any{}, []string{id}))
		}
	case r.Method == http.MethodGet && r.URL.Path == "/agents":
		var out []any
		var missing []string
		for _, ag := range a.agents {
			if (q.Has("agents_list") && ag.id == q.Get("agents_list")) || (q.Has("name") && ag.name == q.Get("name")) {
				out = append(out, map[string]any{"id": ag.id, "name": ag.name, "status": ag.status, "dateAdd": ag.added.Format("2006-01-02T15:04:05+00:00")})
			}
		}
		if out == nil && q.Has("agents_list") {
			missing = []string{q.Get("agents_list")}
		}
		reply(w, 200, items(out, missing))
	case r.Method == http.MethodDelete && r.URL.Path == "/agents":
		id := q.Get("agents_list")
		if q.Get("status") != "all" || q.Get("older_than") != "0s" {
			reply(w, 400, map[string]any{"error": 1, "detail": "unexpected filter"})
			return
		}
		if _, ok := a.agents[id]; !ok {
			reply(w, 200, items([]any{}, []string{id}))
			return
		}
		delete(a.agents, id)
		reply(w, 200, items([]any{id}, nil))
	default:
		reply(w, 404, map[string]any{"error": 1, "detail": "not found"})
	}
}

// connect is the agent connecting with the client.keys line it was given.
func (a *fakeAPI) connect(line string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ag := range a.agents {
		if decoded, _ := base64.StdEncoding.DecodeString(ag.key); string(decoded) == line {
			ag.status = "active"
			a.connections++
			return true
		}
	}
	return false
}

func (a *fakeAPI) byName(name string) []*apiAgent {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*apiAgent
	for _, ag := range a.agents {
		if ag.name == name {
			out = append(out, ag)
		}
	}
	return out
}

func (a *fakeAPI) disconnectAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ag := range a.agents {
		ag.status = "disconnected"
	}
}

// sshGuest is a guest reached over real SSH. It interprets the two commands
// the controller sends and runs the image's real wazuh-enroll.sh against a
// scratch root, with systemctl and chown recorded instead of run.
type sshGuest struct {
	t           *testing.T
	root, bin   string
	addr        string
	fingerprint string
	api         *fakeAPI
	mu          sync.Mutex
	run         sync.Mutex
	commands    []string
}

func (g *sshGuest) sent() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.commands...)
}

func newSSHGuest(t *testing.T, api *fakeAPI, hostname string) *sshGuest {
	t.Helper()
	g := &sshGuest{t: t, root: t.TempDir(), bin: t.TempDir(), api: api}
	for _, name := range []string{"bash", "env", "jq", "base64", "flock", "stat", "id", "mktemp", "cat", "chmod", "mv", "sync", "sed", "grep", "wc", "rm", "sha256sum"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s unavailable: %v", name, err)
		}
		if err := os.Symlink(path, filepath.Join(g.bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"chown", "systemctl"} {
		stub := "#!/bin/sh\necho " + name + " \"$@\" >>" + filepath.Join(g.root, "calls") + "\n"
		if err := os.WriteFile(filepath.Join(g.bin, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	conf, err := os.ReadFile("../../images/arch-base/overlay/var/ossec/etc/ossec.conf")
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{"var/ossec/etc/ossec.conf": string(conf), "proc/sys/kernel/hostname": hostname + "\n"} {
		os.MkdirAll(filepath.Join(g.root, filepath.Dir(path)), 0o755)
		os.WriteFile(filepath.Join(g.root, path), []byte(content), 0o660)
	}
	os.MkdirAll(filepath.Join(g.root, "var/lib/srv"), 0o700)

	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := gossh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	g.fingerprint = gossh.FingerprintSHA256(signer.PublicKey())
	srv := &gliderssh.Server{Handler: g.handle}
	srv.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g.addr = l.Addr().String()
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return g
}

func (g *sshGuest) handle(s gliderssh.Session) {
	g.mu.Lock()
	g.commands = append(g.commands, s.User()+": "+s.RawCommand())
	g.mu.Unlock()
	// One enrollment at a time, as systemd runs the oneshot unit.
	g.run.Lock()
	defer g.run.Unlock()
	if s.User() != "root" {
		s.Exit(1)
		return
	}
	input := filepath.Join(g.root, "run/srv-wazuh/enrollment.json")
	switch s.RawCommand() {
	case writeEnrollmentCommand:
		os.MkdirAll(filepath.Dir(input), 0o700)
		body, _ := io.ReadAll(s)
		tmp := filepath.Join(filepath.Dir(input), ".enrollment.json.tmp")
		if os.WriteFile(tmp, body, 0o600) != nil || os.Rename(tmp, input) != nil {
			s.Exit(1)
			return
		}
		s.Exit(0)
	case startEnrollmentCommand:
		script, _ := filepath.Abs("../../images/arch-base/overlay/usr/local/lib/srv/wazuh-enroll.sh")
		cmd := exec.Command(filepath.Join(g.bin, "bash"), script, "enroll", input)
		cmd.Env = []string{"PATH=" + g.bin, "SRV_WAZUH_ENROLL_ROOT=" + g.root}
		out, err := cmd.CombinedOutput()
		s.Write(out)
		if err != nil {
			s.Exit(1)
			return
		}
		// The started agent connects with whatever client.keys now holds.
		keys, _ := os.ReadFile(filepath.Join(g.root, "var/ossec/etc/client.keys"))
		g.api.connect(strings.TrimSuffix(string(keys), "\n"))
		s.Exit(0)
	default:
		fmt.Fprintf(s, "unexpected command %q\n", s.RawCommand())
		s.Exit(127)
	}
}

func (g *sshGuest) keys() string {
	b, _ := os.ReadFile(filepath.Join(g.root, "var/ossec/etc/client.keys"))
	return string(b)
}

// enroll runs one enrollment the way cmd/srv-wazuh-enroll does.
func (g *sshGuest) enroll(c *Coordinator, asset string, inc Incarnation, name string) (string, error) {
	ctx := context.Background()
	client, err := DialGuest(ctx, nil, g.addr, g.fingerprint, nil)
	if err != nil {
		return "", err
	}
	defer client.Close()
	return c.Enroll(ctx, asset, inc, name, CommandGuest{Run: SSHRunner(client)})
}

func e2eCoordinator(t *testing.T, apiURL string, client *http.Client, state string) *Coordinator {
	t.Helper()
	password := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(password, []byte("test-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"SRV_WAZUH_API_URL": apiURL, "SRV_WAZUH_API_USER": "srv-enroll", "SRV_WAZUH_API_PASSWORD_FILE": password,
		"SRV_WAZUH_MANAGER_ADDRESS": "wazuh.example.ts.net", "SRV_WAZUH_STATE": state,
	}
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	c, err := cfg.Coordinator()
	if err != nil {
		t.Fatal(err)
	}
	// Trust the test server's certificate as a CA file would.
	c.Manager.(*HTTPManager).Client = client
	c.ReportTimeout, c.PollInterval = 5*time.Second, 10*time.Millisecond
	return c
}

func TestEndToEndEnrollment(t *testing.T) {
	api, srv := newFakeAPI(t)
	state := filepath.Join(t.TempDir(), "wazuh-state.json")
	c := e2eCoordinator(t, srv.URL, srv.Client(), state)
	incA := Incarnation{ID: "inst-a", Seq: 1}

	// First enroll: registered, delivered over SSH, installed by the guest
	// script, and reporting with the key the manager issued.
	guest := newSSHGuest(t, api, "vm-a")
	id, err := guest.enroll(c, "nrc:asset-1", incA, "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	agents := api.byName("vm-a")
	if len(agents) != 1 || agents[0].id != id || agents[0].status != "active" {
		t.Fatalf("after first enroll: %+v", agents)
	}
	decoded, _ := base64.StdEncoding.DecodeString(agents[0].key)
	if guest.keys() != string(decoded)+"\n" {
		t.Fatalf("guest client.keys %q, manager key %q", guest.keys(), decoded)
	}
	if _, err := os.Stat(filepath.Join(guest.root, "run/srv-wazuh/enrollment.json")); !os.IsNotExist(err) {
		t.Fatal("identity file left on the guest")
	}
	calls, _ := os.ReadFile(filepath.Join(guest.root, "calls"))
	if !strings.Contains(string(calls), "systemctl restart wazuh-agent.service") {
		t.Fatalf("guest calls: %s", calls)
	}
	if guest.sent()[0] != "root: "+writeEnrollmentCommand || guest.sent()[1] != "root: "+startEnrollmentCommand {
		t.Fatalf("ssh commands: %q", guest.sent())
	}

	// Reboot: the agent dropped off with the VM; the controller (a fresh
	// process reading the same state) reuses the registration.
	api.disconnectAll()
	c = e2eCoordinator(t, srv.URL, srv.Client(), state)
	again, err := guest.enroll(c, "nrc:asset-1", incA, "vm-a")
	if err != nil || again != id || api.posts != 1 || len(api.byName("vm-a")) != 1 || api.byName("vm-a")[0].status != "active" {
		t.Fatalf("reboot: %v, %s -> %s, posts %d", err, id, again, api.posts)
	}

	// Clean rebuild: a new incarnation with the same hostname on a fresh
	// disk replaces only this asset's registration.
	other := newSSHGuest(t, api, "vm-z")
	otherID, err := other.enroll(c, "nrc:asset-2", incA, "vm-z")
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := newSSHGuest(t, api, "vm-a")
	newID, err := rebuilt.enroll(c, "nrc:asset-1", Incarnation{ID: "inst-b", Seq: 2}, "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if agents := api.byName("vm-a"); newID == id || len(agents) != 1 || agents[0].id != newID || agents[0].status != "active" {
		t.Fatalf("rebuild: %s -> %s, agents %+v", id, newID, agents)
	}
	if agents := api.byName("vm-z"); len(agents) != 1 || agents[0].id != otherID {
		t.Fatalf("rebuild touched another asset: %+v", agents)
	}
	// The old incarnation cannot come back and claim the asset.
	if _, err := guest.enroll(c, "nrc:asset-1", incA, "vm-a"); !errors.Is(err, ErrStaleIncarnation) {
		t.Fatalf("old incarnation after rebuild: %v", err)
	}
}

func TestEndToEndManagerUnreachable(t *testing.T) {
	api, srv := newFakeAPI(t)
	state := filepath.Join(t.TempDir(), "wazuh-state.json")
	guest := newSSHGuest(t, api, "vm-a")
	down := httptest.NewTLSServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	c := e2eCoordinator(t, downURL, srv.Client(), state)
	if _, err := guest.enroll(c, "asset", Incarnation{ID: "inst-a", Seq: 1}, "vm-a"); err == nil {
		t.Fatal("enroll succeeded without a manager")
	}
	if len(guest.sent()) != 0 || guest.keys() != "" {
		t.Fatalf("guest touched while the manager was down: %q", guest.sent())
	}
	// Once the manager is back, the same enrollment completes once.
	c = e2eCoordinator(t, srv.URL, srv.Client(), state)
	if _, err := guest.enroll(c, "asset", Incarnation{ID: "inst-a", Seq: 1}, "vm-a"); err != nil {
		t.Fatal(err)
	}
	if api.posts != 1 || len(api.byName("vm-a")) != 1 || api.connections != 1 {
		t.Fatalf("recovery: posts %d, agents %+v", api.posts, api.byName("vm-a"))
	}
}

func TestEndToEndDuplicatePrevention(t *testing.T) {
	api, srv := newFakeAPI(t)
	state := filepath.Join(t.TempDir(), "wazuh-state.json")
	guest := newSSHGuest(t, api, "vm-a")
	inc := Incarnation{ID: "inst-a", Seq: 1}

	// Several controller processes enroll the same guest at once. They share
	// only the state file and the manager; slow registrations make them overlap.
	api.postDelay = 50 * time.Millisecond
	var wg sync.WaitGroup
	ids := make([]string, 6)
	errs := make([]error, len(ids))
	for i := range ids {
		c := e2eCoordinator(t, srv.URL, srv.Client(), state)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = guest.enroll(c, "asset", inc, "vm-a")
		}()
	}
	wg.Wait()
	var winner string
	for i, err := range errs {
		if err == nil {
			if winner != "" && ids[i] != winner {
				t.Fatalf("controllers enrolled different agents: %v", ids)
			}
			winner = ids[i]
		}
	}
	if agents := api.byName("vm-a"); winner == "" || len(agents) != 1 || agents[0].id != winner {
		t.Fatalf("concurrent enroll: ids %v errs %v agents %+v", ids, errs, agents)
	}
	for _, err := range errs {
		if err != nil && !errors.Is(err, ErrConflict) {
			t.Fatalf("concurrent loser failed unexpectedly: %v", err)
		}
	}
	api.postDelay = 0

	// A name already used by an agent this controller did not create is
	// refused, and that agent is left alone.
	other := newSSHGuest(t, api, "vm-b")
	api.mu.Lock()
	api.agents["900"] = &apiAgent{id: "900", name: "vm-b", key: "x", status: "active", added: time.Now().Add(-time.Hour)}
	api.mu.Unlock()
	c := e2eCoordinator(t, srv.URL, srv.Client(), state)
	if _, err := other.enroll(c, "asset-b", inc, "vm-b"); !errors.Is(err, ErrNameExists) {
		t.Fatalf("foreign name: %v", err)
	}
	if agents := api.byName("vm-b"); len(agents) != 1 || agents[0].id != "900" || len(other.sent()) != 0 {
		t.Fatalf("foreign agent: %+v, guest %q", agents, other.sent())
	}
}

func TestEndToEndRejectsWrongHostKey(t *testing.T) {
	api, _ := newFakeAPI(t)
	guest := newSSHGuest(t, api, "vm-a")
	if _, err := DialGuest(context.Background(), nil, guest.addr, "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", nil); err == nil {
		t.Fatal("unpinned host key accepted")
	}
}

func TestConfigFromEnv(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	os.WriteFile(good, []byte("secret\n"), 0o600)
	loose := filepath.Join(dir, "loose")
	os.WriteFile(loose, []byte("secret\n"), 0o644)
	os.Chmod(loose, 0o644)
	base := map[string]string{"SRV_WAZUH_API_URL": "https://m:55000/", "SRV_WAZUH_API_USER": "u", "SRV_WAZUH_MANAGER_ADDRESS": "m", "SRV_WAZUH_STATE": "/s"}
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"file":           {env: map[string]string{"SRV_WAZUH_API_PASSWORD_FILE": good}},
		"inline":         {env: map[string]string{"SRV_WAZUH_API_PASSWORD": "secret"}},
		"both":           {env: map[string]string{"SRV_WAZUH_API_PASSWORD_FILE": good, "SRV_WAZUH_API_PASSWORD": "secret"}, want: "only one"},
		"none":           {want: "missing SRV_WAZUH_API_PASSWORD(_FILE)"},
		"readable file":  {env: map[string]string{"SRV_WAZUH_API_PASSWORD_FILE": loose}, want: "group or others"},
		"plain http":     {env: map[string]string{"SRV_WAZUH_API_PASSWORD": "secret", "SRV_WAZUH_API_URL": "http://m:55000"}, want: "https"},
		"bad port":       {env: map[string]string{"SRV_WAZUH_API_PASSWORD": "secret", "SRV_WAZUH_MANAGER_PORT": "70000"}, want: "SRV_WAZUH_MANAGER_PORT"},
		"missing config": {env: map[string]string{"SRV_WAZUH_API_PASSWORD": "secret", "SRV_WAZUH_STATE": ""}, want: "SRV_WAZUH_STATE"},
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			for k, v := range tc.env {
				env[k] = v
			}
			cfg, err := ConfigFromEnv(func(k string) string { return env[k] })
			if tc.want == "" {
				if err != nil || cfg.APIPassword != "secret" || cfg.APIURL != "https://m:55000" || cfg.ManagerPort != 1514 {
					t.Fatalf("%+v %v", cfg, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestEndToEndLostRegistrationResponse(t *testing.T) {
	api, srv := newFakeAPI(t)
	state := filepath.Join(t.TempDir(), "wazuh-state.json")
	guest := newSSHGuest(t, api, "vm-a")
	inc := Incarnation{ID: "inst-a", Seq: 1}
	api.dropPost = true
	c := e2eCoordinator(t, srv.URL, srv.Client(), state)
	if _, err := guest.enroll(c, "asset", inc, "vm-a"); err == nil {
		t.Fatal("lost registration response reported success")
	}
	// The retry adopts the agent authd created, rather than failing on the
	// taken name or registering a second one.
	id, err := guest.enroll(c, "asset", inc, "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if agents := api.byName("vm-a"); api.posts != 2 || len(agents) != 1 || agents[0].id != id || agents[0].status != "active" {
		t.Fatalf("posts %d, agents %+v", api.posts, agents)
	}
}

func TestFileStoreCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.json")
	a, b := &FileStore{Path: path}, &FileStore{Path: path}
	if err := a.Put(ctx, Registration{Asset: "x", Generation: 1}, 0); err != nil {
		t.Fatal(err)
	}
	// b read generation 0 earlier; its write must not land.
	if err := b.Put(ctx, Registration{Asset: "x", Generation: 1, AgentID: "002"}, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale put: %v", err)
	}
	if err := b.Delete(ctx, "x", 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale delete: %v", err)
	}
	if err := b.Put(ctx, Registration{Asset: "x", Generation: 2, AgentID: "003"}, 1); err != nil {
		t.Fatal(err)
	}
	r, ok, err := a.Get(ctx, "x")
	if err != nil || !ok || r.AgentID != "003" || r.Generation != 2 {
		t.Fatalf("get: %+v %v %v", r, ok, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("state file mode: %v %v", info.Mode(), err)
	}
}
