// Package wazuh registers srv guests with a Wazuh manager and delivers each
// guest its agent identity.
//
// The controller owns every registration. A stable asset (the NRC asset ID)
// maps to one srv incarnation (an instance ID with a monotonic sequence), which
// maps to one Wazuh agent. Enroll on the mapped incarnation reuses its agent,
// so an ordinary boot never re-registers. Enroll on a newer incarnation (a clean
// rebuild) retires only the agent mapped to the asset, then registers afresh.
// Nothing here reacts to an agent disconnecting: only Enroll for a newer
// incarnation and Retire for the mapped one delete a registration.
//
// Calls are serialised per asset within a Coordinator, and every state change
// is a compare-and-swap on the stored registration, so two controllers cannot
// interleave changes to one asset.
package wazuh

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNotFound is returned by Manager lookups for unknown agents.
	ErrNotFound = errors.New("wazuh agent not found")
	// ErrNameExists is returned by Manager.Register when the name is taken.
	ErrNameExists = errors.New("wazuh agent name already registered")
	// ErrConflict is returned by Store when the stored generation moved on.
	ErrConflict = errors.New("registration changed concurrently")
	// ErrStaleIncarnation rejects an incarnation older than the mapped one.
	ErrStaleIncarnation = errors.New("incarnation is older than the mapped one")
	// ErrNotMapped rejects retiring an incarnation that does not own the asset.
	ErrNotMapped = errors.New("incarnation is not mapped to the asset")
	// ErrNotReporting means the guest took its identity but the manager never
	// saw the agent connect.
	ErrNotReporting = errors.New("wazuh agent is not reporting to the manager")
)

// Agent is a manager-side agent record.
type Agent struct {
	ID     string
	Name   string
	Status string // active, disconnected, pending, never_connected
	Added  time.Time
}

// Manager is the Wazuh manager API. Registration goes through the manager's
// local authd (POST /agents), so guests never reach authd or TCP/1515.
type Manager interface {
	Register(ctx context.Context, name string) (id string, err error)
	Key(ctx context.Context, id string) (string, error)
	Agent(ctx context.Context, id string) (Agent, error)
	AgentByName(ctx context.Context, name string) (Agent, error)
	// Delete removes exactly one agent; an unknown ID is not an error.
	Delete(ctx context.Context, id string) error
}

// Guest delivers an identity over the authenticated guest channel.
type Guest interface {
	// WriteEnrollment atomically replaces /run/srv-wazuh/enrollment.json,
	// owned by root with mode 0600.
	WriteEnrollment(ctx context.Context, identity []byte) error
	// StartEnrollment starts srv-wazuh-enroll.service and waits for it.
	StartEnrollment(ctx context.Context) error
}

// Incarnation is one srv instance of an asset. Seq must increase with every
// clean rebuild of the asset (for example the instance creation time).
type Incarnation struct {
	ID  string
	Seq int64
}

const (
	StateRegistering = "registering"
	StateRegistered  = "registered"
	StateRetiring    = "retiring"
)

// Registration is the stored mapping for one asset.
type Registration struct {
	Asset       string
	Incarnation Incarnation
	AgentName   string
	AgentID     string // empty until registered
	State       string
	// RetiringID is the agent being deleted while State is retiring.
	RetiringID string
	// RegisteringSince fences adoption of an agent created by an interrupted
	// registration: only agents added after it, never connected, qualify.
	RegisteringSince time.Time
	// Generation increments with every stored change.
	Generation int64
}

// Store persists registrations. Put stores r if the stored generation is
// prev (0 for a new asset) and sets r's generation to prev+1; Delete removes
// the asset if its generation is prev. Both return ErrConflict otherwise.
type Store interface {
	Get(ctx context.Context, asset string) (Registration, bool, error)
	Put(ctx context.Context, r Registration, prev int64) error
	Delete(ctx context.Context, asset string, prev int64) error
}

// Coordinator enrolls and retires guests.
type Coordinator struct {
	Manager        Manager
	Store          Store
	ManagerAddress string
	ManagerPort    int
	Now            func() time.Time
	// ReportTimeout bounds the wait for the agent to connect after delivery
	// (default 2 minutes), polling every PollInterval (default 5 seconds).
	ReportTimeout time.Duration
	PollInterval  time.Duration

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

var (
	assetRE   = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	nameRE    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	agentIDRE = regexp.MustCompile(`^[0-9]{3,8}$`)
	keyLineRE = regexp.MustCompile(`^([0-9]{1,8}) ([A-Za-z0-9._-]{1,128}) (any|[0-9./]{7,18}) ([0-9A-Fa-f]{64})$`)
	addressRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,62})(\.[A-Za-z0-9]([A-Za-z0-9-]{0,62}))*$`)
)

func (c *Coordinator) lock(asset string) func() {
	c.mu.Lock()
	if c.locks == nil {
		c.locks = map[string]*sync.Mutex{}
	}
	l := c.locks[asset]
	if l == nil {
		l = &sync.Mutex{}
		c.locks[asset] = l
	}
	c.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (c *Coordinator) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// put stores r with the next generation and records it in *r.
func (c *Coordinator) put(ctx context.Context, r *Registration) error {
	prev := r.Generation
	next := *r
	next.Generation = prev + 1
	if err := c.Store.Put(ctx, next, prev); err != nil {
		return err
	}
	*r = next
	return nil
}

// Enroll makes inc the asset's incarnation with a registered agent named
// name, then delivers that agent's identity to guest.
func (c *Coordinator) Enroll(ctx context.Context, asset string, inc Incarnation, name string, guest Guest) (string, error) {
	if !assetRE.MatchString(asset) || inc.ID == "" || !nameRE.MatchString(name) {
		return "", errors.New("invalid asset, incarnation or agent name")
	}
	if !addressRE.MatchString(c.ManagerAddress) || len(c.ManagerAddress) > 253 || c.ManagerPort < 1 || c.ManagerPort > 65535 {
		return "", errors.New("invalid manager address")
	}
	defer c.lock(asset)()

	r, ok, err := c.Store.Get(ctx, asset)
	if err != nil {
		return "", err
	}
	switch {
	case !ok:
		r = Registration{Asset: asset, Incarnation: inc, AgentName: name}
	case inc.Seq < r.Incarnation.Seq:
		return "", fmt.Errorf("%w: %s has %s, not %s", ErrStaleIncarnation, asset, r.Incarnation.ID, inc.ID)
	case inc.Seq == r.Incarnation.Seq && inc.ID != r.Incarnation.ID:
		return "", fmt.Errorf("incarnations %s and %s of %s share sequence %d", r.Incarnation.ID, inc.ID, asset, inc.Seq)
	case inc.ID != r.Incarnation.ID:
		// A clean rebuild: hand the mapped agent over for retirement first.
		if r.State == StateRegistered {
			r.RetiringID = r.AgentID
		}
		if r.RetiringID != "" {
			r.State = StateRetiring
		} else {
			r.State = ""
		}
		r.Incarnation, r.AgentName, r.AgentID, r.RegisteringSince = inc, name, "", time.Time{}
		if err := c.put(ctx, &r); err != nil {
			return "", err
		}
	case name != r.AgentName:
		return "", fmt.Errorf("%s is registered as %s, not %s", inc.ID, r.AgentName, name)
	}

	if r.State == StateRetiring {
		if err := c.deleteAgent(ctx, r.RetiringID, r.AgentName); err != nil {
			return "", err
		}
		r.State, r.RetiringID = "", ""
		if err := c.put(ctx, &r); err != nil {
			return "", err
		}
	}
	if r.State == StateRegistered {
		// Ordinary boot: keep the agent unless the manager lost it.
		agent, err := c.Manager.Agent(ctx, r.AgentID)
		switch {
		case errors.Is(err, ErrNotFound):
			r.State, r.AgentID = "", ""
		case err != nil:
			return "", err
		case agent.Name != r.AgentName:
			return "", fmt.Errorf("agent %s is now %q, not %q; refusing to reuse it", r.AgentID, agent.Name, r.AgentName)
		}
	}
	if r.State != StateRegistered {
		if err := c.register(ctx, &r); err != nil {
			return "", err
		}
	}
	return r.AgentID, c.deliver(ctx, r, guest)
}

func (c *Coordinator) register(ctx context.Context, r *Registration) error {
	if r.State != StateRegistering {
		r.State, r.RegisteringSince = StateRegistering, c.now()
		if err := c.put(ctx, r); err != nil {
			return err
		}
	}
	id, err := c.Manager.Register(ctx, r.AgentName)
	if errors.Is(err, ErrNameExists) {
		// Adopt only an agent an interrupted attempt of this registration
		// created: added since it began and never used by any guest.
		agent, lookupErr := c.Manager.AgentByName(ctx, r.AgentName)
		if lookupErr != nil {
			return fmt.Errorf("%w; looking it up: %v", err, lookupErr)
		}
		// The manager records dateAdd to the second. A manager clock behind
		// ours fails safe: the agent is refused, never deleted.
		if agent.Status != "never_connected" || agent.Added.Before(r.RegisteringSince.Truncate(time.Second)) {
			return fmt.Errorf("%w: %s belongs to agent %s (%s, added %s); not adopting or deleting it",
				err, r.AgentName, agent.ID, agent.Status, agent.Added.Format(time.RFC3339))
		}
		id, err = agent.ID, nil
	}
	if err != nil {
		return err
	}
	if !agentIDRE.MatchString(id) || strings.Trim(id, "0") == "" {
		return fmt.Errorf("manager returned invalid agent ID %q", id)
	}
	r.State, r.AgentID = StateRegistered, id
	return c.put(ctx, r)
}

// deleteAgent removes id after checking that it is still the agent of name,
// so a recycled ID is never deleted.
func (c *Coordinator) deleteAgent(ctx context.Context, id, name string) error {
	agent, err := c.Manager.Agent(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if agent.Name != name {
		return fmt.Errorf("agent %s is now %q, not %q; not deleting it", id, agent.Name, name)
	}
	return c.Manager.Delete(ctx, id)
}

type enrollmentFile struct {
	Version        int    `json:"version"`
	AgentID        string `json:"agent_id"`
	AgentName      string `json:"agent_name"`
	AgentKey       string `json:"agent_key"`
	ManagerAddress string `json:"manager_address"`
	ManagerPort    int    `json:"manager_port"`
}

func (c *Coordinator) deliver(ctx context.Context, r Registration, guest Guest) error {
	key, err := c.Manager.Key(ctx, r.AgentID)
	if err != nil {
		return err
	}
	// The guest checks this too; failing here keeps a wrong key off the wire.
	decoded, err := base64.StdEncoding.DecodeString(key)
	m := keyLineRE.FindStringSubmatch(string(decoded))
	if err != nil || m == nil || strings.TrimLeft(m[1], "0") != strings.TrimLeft(r.AgentID, "0") || m[2] != r.AgentName {
		return fmt.Errorf("manager returned a key that is not agent %s %s", r.AgentID, r.AgentName)
	}
	identity, err := json.Marshal(enrollmentFile{1, r.AgentID, r.AgentName, key, c.ManagerAddress, c.ManagerPort})
	if err != nil {
		return err
	}
	if err := guest.WriteEnrollment(ctx, identity); err != nil {
		return fmt.Errorf("delivering identity: %w", err)
	}
	if err := guest.StartEnrollment(ctx); err != nil {
		return fmt.Errorf("starting srv-wazuh-enroll: %w", err)
	}
	return c.awaitReporting(ctx, r)
}

// awaitReporting waits until the manager sees the agent connected with the
// delivered key, which is what proves the enrollment end to end.
func (c *Coordinator) awaitReporting(ctx context.Context, r Registration) error {
	timeout, interval := c.ReportTimeout, c.PollInterval
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := "unknown"
	for {
		agent, err := c.Manager.Agent(ctx, r.AgentID)
		switch {
		case err == nil && agent.Name != r.AgentName:
			return fmt.Errorf("agent %s is now %q, not %q", r.AgentID, agent.Name, r.AgentName)
		case err == nil && agent.Status == "active":
			return nil
		case err == nil:
			last = agent.Status
		case ctx.Err() == nil:
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: agent %s %s is %s after %s", ErrNotReporting, r.AgentID, r.AgentName, last, timeout)
		case <-time.After(interval):
		}
	}
}

// Retire deletes the asset's agent and mapping when the asset itself is
// decommissioned. Only the mapped incarnation may retire it.
func (c *Coordinator) Retire(ctx context.Context, asset string, inc Incarnation) error {
	defer c.lock(asset)()
	r, ok, err := c.Store.Get(ctx, asset)
	if err != nil || !ok {
		return err
	}
	if r.Incarnation != inc {
		return fmt.Errorf("%w: %s has %s", ErrNotMapped, asset, r.Incarnation.ID)
	}
	if r.State == StateRegistered {
		r.State, r.RetiringID, r.AgentID = StateRetiring, r.AgentID, ""
		if err := c.put(ctx, &r); err != nil {
			return err
		}
	}
	if r.State == StateRetiring {
		if err := c.deleteAgent(ctx, r.RetiringID, r.AgentName); err != nil {
			return err
		}
	}
	// An interrupted registration may have left a never-connected agent.
	if r.State == StateRegistering {
		agent, err := c.Manager.AgentByName(ctx, r.AgentName)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return err
		case agent.Status == "never_connected" && !agent.Added.Before(r.RegisteringSince.Truncate(time.Second)):
			if err := c.Manager.Delete(ctx, agent.ID); err != nil {
				return err
			}
		}
	}
	return c.Store.Delete(ctx, asset, r.Generation)
}

// MemoryStore is a Store for a single controller process.
type MemoryStore struct {
	mu   sync.Mutex
	regs map[string]Registration
}

func (s *MemoryStore) Get(_ context.Context, asset string) (Registration, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.regs[asset]
	return r, ok, nil
}

func (s *MemoryStore) Put(_ context.Context, r Registration, prev int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.regs[r.Asset].Generation != prev || r.Generation != prev+1 {
		return ErrConflict
	}
	if s.regs == nil {
		s.regs = map[string]Registration{}
	}
	s.regs[r.Asset] = r
	return nil
}

func (s *MemoryStore) Delete(_ context.Context, asset string, prev int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.regs[asset].Generation != prev {
		return ErrConflict
	}
	delete(s.regs, asset)
	return nil
}
