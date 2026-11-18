package nethelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
)

// External-only guests are confined by srv-owned chains that are jumped to
// first from the host's INPUT, FORWARD and OUTPUT chains. Guest-originated
// traffic may leave only from the guest's single address, only through the
// approved uplinks and never to special-purpose, private, tailnet, VM or
// operator-denied IPv4 destinations. Nothing may initiate traffic to the guest
// except the host's SSH client. IPv6 is dropped at the TAP.
const (
	externalForwardChain = "SRV-XFWD"
	externalInputChain   = "SRV-XIN"
	externalOutputChain  = "SRV-XOUT"
)

// Always denied to external-only guests, in addition to the request's list.
var externalDeniedIPv4 = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
}

func externalInstanceChain(tap string) string { return "SRVX-" + tap }

var iptablesChainExists = func(ctx context.Context, table, chain string) bool {
	return exec.CommandContext(ctx, "iptables", "-t", table, "-S", chain).Run() == nil
}

func (r SetupRequest) external() bool { return len(r.ExternalUplinks) > 0 }

func (r SetupRequest) validateExternal() error {
	if _, err := r.guestAddress(); err != nil {
		return err
	}
	if strings.TrimSpace(r.InstanceID) == "" {
		return errors.New("external-only networking requires an instance identity")
	}
	for _, uplink := range r.ExternalUplinks {
		if err := validateInterfaceName("external uplink", uplink); err != nil {
			return err
		}
		if uplink == r.TapDevice {
			return errors.New("external uplink must not be the guest TAP")
		}
	}
	for _, cidr := range r.DenyCIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil || !prefix.Addr().Is4() {
			return fmt.Errorf("deny cidr %q must be IPv4 CIDR", cidr)
		}
	}
	return nil
}

// guestAddress is the single guest address: inside the allocation and neither
// its network, broadcast nor the host address.
func (r SetupRequest) guestAddress() (netip.Addr, error) {
	network, err := netip.ParsePrefix(r.NetworkCIDR)
	if err != nil || !network.Addr().Is4() || network.Bits() != 30 || network != network.Masked() {
		return netip.Addr{}, errors.New("external-only networking requires the native IPv4 /30 allocation")
	}
	guest, err := netip.ParseAddr(strings.TrimSpace(strings.Split(r.GuestAddr, "/")[0]))
	if err != nil || !network.Contains(guest) {
		return netip.Addr{}, errors.New("guest address must be inside the instance allocation")
	}
	host, err := netip.ParsePrefix(r.HostAddr)
	if err != nil || host.Addr() == guest || guest == network.Addr() || guest == network.Addr().Next().Next().Next() {
		return netip.Addr{}, errors.New("guest address must be a usable non-host address")
	}
	return guest, nil
}

type externalRule struct {
	ip6   bool
	table string
	chain string
	rule  []string
}

// externalRules returns the instance's rules in installation order. Rules in
// the shared chains only match this TAP; per-instance chains are terminal.
func externalRules(r SetupRequest) []externalRule {
	guest, _ := r.guestAddress()
	tap, own := r.TapDevice, externalInstanceChain(r.TapDevice)
	var rules []externalRule
	add := func(table, chain string, rule ...string) {
		rules = append(rules, externalRule{table: table, chain: chain, rule: rule})
	}
	add("filter", own, "!", "-s", guest.String()+"/32", "-j", "DROP")
	for _, cidr := range append(append([]string(nil), externalDeniedIPv4...), r.DenyCIDRs...) {
		// iptables lists canonical prefixes; verification compares listings.
		prefix, _ := netip.ParsePrefix(strings.TrimSpace(cidr))
		add("filter", own, "-d", prefix.Masked().String(), "-j", "DROP")
	}
	for _, uplink := range r.ExternalUplinks {
		add("filter", own, "-o", uplink, "-j", "ACCEPT")
	}
	add("filter", own, "-j", "DROP")
	add("filter", externalForwardChain, "-i", tap, "-j", own)
	add("filter", externalForwardChain, "-o", tap, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT")
	add("filter", externalForwardChain, "-o", tap, "-j", "DROP")
	// Replies to the host must come from the guest address: conntrack state
	// alone would admit a spoofed source matching another host connection.
	add("filter", externalInputChain, "-i", tap, "-s", guest.String()+"/32", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT")
	add("filter", externalInputChain, "-i", tap, "-j", "DROP")
	add("filter", externalOutputChain, "-o", tap, "-p", "tcp", "-d", guest.String()+"/32", "--dport", "22", "-j", "ACCEPT")
	add("filter", externalOutputChain, "-o", tap, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT")
	add("filter", externalOutputChain, "-o", tap, "-j", "DROP")
	for _, uplink := range r.ExternalUplinks {
		add("nat", own, "-s", guest.String()+"/32", "-o", uplink, "-j", "MASQUERADE")
	}
	add("nat", "POSTROUTING", "-s", r.NetworkCIDR, "-j", own)
	for _, chain := range []string{"FORWARD", "INPUT"} {
		rules = append(rules, externalRule{ip6: true, table: "filter", chain: chain, rule: []string{"-i", tap, "-j", "DROP"}})
	}
	for _, chain := range []string{"FORWARD", "OUTPUT"} {
		rules = append(rules, externalRule{ip6: true, table: "filter", chain: chain, rule: []string{"-o", tap, "-j", "DROP"}})
	}
	return rules
}

func (e externalRule) binary() string {
	if e.ip6 {
		return "ip6tables"
	}
	return "iptables"
}

var externalHooks = map[string]string{"FORWARD": externalForwardChain, "INPUT": externalInputChain, "OUTPUT": externalOutputChain}

// setupExternalNetwork installs every rule before the TAP exists and before
// it is up; the guest is not started until this returns successfully.
func (s *Server) setupExternalNetwork(ctx context.Context, req SetupRequest) error {
	if err := s.installExternalRules(ctx, req); err != nil {
		return err
	}
	if err := s.runner(ctx, "ip", "tuntap", "add", "dev", req.TapDevice, "mode", "tap", "user", s.tapUser); err != nil {
		return err
	}
	if err := s.runner(ctx, "sysctl", "-q", "-w", "net.ipv6.conf."+req.TapDevice+".disable_ipv6=1"); err != nil {
		return err
	}
	if err := s.runner(ctx, "ip", "addr", "add", req.HostAddr, "dev", req.TapDevice); err != nil {
		return err
	}
	if err := s.verifyExternalNetwork(ctx, req); err != nil {
		return err
	}
	return s.runner(ctx, "ip", "link", "set", "dev", req.TapDevice, "up")
}

// installExternalRules makes the instance's rules match req. It is also used
// for a running guest, so the instance chains are replaced without a gap:
// filter keeps a leading DROP until the new rules are complete, and nat adds
// the new rules before removing the old ones.
func (s *Server) installExternalRules(ctx context.Context, req SetupRequest) error {
	for hook, chain := range externalHooks {
		if err := s.ensureChain(ctx, "iptables", "filter", chain); err != nil {
			return err
		}
		out, err := s.output(ctx, "iptables", "-t", "filter", "-S", hook)
		if err != nil {
			return fmt.Errorf("list %s: %w", hook, err)
		}
		// Another tool may have inserted ahead of an existing jump. Never
		// delete it first: that would open a gap for running guests. A later
		// duplicate jump is harmless because the instance rules are terminal.
		if firstAppendedRule(string(out)) != "-A "+hook+" -j "+chain {
			if err := s.runner(ctx, "iptables", "-t", "filter", "-I", hook, "1", "-j", chain); err != nil {
				return err
			}
		}
	}
	own := externalInstanceChain(req.TapDevice)
	old := map[string]int{}
	for _, table := range []string{"filter", "nat"} {
		if err := s.ensureChain(ctx, "iptables", table, own); err != nil {
			return err
		}
		n, err := s.chainLength(ctx, table, own)
		if err != nil {
			return err
		}
		old[table] = n
	}
	if old["filter"] > 0 {
		if err := s.runner(ctx, "iptables", "-t", "filter", "-I", own, "1", "-j", "DROP"); err != nil {
			return err
		}
		for i := 0; i < old["filter"]; i++ {
			if err := s.runner(ctx, "iptables", "-t", "filter", "-D", own, "2"); err != nil {
				return err
			}
		}
	}
	for _, rule := range externalRules(req) {
		if rule.chain == own {
			if err := s.runner(ctx, rule.binary(), append([]string{"-t", rule.table, "-A", rule.chain}, rule.rule...)...); err != nil {
				return err
			}
			continue
		}
		if rule.ip6 {
			// Insert ahead of host rules; shared jumps are not used for IPv6.
			if _, err := s.output(ctx, "ip6tables", append([]string{"-t", "filter", "-C", rule.chain}, rule.rule...)...); err == nil {
				continue
			}
			if err := s.runner(ctx, "ip6tables", append([]string{"-t", "filter", "-I", rule.chain, "1"}, rule.rule...)...); err != nil {
				return err
			}
			continue
		}
		if err := ensureIPTablesRule(ctx, s.runner, rule.table, rule.chain, rule.rule...); err != nil {
			return err
		}
	}
	if old["filter"] > 0 {
		if err := s.runner(ctx, "iptables", "-t", "filter", "-D", own, "1"); err != nil {
			return err
		}
	}
	for i := 0; i < old["nat"]; i++ {
		if err := s.runner(ctx, "iptables", "-t", "nat", "-D", own, "1"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) chainLength(ctx context.Context, table, chain string) (int, error) {
	out, err := s.output(ctx, "iptables", "-t", table, "-S", chain)
	if err != nil {
		return 0, fmt.Errorf("list %s: %w", chain, err)
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "-A "+chain+" ") {
			n++
		}
	}
	return n, nil
}

// handleReapplyExternal replaces a running guest's rules with the current
// policy, for example after a configuration change and service restart. It
// never changes the TAP's link state, so a quarantine stays in place. If the
// rules cannot be installed and verified, the TAP is taken down.
func (s *Server) handleReapplyExternal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, errors.New("POST required"))
		return
	}
	var req SetupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	if err := req.Validate(); err != nil || !req.external() {
		respondError(w, http.StatusBadRequest, errors.Join(errors.New("external-only request required"), err))
		return
	}
	ctx, release, err := s.lockNetwork(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}
	defer release()
	if err := s.checkDeletion(req.InstanceID, true); err != nil {
		respondError(w, http.StatusConflict, err)
		return
	}
	if _, err := s.output(ctx, "ip", "link", "show", "dev", req.TapDevice); err != nil {
		respondError(w, http.StatusConflict, errors.New("external-only TAP does not exist"))
		return
	}
	err = s.installExternalRules(ctx, req)
	if err == nil {
		err = s.verifyExternalNetwork(ctx, req)
	}
	if err != nil {
		downErr := s.runner(ctx, "ip", "link", "set", "dev", req.TapDevice, "down")
		s.log.Error("external-only policy reapply failed; TAP quarantined", "tap", req.TapDevice, "err", err, "down_err", downErr)
		respondError(w, http.StatusConflict, errors.Join(err, downErr))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ReapplyExternalNetwork replaces a running external-only guest's rules with
// req's policy. On failure the helper has already taken the TAP down.
func (c *Client) ReapplyExternalNetwork(ctx context.Context, req SetupRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	if !req.external() {
		return errors.New("external-only request required")
	}
	return c.post(ctx, "/network/reapply-external-v1", req)
}

func (s *Server) ensureChain(ctx context.Context, binary, table, chain string) error {
	err := s.runner(ctx, binary, "-t", table, "-N", chain)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}
	return nil
}

// verifyExternalNetwork proves that every instance rule is present and that
// each shared chain is still the first rule of its hook. Host firewall tools
// that later insert ahead of srv, or flush its rules, fail this check.
func (s *Server) verifyExternalNetwork(ctx context.Context, req SetupRequest) error {
	for hook, chain := range externalHooks {
		out, err := s.output(ctx, "iptables", "-t", "filter", "-S", hook)
		if err != nil {
			return fmt.Errorf("list %s: %w", hook, err)
		}
		if first := firstAppendedRule(string(out)); first != "-A "+hook+" -j "+chain {
			return fmt.Errorf("external-only jump is not the first %s rule", hook)
		}
	}
	for _, rule := range externalRules(req) {
		args := append([]string{"-t", rule.table, "-C", rule.chain}, rule.rule...)
		if _, err := s.output(ctx, rule.binary(), args...); err != nil {
			return fmt.Errorf("external-only rule missing: %s %s", rule.binary(), strings.Join(args, " "))
		}
	}
	// Order inside the instance chain matters; -C proves presence only.
	out, err := s.output(ctx, "iptables", "-t", "filter", "-S", externalInstanceChain(req.TapDevice))
	if err != nil {
		return err
	}
	var want, got []string
	for _, rule := range externalRules(req) {
		if !rule.ip6 && rule.table == "filter" && rule.chain == externalInstanceChain(req.TapDevice) {
			want = append(want, strings.Join(rule.rule, " "))
		}
	}
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "-A "+externalInstanceChain(req.TapDevice)+" "); ok {
			got = append(got, rest)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		return errors.New("external-only instance chain differs from its policy")
	}
	return nil
}

// linkUp reports the administrative UP flag from `ip link show` output.
func linkUp(show string) bool {
	_, rest, ok := strings.Cut(show, "<")
	flags, _, ok2 := strings.Cut(rest, ">")
	return ok && ok2 && slices.Contains(strings.Split(flags, ","), "UP")
}

func firstAppendedRule(listing string) string {
	for _, line := range strings.Split(listing, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "-A ") {
			return line
		}
	}
	return ""
}

// cleanupExternalNetwork is attempted for every instance. A missing instance
// chain means no external-only rules were installed for this TAP.
func (s *Server) cleanupExternalNetwork(ctx context.Context, req CleanupRequest) []string {
	own := externalInstanceChain(req.TapDevice)
	// A failed probe skips this cleanup like an absent chain; finalization's
	// absence proof still finds any rule or chain left for the TAP.
	if !iptablesChainExists(ctx, "filter", own) {
		return nil
	}
	var errs []string
	if err := deleteIPTablesRule(ctx, s.runner, "nat", "POSTROUTING", "-s", req.NetworkCIDR, "-j", own); err != nil {
		errs = append(errs, err.Error())
	}
	for _, target := range [][2]string{
		{"iptables", externalForwardChain}, {"iptables", externalInputChain}, {"iptables", externalOutputChain},
		{"ip6tables", "FORWARD"}, {"ip6tables", "INPUT"}, {"ip6tables", "OUTPUT"},
	} {
		if err := s.deleteRulesMentioning(ctx, target[0], "filter", target[1], req.TapDevice); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errs // Keep the instance chain so a retry finds this cleanup again.
	}
	for _, table := range []string{"nat", "filter"} {
		for _, op := range []string{"-F", "-X"} {
			if err := s.runner(ctx, "iptables", "-t", table, op, own); err != nil && !strings.Contains(err.Error(), "No chain") {
				errs = append(errs, err.Error())
			}
		}
	}
	return errs
}

// deleteRulesMentioning deletes every rule of chain that matches the TAP as
// its input or output interface.
func (s *Server) deleteRulesMentioning(ctx context.Context, binary, table, chain, tap string) error {
	out, err := s.output(ctx, binary, "-t", table, "-S", chain)
	if err != nil {
		return fmt.Errorf("list %s %s: %w", binary, chain, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "-A" || fields[1] != chain {
			continue
		}
		mentions := false
		for i := 2; i+1 < len(fields); i++ {
			if (fields[i] == "-i" || fields[i] == "-o") && fields[i+1] == tap {
				mentions = true
			}
		}
		if !mentions {
			continue
		}
		if err := s.runner(ctx, binary, append([]string{"-t", table, "-D"}, fields[1:]...)...); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleVerifyExternal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, errors.New("POST required"))
		return
	}
	var req SetupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	if err := req.Validate(); err != nil || !req.external() {
		respondError(w, http.StatusBadRequest, errors.Join(errors.New("external-only request required"), err))
		return
	}
	ctx, release, err := s.lockNetwork(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}
	defer release()
	link, err := s.output(ctx, "ip", "link", "show", "dev", req.TapDevice)
	if err != nil {
		w.WriteHeader(http.StatusNoContent) // No TAP: the guest has no network path.
		return
	}
	if !linkUp(string(link)) {
		// Quarantined earlier and not yet stopped: keep reporting it.
		respondError(w, http.StatusConflict, errors.New("external-only TAP is quarantined"))
		return
	}
	if err := s.verifyExternalNetwork(ctx, req); err != nil {
		// Fail closed first; the caller stops the guest afterwards.
		downErr := s.runner(ctx, "ip", "link", "set", "dev", req.TapDevice, "down")
		s.log.Error("external-only policy lost; TAP quarantined", "tap", req.TapDevice, "err", err, "down_err", downErr)
		respondError(w, http.StatusConflict, errors.Join(err, downErr))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// VerifyExternalNetwork checks a running external-only guest's policy. On a
// mismatch the helper has already taken the TAP down before returning.
func (c *Client) VerifyExternalNetwork(ctx context.Context, req SetupRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	if !req.external() {
		return errors.New("external-only request required")
	}
	return c.post(ctx, "/network/verify-external-v1", req)
}
