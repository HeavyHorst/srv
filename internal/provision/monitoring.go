package provision

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"srv/internal/model"
	"srv/internal/wazuh"
)

// WazuhMonitoring enrolls every ready tailnet guest with the Wazuh manager.
// The asset is the guest name under AssetPrefix, which stays the same across
// clean rebuilds; the incarnation is the instance ID, sequenced by creation
// time, so an ordinary boot reuses the agent and a rebuild replaces it.
type WazuhMonitoring struct {
	Coordinator *wazuh.Coordinator
	AssetPrefix string
	// OpenGuest connects to addr as root, accepting only the host key with the
	// given SHA256 fingerprint. The closer ends the connection.
	OpenGuest func(ctx context.Context, addr, hostKey string) (wazuh.Guest, io.Closer, error)
	// AttemptTimeout bounds one enrollment (default 5 minutes). A failed
	// attempt is retried after RetryInterval (default 30 seconds), doubling up
	// to 15 minutes, for as long as the guest stays ready.
	AttemptTimeout time.Duration
	RetryInterval  time.Duration
}

type monitoringWorker struct {
	WazuhMonitoring
	ctx context.Context
	mu  sync.Mutex
	// active holds guests with a running enrollment loop; true asks the loop
	// to run once more because the guest became ready again meanwhile.
	active map[string]bool
	wg     sync.WaitGroup
}

// SetWazuhMonitoring enables enrollment for guests that become ready after
// it is called. Retries stop when ctx ends.
func (p *Provisioner) SetWazuhMonitoring(ctx context.Context, m WazuhMonitoring) {
	if m.AttemptTimeout <= 0 {
		m.AttemptTimeout = 5 * time.Minute
	}
	if m.RetryInterval <= 0 {
		m.RetryInterval = 30 * time.Second
	}
	p.monitoring = &monitoringWorker{WazuhMonitoring: m, ctx: ctx, active: map[string]bool{}}
}

// ResumeMonitoring queues every ready tailnet guest, so guests that kept
// running across a control-plane restart are enrolled or re-delivered too.
func (p *Provisioner) ResumeMonitoring(ctx context.Context) error {
	if p.monitoring == nil {
		return nil
	}
	instances, err := p.store.ListInstances(ctx, false)
	if err != nil {
		return err
	}
	for _, inst := range instances {
		if monitorable(inst) {
			p.requestMonitoring(inst.Name)
		}
	}
	return nil
}

func monitorable(inst model.Instance) bool {
	return inst.DeletedAt == nil && inst.State == model.StateReady && inst.GuestTailnet() && inst.TailnetNodeID != "" && inst.TailscaleIP != "" && imageShipsWazuh(inst)
}

// imageShipsWazuh reports whether the guest booted an image whose build
// manifest, beside its kernel, records the Wazuh agent. Guests from older
// images, images built with SRV_WAZUH_AGENT=0 and Orbs are never contacted
// or registered. A guest whose kernel moved out of its image directory (a
// kernel upgrade) is no longer enrolled, which fails safe.
func imageShipsWazuh(inst model.Instance) bool {
	if inst.KernelPath == "" {
		return false
	}
	manifest, err := os.ReadFile(filepath.Join(filepath.Dir(inst.KernelPath), "manifest.txt"))
	return err == nil && strings.Contains("\n"+string(manifest), "\nwazuh_agent_version=")
}

// requestMonitoring starts enrolling a guest that just became ready. It
// returns at once: readiness never waits for the manager.
func (p *Provisioner) requestMonitoring(name string) {
	w := p.monitoring
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, running := w.active[name]; running {
		w.active[name] = true
		return
	}
	w.active[name] = false
	w.wg.Add(1)
	go p.monitorGuest(w, name)
}

// monitorGuest runs enrollment for one guest until it succeeds, the guest is
// no longer ready, or the worker stops. One loop per guest at a time.
func (p *Provisioner) monitorGuest(w *monitoringWorker, name string) {
	defer w.wg.Done()
	delay := w.RetryInterval
	for {
		inst, retry, err := p.enrollGuest(w, name)
		switch {
		case err == nil:
			delay = w.RetryInterval
		case retry:
			p.recordEvent(inst.ID, "wazuh", "monitoring pending; enrollment will be retried", map[string]any{"error": err.Error(), "retry_in": delay.String()})
		default:
			p.recordEvent(inst.ID, "wazuh", "monitoring not enrolled", map[string]any{"error": err.Error()})
		}
		w.mu.Lock()
		rerun := w.active[name]
		w.active[name] = false
		if !rerun && (err == nil || !retry || w.ctx.Err() != nil) {
			delete(w.active, name)
			w.mu.Unlock()
			return
		}
		w.mu.Unlock()
		if rerun {
			continue
		}
		select {
		case <-w.ctx.Done():
		case <-time.After(delay):
		}
		delay = min(delay*2, 15*time.Minute)
	}
}

// enrollGuest makes one enrollment attempt. retry reports whether a later
// attempt could succeed while the guest stays as it is.
func (p *Provisioner) enrollGuest(w *monitoringWorker, name string) (model.Instance, bool, error) {
	ctx, cancel := context.WithTimeout(w.ctx, w.AttemptTimeout)
	defer cancel()
	inst, err := p.store.GetInstance(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return inst, false, nil
	}
	if err != nil {
		return inst, true, err
	}
	if !monitorable(inst) {
		// The next readiness requests enrollment again.
		return inst, false, nil
	}
	hostKey, err := p.guestSSHHostKey(ctx, inst)
	if err != nil {
		return inst, true, err
	}
	guest, conn, err := w.OpenGuest(ctx, net.JoinHostPort(inst.TailscaleIP, "22"), hostKey)
	if err != nil {
		return inst, true, fmt.Errorf("connecting to guest: %w", err)
	}
	defer conn.Close()
	asset := w.AssetPrefix + inst.Name
	id, err := w.Coordinator.Enroll(ctx, asset, wazuh.Incarnation{ID: inst.ID, Seq: inst.CreatedAt.UnixNano()}, asset, guest)
	if err != nil {
		// A newer incarnation owns the asset; this one must never take it back.
		return inst, !errors.Is(err, wazuh.ErrStaleIncarnation), err
	}
	p.recordEvent(inst.ID, "wazuh", "wazuh agent "+id+" is reporting", map[string]any{"agent_id": id, "agent_name": asset, "ssh_host_key": hostKey})
	return inst, false, nil
}

// guestSSHHostKey pins the guest's Tailscale SSH host key as its tailnet node,
// identified by the stored node ID and address, advertises it.
func (p *Provisioner) guestSSHHostKey(ctx context.Context, inst model.Instance) (string, error) {
	if p.localClient == nil {
		return "", errors.New("tailnet local API is unavailable")
	}
	status, err := p.localClient.Status(ctx)
	if err != nil {
		return "", fmt.Errorf("read tailnet peers: %w", err)
	}
	ip, err := netip.ParseAddr(inst.TailscaleIP)
	if err != nil {
		return "", err
	}
	var keys []string
	found := 0
	for _, peer := range status.Peer {
		if peer != nil && string(peer.ID) == inst.TailnetNodeID {
			found++
			if !slices.Contains(peer.TailscaleIPs, ip) {
				return "", errors.New("guest tailnet node no longer has its recorded address")
			}
			keys = peer.SSH_HostKeys
		}
	}
	if found != 1 {
		return "", fmt.Errorf("guest tailnet node %s is not a unique local peer", inst.TailnetNodeID)
	}
	return wazuh.HostKeyFingerprint(keys)
}
