package wazuh

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const hexKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// fakeManager models the manager's agent table. Names are unique, as authd
// enforces, and IDs are never reused.
type fakeManager struct {
	mu     sync.Mutex
	agents map[string]Agent
	next   int
	now    func() time.Time
	calls  []string
	// failRegisterAfterCreate creates the agent but reports an error, like a
	// response lost on the way back.
	failRegisterAfterCreate bool
	failDelete              error
	keyFor                  func(id, name string) string
}

func newFakeManager(now func() time.Time) *fakeManager {
	return &fakeManager{agents: map[string]Agent{}, next: 1, now: now}
}

func (m *fakeManager) record(format string, args ...any) {
	m.calls = append(m.calls, fmt.Sprintf(format, args...))
}

func (m *fakeManager) add(name, status string, added time.Time) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := fmt.Sprintf("%03d", m.next)
	m.next++
	m.agents[id] = Agent{ID: id, Name: name, Status: status, Added: added}
	return id
}

func (m *fakeManager) Register(_ context.Context, name string) (string, error) {
	m.mu.Lock()
	m.record("register %s", name)
	for _, a := range m.agents {
		if a.Name == name {
			m.mu.Unlock()
			return "", ErrNameExists
		}
	}
	m.mu.Unlock()
	id := m.add(name, "never_connected", m.now())
	if m.failRegisterAfterCreate {
		m.failRegisterAfterCreate = false
		return "", errors.New("connection reset")
	}
	return id, nil
}

func (m *fakeManager) Key(_ context.Context, id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	if !ok {
		return "", ErrNotFound
	}
	if m.keyFor != nil {
		return m.keyFor(id, a.Name), nil
	}
	return base64.StdEncoding.EncodeToString([]byte(id + " " + a.Name + " any " + hexKey)), nil
}

func (m *fakeManager) Agent(_ context.Context, id string) (Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	if !ok {
		return Agent{}, ErrNotFound
	}
	return a, nil
}

func (m *fakeManager) AgentByName(_ context.Context, name string) (Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.agents {
		if a.Name == name {
			return a, nil
		}
	}
	return Agent{}, ErrNotFound
}

func (m *fakeManager) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failDelete != nil {
		err := m.failDelete
		m.failDelete = nil
		return err
	}
	m.record("delete %s", id)
	delete(m.agents, id)
	return nil
}

func (m *fakeManager) deletes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, c := range m.calls {
		if strings.HasPrefix(c, "delete ") {
			out = append(out, c)
		}
	}
	return out
}

func (m *fakeManager) registers() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if strings.HasPrefix(c, "register ") {
			n++
		}
	}
	return n
}

// fakeGuest stands in for a guest whose agent connects with the delivered
// key as soon as srv-wazuh-enroll starts it.
type fakeGuest struct {
	mgr      *fakeManager
	mu       sync.Mutex
	steps    []string
	identity []byte
	failErr  error
}

func (g *fakeGuest) WriteEnrollment(_ context.Context, identity []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failErr != nil {
		return g.failErr
	}
	g.steps = append(g.steps, "write")
	g.identity = append([]byte(nil), identity...)
	return nil
}

func (g *fakeGuest) StartEnrollment(context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.steps = append(g.steps, "start")
	var identity enrollmentFile
	if err := json.Unmarshal(g.identity, &identity); err != nil {
		return err
	}
	g.mgr.connect(identity.AgentID, identity.AgentKey)
	return nil
}

// connect marks an agent active if it presents the manager's key.
func (m *fakeManager) connect(id, key string) {
	want, err := m.Key(context.Background(), id)
	if err != nil || want != key {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.agents[id]
	a.Status = "active"
	m.agents[id] = a
}

type fixture struct {
	clock time.Time
	mgr   *fakeManager
	store *MemoryStore
	c     *Coordinator
}

func newFixture() *fixture {
	f := &fixture{clock: t0, store: &MemoryStore{}}
	now := func() time.Time { return f.clock }
	f.mgr = newFakeManager(now)
	f.c = &Coordinator{Manager: f.mgr, Store: f.store, ManagerAddress: "wazuh.example.ts.net", ManagerPort: 1514, Now: now,
		ReportTimeout: time.Second, PollInterval: time.Millisecond}
	return f
}

func (f *fixture) guest() *fakeGuest { return &fakeGuest{mgr: f.mgr} }

func (f *fixture) tick() { f.clock = f.clock.Add(time.Minute) }

func (f *fixture) mapping(t *testing.T, asset string) Registration {
	t.Helper()
	r, ok, _ := f.store.Get(context.Background(), asset)
	if !ok {
		t.Fatalf("no mapping for %s", asset)
	}
	return r
}

var (
	incA = Incarnation{ID: "inst-a", Seq: 100}
	incB = Incarnation{ID: "inst-b", Seq: 200}
)

func TestEnrollDeliversIdentity(t *testing.T) {
	f := newFixture()
	g := f.guest()
	id, err := f.c.Enroll(context.Background(), "nrc:asset-1", incA, "vm-a", g)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(g.steps, ",") != "write,start" {
		t.Fatalf("guest steps = %v, want write then start", g.steps)
	}
	var got map[string]any
	if err := json.Unmarshal(g.identity, &got); err != nil {
		t.Fatal(err)
	}
	wantKey := base64.StdEncoding.EncodeToString([]byte(id + " vm-a any " + hexKey))
	want := map[string]any{"version": 1.0, "agent_id": id, "agent_name": "vm-a", "agent_key": wantKey, "manager_address": "wazuh.example.ts.net", "manager_port": 1514.0}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("identity = %v\nwant %v", got, want)
	}
	if r := f.mapping(t, "nrc:asset-1"); r.Incarnation != incA || r.AgentID != id || r.State != StateRegistered {
		t.Fatalf("mapping = %+v", r)
	}
}

func TestOrdinaryBootReusesRegistration(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	first, err := f.c.Enroll(ctx, "asset", incA, "vm-a", f.guest())
	if err != nil {
		t.Fatal(err)
	}
	// The agent went offline with the VM; that alone changes nothing.
	f.mgr.mu.Lock()
	a := f.mgr.agents[first]
	a.Status = "disconnected"
	f.mgr.agents[first] = a
	f.mgr.mu.Unlock()
	f.tick()
	g := f.guest()
	again, err := f.c.Enroll(ctx, "asset", incA, "vm-a", g)
	if err != nil {
		t.Fatal(err)
	}
	if again != first || f.mgr.registers() != 1 || len(f.mgr.deletes()) != 0 {
		t.Fatalf("reboot: id %s->%s, registers %d, deletes %v", first, again, f.mgr.registers(), f.mgr.deletes())
	}
	if len(g.steps) != 2 {
		t.Fatalf("reboot did not redeliver the identity: %v", g.steps)
	}
}

func TestRebuildRetiresOnlyMappedRegistration(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	old, err := f.c.Enroll(ctx, "asset", incA, "vm-a", f.guest())
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.c.Enroll(ctx, "asset-2", incA, "vm-z", f.guest())
	if err != nil {
		t.Fatal(err)
	}
	f.tick()
	// The rebuilt guest keeps its hostname, so the name must be freed first.
	id, err := f.c.Enroll(ctx, "asset", incB, "vm-a", f.guest())
	if err != nil {
		t.Fatal(err)
	}
	if id == old {
		t.Fatal("rebuild reused the old agent")
	}
	if d := f.mgr.deletes(); len(d) != 1 || d[0] != "delete "+old {
		t.Fatalf("deletes = %v, want only %s", d, old)
	}
	if _, err := f.mgr.Agent(ctx, other); err != nil {
		t.Fatalf("other asset's agent: %v", err)
	}
	if r := f.mapping(t, "asset"); r.Incarnation != incB || r.AgentID != id || r.RetiringID != "" {
		t.Fatalf("mapping = %+v", r)
	}
}

func TestStaleIncarnationsAreFenced(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	id, err := f.c.Enroll(ctx, "asset", incB, "vm-a", f.guest())
	if err != nil {
		t.Fatal(err)
	}
	g := f.guest()
	if _, err := f.c.Enroll(ctx, "asset", incA, "vm-a", g); !errors.Is(err, ErrStaleIncarnation) {
		t.Fatalf("older incarnation: %v", err)
	}
	if _, err := f.c.Enroll(ctx, "asset", Incarnation{ID: "inst-c", Seq: incB.Seq}, "vm-a", g); err == nil {
		t.Fatal("second incarnation with the same sequence accepted")
	}
	if err := f.c.Retire(ctx, "asset", incA); !errors.Is(err, ErrNotMapped) {
		t.Fatalf("retire by unmapped incarnation: %v", err)
	}
	if len(f.mgr.deletes()) != 0 || len(g.steps) != 0 || f.mapping(t, "asset").AgentID != id {
		t.Fatalf("fenced call had effects: deletes %v, guest %v", f.mgr.deletes(), g.steps)
	}
}

func TestInterruptedRebuildResumes(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	old, _ := f.c.Enroll(ctx, "asset", incA, "vm-a", f.guest())
	f.mgr.failDelete = errors.New("manager unavailable")
	if _, err := f.c.Enroll(ctx, "asset", incB, "vm-a", f.guest()); err == nil {
		t.Fatal("rebuild succeeded without deleting the old agent")
	}
	if r := f.mapping(t, "asset"); r.State != StateRetiring || r.RetiringID != old || r.Incarnation != incB {
		t.Fatalf("interrupted rebuild left %+v", r)
	}
	// The old incarnation is fenced even though its agent still exists.
	if _, err := f.c.Enroll(ctx, "asset", incA, "vm-a", f.guest()); !errors.Is(err, ErrStaleIncarnation) {
		t.Fatalf("old incarnation after interrupted rebuild: %v", err)
	}
	f.tick()
	id, err := f.c.Enroll(ctx, "asset", incB, "vm-a", f.guest())
	if err != nil {
		t.Fatal(err)
	}
	if d := f.mgr.deletes(); len(d) != 1 || d[0] != "delete "+old || id == old {
		t.Fatalf("resume: deletes %v, id %s", d, id)
	}
}

func TestInterruptedRegistrationAdoptsOnlyItsAgent(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	f.mgr.failRegisterAfterCreate = true
	if _, err := f.c.Enroll(ctx, "asset", incA, "vm-a", f.guest()); err == nil {
		t.Fatal("lost registration response reported success")
	}
	f.tick()
	id, err := f.c.Enroll(ctx, "asset", incA, "vm-a", f.guest())
	if err != nil {
		t.Fatal(err)
	}
	if f.mgr.registers() != 2 || len(f.mgr.agents) != 1 || f.mapping(t, "asset").AgentID != id {
		t.Fatalf("adoption: registers %d, agents %v", f.mgr.registers(), f.mgr.agents)
	}
}

func TestForeignAgentNameIsNeverTakenOver(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		added        time.Duration
	}{
		{name: "connected agent", status: "active", added: time.Hour},
		{name: "disconnected agent", status: "disconnected", added: time.Hour},
		// Never connected, but registered before this registration began.
		{name: "older unused agent", status: "never_connected", added: -time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			foreign := f.mgr.add("vm-a", tc.status, t0.Add(tc.added))
			g := f.guest()
			if _, err := f.c.Enroll(context.Background(), "asset", incA, "vm-a", g); !errors.Is(err, ErrNameExists) {
				t.Fatalf("enroll: %v", err)
			}
			if _, err := f.mgr.Agent(context.Background(), foreign); err != nil || len(f.mgr.deletes()) != 0 || len(g.steps) != 0 {
				t.Fatalf("foreign agent touched: %v, deletes %v, guest %v", err, f.mgr.deletes(), g.steps)
			}
		})
	}
}

func TestReusedAgentIDIsNotTrusted(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	id, _ := f.c.Enroll(ctx, "asset", incA, "vm-a", f.guest())
	f.mgr.mu.Lock()
	f.mgr.agents[id] = Agent{ID: id, Name: "someone-else", Status: "active"}
	f.mgr.mu.Unlock()
	g := f.guest()
	if _, err := f.c.Enroll(ctx, "asset", incA, "vm-a", g); err == nil || len(g.steps) != 0 {
		t.Fatalf("reboot reused a renamed agent: %v %v", err, g.steps)
	}
	if _, err := f.c.Enroll(ctx, "asset", incB, "vm-a", g); err == nil || len(f.mgr.deletes()) != 0 {
		t.Fatalf("rebuild deleted a renamed agent: %v %v", err, f.mgr.deletes())
	}
}

func TestLostRegistrationIsRecreated(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	old, _ := f.c.Enroll(ctx, "asset", incA, "vm-a", f.guest())
	f.mgr.mu.Lock()
	delete(f.mgr.agents, old) // removed at the manager by an operator
	f.mgr.mu.Unlock()
	id, err := f.c.Enroll(ctx, "asset", incA, "vm-a", f.guest())
	if err != nil || id == old || f.mapping(t, "asset").AgentID != id {
		t.Fatalf("recreate: %v, %s -> %s", err, old, id)
	}
}

func TestBadKeyIsNotDelivered(t *testing.T) {
	for name, key := range map[string]func(id, agent string) string{
		"other id": func(_, agent string) string {
			return base64.StdEncoding.EncodeToString([]byte("999 " + agent + " any " + hexKey))
		},
		"other name": func(id, _ string) string {
			return base64.StdEncoding.EncodeToString([]byte(id + " vm-x any " + hexKey))
		},
		"not base64": func(string, string) string { return "%%%" },
		"short key": func(id, agent string) string {
			return base64.StdEncoding.EncodeToString([]byte(id + " " + agent + " any abc"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			f.mgr.keyFor = key
			g := f.guest()
			if _, err := f.c.Enroll(context.Background(), "asset", incA, "vm-a", g); err == nil || len(g.steps) != 0 {
				t.Fatalf("bad key delivered: %v %v", err, g.steps)
			}
		})
	}
}

func TestDeliveryFailureKeepsRegistration(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	g := &fakeGuest{mgr: f.mgr, failErr: errors.New("ssh: unreachable")}
	if _, err := f.c.Enroll(ctx, "asset", incA, "vm-a", g); err == nil || len(g.steps) != 0 {
		t.Fatalf("enroll: %v, steps %v", err, g.steps)
	}
	g.failErr = nil
	if _, err := f.c.Enroll(ctx, "asset", incA, "vm-a", g); err != nil {
		t.Fatal(err)
	}
	if f.mgr.registers() != 1 || strings.Join(g.steps, ",") != "write,start" {
		t.Fatalf("retry: registers %d, steps %v", f.mgr.registers(), g.steps)
	}
}

func TestRetireDeletesMappedAgent(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	id, _ := f.c.Enroll(ctx, "asset", incA, "vm-a", f.guest())
	if err := f.c.Retire(ctx, "asset", incA); err != nil {
		t.Fatal(err)
	}
	if d := f.mgr.deletes(); len(d) != 1 || d[0] != "delete "+id {
		t.Fatalf("deletes = %v", d)
	}
	if _, ok, _ := f.store.Get(ctx, "asset"); ok {
		t.Fatal("mapping kept after retire")
	}
	if err := f.c.Retire(ctx, "asset", incA); err != nil {
		t.Fatalf("repeat retire: %v", err)
	}
}

func TestEnrollSerializesPerAsset(t *testing.T) {
	f := newFixture()
	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], _ = f.c.Enroll(context.Background(), "asset", incA, "vm-a", f.guest())
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] || id == "" {
			t.Fatalf("concurrent enrollments returned %v", ids)
		}
	}
	if f.mgr.registers() != 1 {
		t.Fatalf("registered %d times", f.mgr.registers())
	}
}

// racingStore lets a second controller change the asset just before the
// first controller's first write.
type racingStore struct {
	*MemoryStore
	before func()
}

func (s *racingStore) Put(ctx context.Context, r Registration, prev int64) error {
	if s.before != nil {
		before := s.before
		s.before = nil
		before()
	}
	return s.MemoryStore.Put(ctx, r, prev)
}

func TestControllersAreFencedByGeneration(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	f.c.Enroll(ctx, "asset", incA, "vm-a", f.guest())
	shared := &racingStore{MemoryStore: f.store}
	second := &Coordinator{Manager: f.mgr, Store: f.store, ManagerAddress: "m", ManagerPort: 1514, Now: f.c.Now, PollInterval: time.Millisecond}
	first := &Coordinator{Manager: f.mgr, Store: shared, ManagerAddress: "m", ManagerPort: 1514, Now: f.c.Now, PollInterval: time.Millisecond}
	incC := Incarnation{ID: "inst-c", Seq: 300}
	shared.before = func() {
		if _, err := second.Enroll(ctx, "asset", incC, "vm-a", f.guest()); err != nil {
			t.Errorf("second controller: %v", err)
		}
	}
	if _, err := first.Enroll(ctx, "asset", incB, "vm-a", f.guest()); !errors.Is(err, ErrConflict) {
		t.Fatalf("first controller: %v", err)
	}
	// Only the second controller's rebuild retired the original agent.
	if len(f.mgr.deletes()) != 1 || f.mapping(t, "asset").Incarnation != incC {
		t.Fatalf("deletes %v, mapping %+v", f.mgr.deletes(), f.mapping(t, "asset"))
	}
}

func TestEnrollWaitsForTheAgentToReport(t *testing.T) {
	f := newFixture()
	f.c.ReportTimeout = 50 * time.Millisecond
	// A guest that takes the identity but whose agent never connects.
	silent := &fakeGuest{mgr: newFakeManager(f.c.Now)}
	silent.mgr.add("unrelated", "active", t0)
	_, err := f.c.Enroll(context.Background(), "asset", incA, "vm-a", silent)
	if !errors.Is(err, ErrNotReporting) || !strings.Contains(err.Error(), "never_connected") {
		t.Fatalf("enroll with a silent agent: %v", err)
	}
	// The registration stays for the next attempt, which reuses it.
	id := f.mapping(t, "asset").AgentID
	if got, err := f.c.Enroll(context.Background(), "asset", incA, "vm-a", f.guest()); err != nil || got != id || f.mgr.registers() != 1 {
		t.Fatalf("retry: %v, %s -> %s, registers %d", err, id, got, f.mgr.registers())
	}
}
