package nethelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os/exec"
	"regexp"
	"strings"
)

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
