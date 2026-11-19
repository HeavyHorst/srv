package provision

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"srv/internal/model"
	"srv/internal/store"
	"srv/internal/wazuh"

	"golang.org/x/crypto/ssh"
	"tailscale.com/client/tailscale/v2"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
)

// monitorManager models the manager's agent table: names are unique, as
// authd enforces, and IDs are never reused.
type monitorManager struct {
	mu        sync.Mutex
	agents    map[string]wazuh.Agent
	next      int
	down      bool
	loseReply bool // create the agent, then fail the response
	registers int
	deletes   []string
}

var errManagerDown = errors.New("dial tcp: connection refused")

func (m *monitorManager) Register(_ context.Context, name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return "", errManagerDown
	}
	m.registers++
	for _, a := range m.agents {
		if a.Name == name {
			return "", wazuh.ErrNameExists
		}
	}
	id := m.addLocked(name, "never_connected")
	if m.loseReply {
		m.loseReply = false
		return "", errors.New("read: connection reset by peer")
	}
	return id, nil
}

func (m *monitorManager) addLocked(name, status string) string {
	m.next++
	id := fmt.Sprintf("%03d", m.next)
	m.agents[id] = wazuh.Agent{ID: id, Name: name, Status: status, Added: time.Now()}
	return id
}

func (m *monitorManager) Key(_ context.Context, id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	switch {
	case m.down:
		return "", errManagerDown
	case !ok:
		return "", wazuh.ErrNotFound
	}
	return base64.StdEncoding.EncodeToString([]byte(id + " " + a.Name + " any " + strings.Repeat("ab", 32))), nil
}

func (m *monitorManager) Agent(_ context.Context, id string) (wazuh.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	switch {
	case m.down:
		return wazuh.Agent{}, errManagerDown
	case !ok:
		return wazuh.Agent{}, wazuh.ErrNotFound
	}
	return a, nil
}

func (m *monitorManager) AgentByName(_ context.Context, name string) (wazuh.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return wazuh.Agent{}, errManagerDown
	}
	for _, a := range m.agents {
		if a.Name == name {
			return a, nil
		}
	}
	return wazuh.Agent{}, wazuh.ErrNotFound
}

func (m *monitorManager) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return errManagerDown
	}
	m.deletes = append(m.deletes, id)
	delete(m.agents, id)
	return nil
}

// connect is the agent connecting with the key it was given.
func (m *monitorManager) connect(identity []byte) error {
	var file struct {
		AgentID   string `json:"agent_id"`
		AgentName string `json:"agent_name"`
		AgentKey  string `json:"agent_key"`
	}
	if err := json.Unmarshal(identity, &file); err != nil {
		return err
	}
	line, err := base64.StdEncoding.DecodeString(file.AgentKey)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[file.AgentID]
	if !ok || a.Name != file.AgentName || !strings.HasPrefix(string(line), a.ID+" "+a.Name+" ") {
		return errors.New("manager rejects the agent key")
	}
	a.Status = "active"
	m.agents[a.ID] = a
	return nil
}

func (m *monitorManager) snapshot() (map[string]wazuh.Agent, int, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	agents := map[string]wazuh.Agent{}
	for id, a := range m.agents {
		agents[id] = a
	}
	return agents, m.registers, slices.Clone(m.deletes)
}

// monitorGuest is the guest end of the SSH channel: srv-wazuh-enroll.service
// installs the delivered identity and the agent connects.
type monitorGuest struct {
	h        *monitorHarness
	identity []byte
}

func (g *monitorGuest) WriteEnrollment(_ context.Context, identity []byte) error {
	g.identity = identity
	return nil
}

func (g *monitorGuest) StartEnrollment(context.Context) error {
	g.h.mu.Lock()
	g.h.delivered = append(g.h.delivered, string(g.identity))
	g.h.mu.Unlock()
	return g.h.manager.connect(g.identity)
}

type monitorHarness struct {
	t       *testing.T
	p       *Provisioner
	st      *store.Store
	manager *monitorManager
	hostKey string // the fingerprint the guest's tailnet node advertises
	cancel  context.CancelFunc

	mu        sync.Mutex
	opened    []string
	delivered []string
}

func newMonitorHarness(t *testing.T) *monitorHarness {
	t.Helper()
	oldList := listTailnetDevices
	t.Cleanup(func() { listTailnetDevices = oldList })
	listTailnetDevices = func(context.Context, *tailscale.Client) ([]tailscale.Device, error) {
		return []tailscale.Device{{NodeID: "node-demo", Hostname: "demo", Addresses: []string{"100.64.0.10"}}}, nil
	}
	cfg := loadProvisionTestConfig(t, map[string]string{"SRV_GUEST_READY_TIMEOUT": "2s"})
	// Guests boot an image whose manifest records the agent.
	if err := os.MkdirAll(cfg.ImagesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ImagesDir(), "manifest.txt"), []byte("arch=x86_64\nwazuh_agent_version=4.14.9-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &monitorHarness{t: t, st: newProvisionTestStore(t, cfg), manager: &monitorManager{agents: map[string]wazuh.Agent{}}}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	h.hostKey = ssh.FingerprintSHA256(signer.PublicKey())
	// Tailscale SSH advertises one key of each type; only ed25519 is pinned.
	rsaLine := "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQDDHr/jh2Jy4yALcK4JyWbVkPRaWmhck3IgCoeOO3z1e2dBowLh64QAM+Qb72pxekALga2oi4GvT+TlWNhzPH4V4Akwt6VGTf/aWgoY1Hdo9tK7Iiv86RA5aRXRLRlmF1HzEyO2Gb1R/rLwfDmS9+DU34/0lj2EJ+T+KvqhL0uv2Q=="
	edLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	client := &readinessTestClient{status: func() (*ipnstate.Status, error) {
		return &ipnstate.Status{Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			{}: {ID: "node-demo", HostName: "demo", TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.10")}, SSH_HostKeys: []string{rsaLine, edLine}},
		}}, nil
	}}
	runner := readinessStartRunner{panicVMRunner: panicVMRunner{t}, start: func() { t.Fatal("pending guest rebooted") }}
	h.p = &Provisioner{cfg: cfg, store: h.st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), vmRunner: runner, tsClient: &tailscale.Client{}, localClient: client}

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	t.Cleanup(func() { cancel(); h.p.monitoring.wg.Wait() })
	h.p.SetWazuhMonitoring(ctx, WazuhMonitoring{
		Coordinator: &wazuh.Coordinator{
			Manager:        h.manager,
			Store:          &wazuh.FileStore{Path: filepath.Join(t.TempDir(), "wazuh.json")},
			ManagerAddress: "wazuh.example.ts.net",
			ManagerPort:    1514,
			PollInterval:   time.Millisecond,
			ReportTimeout:  time.Second,
		},
		AssetPrefix: "gpu-rig-",
		OpenGuest: func(_ context.Context, addr, hostKey string) (wazuh.Guest, io.Closer, error) {
			h.mu.Lock()
			h.opened = append(h.opened, addr+" "+hostKey)
			h.mu.Unlock()
			return &monitorGuest{h: h}, io.NopCloser(nil), nil
		},
		RetryInterval: 5 * time.Millisecond,
	})
	return h
}

// boot runs one cold boot of an incarnation through provisioning readiness:
// the guest reports this attempt's enrollment on serial and the reconcile
// path marks it ready.
func (h *monitorHarness) boot(id string, created time.Time, attempt string) model.Instance {
	h.t.Helper()
	ctx := context.Background()
	inst := provisionTestInstance(h.p.cfg, "demo", model.StateAwaitingTailnet, created)
	inst.ID = id
	inst.SocketPath, inst.FirecrackerPID = readinessTestSocket(h.t), os.Getpid()
	inst.StartRequestID, inst.StartBootID = attempt, "test-boot"
	inst.TailscaleName, inst.TailscaleIP, inst.TailnetNodeID = "", "", ""
	inst.SerialLogPath = filepath.Join(h.t.TempDir(), "serial.log")
	report := fmt.Sprintf("srv-enrollment: {\"nonce\":%q,\"node_id\":\"node-demo\"}\n", attempt)
	if err := os.WriteFile(inst.SerialLogPath, []byte(report), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if _, found, _ := h.st.FindInstance(ctx, inst.Name); found {
		if err := h.st.UpdateInstance(ctx, inst); err != nil {
			h.t.Fatal(err)
		}
	} else if err := h.st.CreateInstance(ctx, inst); err != nil {
		h.t.Fatal(err)
	}
	got, err := h.p.ReconcileInstance(ctx, inst.Name)
	if err != nil || got.State != model.StateReady {
		h.t.Fatalf("readiness: %+v, %v", got, err)
	}
	return got
}

// settle waits for enrollment to finish; a loop still retrying fails the test.
func (h *monitorHarness) settle() {
	h.t.Helper()
	done := make(chan struct{})
	go func() { h.p.monitoring.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		h.cancel()
		<-done
		h.t.Fatal("enrollment still retrying after 10s")
	}
}

func (h *monitorHarness) events(id string) []string {
	h.t.Helper()
	events, err := h.st.ListEvents(context.Background(), id, 200)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		if e.Type == "wazuh" {
			out = append(out, e.Message)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestProvisioningEnrollsMonitoring(t *testing.T) {
	h := newMonitorHarness(t)
	created := time.Now().UTC().Add(-time.Hour)

	// First boot: one agent, named for the asset, reporting with the key
	// delivered over SSH to the pinned tailnet address and host key.
	first := h.boot("demo-id-1", created, "attempt-1")
	h.settle()
	agents, registers, deletes := h.manager.snapshot()
	if len(agents) != 1 || registers != 1 || len(deletes) != 0 {
		t.Fatalf("first enroll: agents %v, registers %d, deletes %v", agents, registers, deletes)
	}
	var agent wazuh.Agent
	for _, a := range agents {
		agent = a
	}
	if agent.Name != "gpu-rig-demo" || agent.Status != "active" {
		t.Fatalf("agent %+v", agent)
	}
	if want := "100.64.0.10:22 " + h.hostKey; len(h.opened) != 1 || h.opened[0] != want {
		t.Fatalf("guest connections %v, want %q", h.opened, want)
	}
	if ev := h.events(first.ID); len(ev) != 1 || ev[0] != "wazuh agent "+agent.ID+" is reporting" {
		t.Fatalf("events %v", ev)
	}

	// Reboot of the same incarnation: the agent is reused, its identity
	// delivered again, and nothing is registered or deleted.
	h.manager.mu.Lock()
	a := h.manager.agents[agent.ID]
	a.Status = "disconnected"
	h.manager.agents[agent.ID] = a
	h.manager.mu.Unlock()
	h.boot("demo-id-1", created, "attempt-2")
	h.settle()
	agents, registers, deletes = h.manager.snapshot()
	if len(agents) != 1 || agents[agent.ID].Status != "active" || registers != 1 || len(deletes) != 0 || len(h.delivered) != 2 || h.delivered[0] != h.delivered[1] {
		t.Fatalf("reboot: agents %v, registers %d, deletes %v, delivered %d", agents, registers, deletes, len(h.delivered))
	}

	// Clean rebuild: a new incarnation of the asset retires only the mapped
	// agent, then registers its own.
	h.manager.mu.Lock()
	other := h.manager.addLocked("gpu-rig-other", "disconnected")
	h.manager.mu.Unlock()
	if err := h.st.DeleteInstance(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	rebuilt := h.boot("demo-id-2", created.Add(time.Minute), "attempt-3")
	h.settle()
	agents, registers, deletes = h.manager.snapshot()
	if len(deletes) != 1 || deletes[0] != agent.ID || registers != 2 || len(agents) != 2 || agents[other].Status != "disconnected" {
		t.Fatalf("rebuild: agents %v, registers %d, deletes %v", agents, registers, deletes)
	}
	for id, a := range agents {
		if id != other && (a.Name != "gpu-rig-demo" || a.Status != "active" || id == agent.ID) {
			t.Fatalf("rebuilt agent %+v", a)
		}
	}
	if ev := h.events(rebuilt.ID); len(ev) != 1 || !strings.HasSuffix(ev[0], "is reporting") {
		t.Fatalf("rebuild events %v", ev)
	}
}

func TestProvisioningMonitoringManagerDown(t *testing.T) {
	h := newMonitorHarness(t)
	h.manager.down = true
	// Readiness completes while the manager is unreachable.
	inst := h.boot("demo-id-1", time.Now().UTC(), "attempt-1")
	waitFor(t, "a pending event", func() bool {
		return slices.Contains(h.events(inst.ID), "monitoring pending; enrollment will be retried")
	})
	if stored, err := h.st.GetInstance(context.Background(), "demo"); err != nil || stored.State != model.StateReady {
		t.Fatalf("guest not left ready: %+v, %v", stored, err)
	}
	h.manager.mu.Lock()
	h.manager.down = false
	h.manager.mu.Unlock()
	h.settle()
	agents, registers, _ := h.manager.snapshot()
	if len(agents) != 1 || registers != 1 {
		t.Fatalf("after recovery: agents %v, registers %d", agents, registers)
	}
	for _, a := range agents {
		if a.Status != "active" {
			t.Fatalf("agent %+v", a)
		}
	}
}

func TestProvisioningMonitoringLostRegisterResponse(t *testing.T) {
	h := newMonitorHarness(t)
	h.manager.loseReply = true
	inst := h.boot("demo-id-1", time.Now().UTC(), "attempt-1")
	h.settle()
	// The retry adopts the agent the lost response created, not a second one.
	agents, registers, deletes := h.manager.snapshot()
	if len(agents) != 1 || registers != 2 || len(deletes) != 0 {
		t.Fatalf("agents %v, registers %d, deletes %v", agents, registers, deletes)
	}
	// Events list newest first.
	if ev := h.events(inst.ID); len(ev) != 2 || !strings.HasSuffix(ev[0], "is reporting") || !strings.HasPrefix(ev[1], "monitoring pending") {
		t.Fatalf("events %v", ev)
	}
}

func TestProvisioningMonitoringDuplicatePrevention(t *testing.T) {
	h := newMonitorHarness(t)
	// Another srv host (or a hand-made registration) already owns the name.
	h.manager.mu.Lock()
	foreign := h.manager.addLocked("gpu-rig-demo", "active")
	h.manager.mu.Unlock()
	inst := h.boot("demo-id-1", time.Now().UTC(), "attempt-1")
	waitFor(t, "a pending event", func() bool {
		return slices.Contains(h.events(inst.ID), "monitoring pending; enrollment will be retried")
	})
	h.cancel()
	h.settle()
	agents, _, deletes := h.manager.snapshot()
	if len(agents) != 1 || agents[foreign].Status != "active" || len(deletes) != 0 || len(h.delivered) != 0 {
		t.Fatalf("foreign agent touched: agents %v, deletes %v, delivered %d", agents, deletes, len(h.delivered))
	}
}

func TestProvisioningMonitoringCoalescesRequests(t *testing.T) {
	h := newMonitorHarness(t)
	h.boot("demo-id-1", time.Now().UTC(), "attempt-1")
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); h.p.requestMonitoring("demo") }()
	}
	wg.Wait()
	if err := h.p.ResumeMonitoring(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.settle()
	agents, registers, deletes := h.manager.snapshot()
	if len(agents) != 1 || registers != 1 || len(deletes) != 0 {
		t.Fatalf("agents %v, registers %d, deletes %v", agents, registers, deletes)
	}
}

// Guests from images without the agent, such as every guest created before
// the agent shipped, are neither registered nor contacted.
func TestProvisioningMonitoringSkipsImagesWithoutAgent(t *testing.T) {
	for _, manifest := range []string{"arch=x86_64\nwazuh_agent=disabled\n", ""} {
		h := newMonitorHarness(t)
		path := filepath.Join(h.p.cfg.ImagesDir(), "manifest.txt")
		if manifest == "" {
			os.Remove(path)
		} else if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		inst := h.boot("demo-id-1", time.Now().UTC(), "attempt-1")
		if err := h.p.ResumeMonitoring(context.Background()); err != nil {
			t.Fatal(err)
		}
		h.settle()
		agents, registers, _ := h.manager.snapshot()
		if len(agents) != 0 || registers != 0 || len(h.opened) != 0 || len(h.events(inst.ID)) != 0 {
			t.Fatalf("manifest %q: agents %v, registers %d, guest connections %v, events %v", manifest, agents, registers, h.opened, h.events(inst.ID))
		}
	}
}
