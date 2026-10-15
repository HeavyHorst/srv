package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"srv/internal/model"
	"srv/internal/vmrunner"

	"tailscale.com/client/tailscale/v2"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

type tailnetLocalClient interface {
	Status(context.Context) (*ipnstate.Status, error)
	Ping(context.Context, netip.Addr, tailcfg.PingType) (*ipnstate.PingResult, error)
}

// The guest reports its own stable node ID over its serial channel using the
// nonce delivered in this attempt's MMDS. Old same-name devices are not evidence
// of enrollment. Legacy pending cold creates without a nonce fail closed.
func (p *Provisioner) waitForEnrollment(ctx context.Context, inst model.Instance) (tailscale.Device, string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.GuestReadyTimeout)
	defer cancel()
	runtime, err := p.instanceRuntimeIdentity(ctx, inst)
	if err != nil {
		return tailscale.Device{}, "", err
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		nodeID, err := enrollmentNode(inst.SerialLogPath, inst.StartRequestID)
		if err == nil {
			device, ok, lookupErr := p.findDevice(ctx, inst.Name)
			err = lookupErr
			if err == nil && ok && device.NodeID == nodeID {
				ip, probeErr := p.verifyLivePeer(ctx, device, "")
				err = probeErr
				if err == nil {
					current, runtimeErr := p.instanceRuntimeIdentity(ctx, inst)
					if runtimeErr != nil || current != runtime {
						return tailscale.Device{}, "", errors.Join(errGuestExited, runtimeErr)
					}
					return device, ip, nil
				}
			} else if err == nil {
				err = errors.New("tailnet API does not match guest enrollment report")
			}
		}
		select {
		case <-ctx.Done():
			return tailscale.Device{}, "", errors.Join(errGuestNotReady, ctx.Err(), err)
		case <-ticker.C:
		}
	}
}

func enrollmentNode(path, nonce string) (string, error) {
	if nonce == "" {
		return "", errors.New("cold enrollment has no attempt nonce; recreate using an updated base image")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	buf := make([]byte, min(info.Size(), 1<<20))
	if _, err := f.ReadAt(buf, info.Size()-int64(len(buf))); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	var found string
	for _, line := range strings.Split(string(buf), "\n") {
		_, payload, ok := strings.Cut(line, "srv-enrollment: ")
		if !ok {
			continue
		}
		var report struct {
			Nonce  string `json:"nonce"`
			NodeID string `json:"node_id"`
		}
		if json.Unmarshal([]byte(payload), &report) != nil || report.Nonce != nonce || report.NodeID == "" {
			continue
		}
		if found != "" && found != report.NodeID {
			return "", errors.New("conflicting guest enrollment identities")
		}
		found = report.NodeID
	}
	if found == "" {
		return "", errors.New("waiting for nonce-bound guest enrollment report; base image must support enrollment_nonce")
	}
	return found, nil
}

// verifyLivePeer establishes tailnet responsiveness, not SSH/application health.
// Neither LastSeen, Online nor InEngine is a fresh reachability test.
func (p *Provisioner) verifyLivePeer(ctx context.Context, device tailscale.Device, expectedIP string) (string, error) {
	if p.localClient == nil || device.NodeID == "" {
		return "", errors.New("live tailnet verification requires local API and stable node identity")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status, err := p.localClient.Status(ctx)
	if err != nil {
		return "", fmt.Errorf("read tailnet peers: %w", err)
	}
	peer, ip, err := matchingPeer(status, device, expectedIP)
	if err != nil {
		return "", err
	}
	result, err := p.localClient.Ping(ctx, ip, tailcfg.PingTSMP)
	if err != nil {
		return "", fmt.Errorf("probe tailnet guest: %w", err)
	}
	if result == nil {
		return "", errors.New("tailnet probe returned no result")
	}
	if result.Err != "" || result.IsLocalIP || result.IP != ip.String() || result.NodeIP != ip.String() {
		return "", fmt.Errorf("tailnet probe failed or returned a different target: %s", result.Err)
	}
	// A ping result does not carry an authenticated node ID. Bind its exact
	// destination to a unique assigned peer address on both sides of the probe.
	after, err := p.localClient.Status(ctx)
	if err != nil {
		return "", fmt.Errorf("recheck tailnet peers: %w", err)
	}
	current, _, err := matchingPeer(after, device, ip.String())
	if err != nil {
		return "", err
	}
	if current.PublicKey != peer.PublicKey {
		return "", errors.New("tailnet peer key changed during probe")
	}
	return ip.String(), nil
}

func matchingPeer(status *ipnstate.Status, device tailscale.Device, expectedIP string) (*ipnstate.PeerStatus, netip.Addr, error) {
	var match *ipnstate.PeerStatus
	if status != nil {
		for _, peer := range status.Peer {
			if peer != nil && string(peer.ID) == device.NodeID {
				if match != nil {
					return nil, netip.Addr{}, errors.New("ambiguous tailnet node identity")
				}
				match = peer
			}
		}
	}
	if match == nil {
		return nil, netip.Addr{}, errors.New("enrolled guest is missing from local tailnet peers")
	}
	for _, address := range device.Addresses {
		ip, err := netip.ParseAddr(address)
		if err != nil || (expectedIP != "" && address != expectedIP) || !slices.Contains(match.TailscaleIPs, ip) {
			continue
		}
		for _, peer := range status.Peer {
			if peer != nil && peer != match && slices.Contains(peer.TailscaleIPs, ip) {
				return nil, netip.Addr{}, errors.New("ambiguous tailnet guest address")
			}
		}
		return match, ip, nil
	}
	return nil, netip.Addr{}, errors.New("enrolled guest address does not match local tailnet peer")
}

// Bind the recorded PID to the instance's actual API socket, then include Linux
// process start time to detect PID reuse across a readiness observation. This
// attests the current runtime; it does not establish historical boot provenance.
func (p *Provisioner) instanceRuntimeIdentity(ctx context.Context, inst model.Instance) (string, error) {
	if inst.FirecrackerPID <= 0 || serialLogShowsGuestHalted(inst.SerialLogPath) {
		return "", errors.New("guest runtime is not running")
	}
	if inst.StartRequestID != "" {
		// Only the privileged runner may access jailed Firecracker sockets. Its
		// receipt binds this attempt to boot ID, PID, start ticks and socket peer.
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		result, err := p.startResult(ctx, inst)
		if err != nil {
			return "", err
		}
		if result.Outcome != vmrunner.StartRunning || result.PID != inst.FirecrackerPID {
			return "", errors.New("runner cannot attest the recorded runtime")
		}
		return inst.StartRequestID, nil
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", inst.SocketPath)
	if err != nil {
		return "", fmt.Errorf("verify guest API socket: %w", err)
	}
	defer conn.Close()
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		return "", err
	}
	var credentials *syscall.Ucred
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, credentialErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return "", err
	}
	if credentialErr != nil {
		return "", credentialErr
	}
	if credentials == nil || int(credentials.Pid) != inst.FirecrackerPID {
		return "", errors.New("guest API socket does not belong to recorded runtime")
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", inst.FirecrackerPID))
	if err != nil {
		return "", err
	}
	// comm may contain spaces and parentheses; the fields after its closing
	// parenthesis begin at field 3 (state); starttime is field 22.
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return "", errors.New("invalid guest process stat")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) <= 19 || fields[0] == "Z" || fields[0] == "X" {
		return "", errors.New("guest process is no longer running")
	}
	return fields[19], nil
}
