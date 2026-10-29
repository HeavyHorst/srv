package nethelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"srv/internal/storage"
)

type FinalizeRequest struct {
	CleanupRequest
	RequestID string `json:"request_id"`
}

type networkDeletion struct {
	FinalizeRequest
	Done bool `json:"done"`
}

func (q FinalizeRequest) Validate() error {
	if err := q.CleanupRequest.Validate(); err != nil {
		return err
	}
	if parsed, err := uuid.Parse(q.InstanceID); err != nil || parsed == uuid.Nil || parsed.String() != q.InstanceID {
		return errors.New("canonical network identity required")
	}
	// Match lifecycle admission, preserving the exact spelling of stored requests.
	// Only the instance ID is used as a filesystem path.
	_, err := uuid.Parse(q.RequestID)
	return err
}

func (c *Client) FinalizeInstanceNetwork(ctx context.Context, q FinalizeRequest) error {
	if err := q.Validate(); err != nil {
		return err
	}
	return c.post(ctx, "/network/finalize-v1", q)
}

// All network mutations share one stable lock, including their subprocesses.
// Closing (not unlocking) preserves exclusion if a child outlives the helper.
func (s *Server) lockNetwork(ctx context.Context) (context.Context, func(), error) {
	if err := storage.ValidatePath(s.stateDir); err != nil {
		return ctx, nil, err
	}
	// Provisioned by systemd; never silently recreate lost deletion records.
	f, err := os.OpenFile(filepath.Join(s.stateDir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return ctx, nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return ctx, nil, err
		}
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return storage.WithInheritedLock(ctx, f), func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			f.Close()
			return ctx, nil, err
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (s *Server) deletionPath(id string) string {
	return filepath.Join(s.stateDir, id+".json")
}

func (s *Server) checkDeletion(id string, required bool) error {
	if id == "" && !required {
		return nil // Legacy VM protocol; drain old unbound Orb jobs at upgrade.
	}
	if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil || parsed.String() != id {
		return errors.New("canonical network identity required")
	}
	_, err := os.Lstat(s.deletionPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("network incarnation has deletion intent")
}

func (s *Server) writeDeletion(d networkDeletion) error {
	f, err := os.CreateTemp(s.stateDir, ".deletion-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := json.NewEncoder(f).Encode(d); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), s.deletionPath(d.InstanceID)); err != nil {
		return err
	}
	return storage.SyncPath(s.stateDir)
}

func (s *Server) handleFinalize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, errors.New("POST required"))
		return
	}
	var q FinalizeRequest
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	if err := q.Validate(); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.finalize(r.Context(), q); err != nil {
		respondError(w, http.StatusConflict, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) finalize(ctx context.Context, q FinalizeRequest) error {
	ctx, release, err := s.lockNetwork(ctx)
	if err != nil {
		return err
	}
	defer release()
	var d networkDeletion
	data, err := os.ReadFile(s.deletionPath(q.InstanceID))
	if err == nil {
		if err := json.Unmarshal(data, &d); err != nil {
			return err
		}
		if err := d.Validate(); err != nil {
			return err
		}
		if d.OutboundInterface == "" {
			return errors.New("stored network deletion has no outbound interface")
		}
		// Routing may have changed since the first attempt. Keep the original
		// NAT interface; only the request/incarnation/TAP/CIDR are retry inputs.
		q.OutboundInterface = d.OutboundInterface
		if d.FinalizeRequest != q {
			return errors.New("network deletion binding mismatch")
		}
		if d.Done {
			// Reused TAPs/CIDRs now belong to someone else: never clean again.
			return storage.SyncPath(s.stateDir)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if q.OutboundInterface == "" {
		return errors.New("initial network finalization requires an outbound interface")
	}
	d.FinalizeRequest = q
	// Re-sync even an existing intent before allowing any destructive work.
	if err := s.writeDeletion(d); err != nil {
		return err
	}
	if err := s.cleanupNetwork(ctx, q.CleanupRequest); err != nil {
		return err
	}
	if err := verifyNetworkAbsent(ctx, q.CleanupRequest, s.output); err != nil {
		return err
	}
	d.Done = true
	return s.writeDeletion(d)
}

func (c *Client) VerifyInstanceNetworkAbsent(ctx context.Context, q CleanupRequest) error {
	if err := q.Validate(); err != nil {
		return err
	}
	return c.post(ctx, "/network/cleanup-proof-v1", q)
}

func (s *Server) handleCleanupProof(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, 405, errors.New("POST required"))
		return
	}
	var q CleanupRequest
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		respondError(w, 400, err)
		return
	}
	if err := q.Validate(); err != nil {
		respondError(w, 400, err)
		return
	}
	if err := verifyNetworkAbsent(r.Context(), q, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).Output()
	}); err != nil {
		respondError(w, 409, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// All queries must succeed. In particular an absent tool, permission failure or
// invalid JSON is never interpreted as an empty ruleset. Matching is deliberately
// conservative; unexpected rules are preserved for the operator, never removed.
func verifyNetworkAbsent(ctx context.Context, q CleanupRequest, output func(context.Context, string, ...string) ([]byte, error)) error {
	prefix, err := netip.ParsePrefix(q.NetworkCIDR)
	if err != nil {
		return err
	}
	if !prefix.Addr().Is4() || prefix.Bits() < 30 {
		return errors.New("cleanup proof requires the native IPv4 /30-or-smaller allocation")
	}
	var addresses []string
	for address := prefix.Masked().Addr(); prefix.Contains(address); address = address.Next() {
		addresses = append(addresses, regexp.QuoteMeta(address.String()))
	}
	// IPv4 boundaries allow ports, ranges, CIDRs and mapped IPv6 notation,
	// but never a prefix of a neighbouring address or another dotted number.
	ownedAddress := regexp.MustCompile(`(^|[^0-9.])(` + strings.Join(addresses, "|") + `)($|[^0-9.]|\.(?:$|[^0-9]))`)
	for _, args := range [][]string{{"ip", "-j", "link", "show"}, {"ip", "-j", "route", "show", "table", "all"}, {"ip", "-j", "rule", "show"}, {"iptables-save"}, {"ip6tables-save"}, {"nft", "-j", "list", "ruleset"}} {
		data, err := output(ctx, args[0], args[1:]...)
		if err != nil {
			return fmt.Errorf("cleanup query %s: %w", args[0], err)
		}
		if args[0] == "ip" {
			var rows []map[string]any
			if err := json.Unmarshal(data, &rows); err != nil || rows == nil {
				return errors.New("invalid ip network proof array")
			}
			for _, row := range rows {
				if row == nil {
					return errors.New("null ip network proof row")
				}
			}
		} else if args[0] == "nft" {
			var value struct {
				Rules []map[string]any `json:"nftables"`
			}
			if err := json.Unmarshal(data, &value); err != nil || value.Rules == nil {
				return errors.New("invalid nft network proof ruleset")
			}
			for _, rule := range value.Rules {
				if rule == nil {
					return errors.New("null nft network proof rule")
				}
			}
		}
		if ownedAddress.Match(data) {
			return fmt.Errorf("network residual for %s", q.NetworkCIDR)
		}
		// TAP names use exact tokens, not IPv4 punctuation boundaries.
		for _, token := range strings.FieldsFunc(string(data), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:/-", r))
		}) {
			if token == q.TapDevice {
				return fmt.Errorf("network residual for %s", q.TapDevice)
			}
		}
	}
	return nil
}
