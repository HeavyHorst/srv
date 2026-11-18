package nethelper

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// fakeFirewall models iptables chains as `iptables -S` lines, so setup,
// verification and cleanup are checked against the resulting ruleset rather
// than against a command transcript.
type fakeFirewall struct {
	mu     sync.Mutex
	chains map[string][]string // "binary table chain" -> rule specs
	links  map[string]bool     // TAP -> up
	calls  []string
	after  func() // called with mu held after each mutation
}

func newFakeFirewall(t *testing.T) (*fakeFirewall, *Server) {
	f := &fakeFirewall{chains: map[string][]string{}, links: map[string]bool{}}
	for _, binary := range []string{"iptables", "ip6tables"} {
		for _, chain := range []string{"INPUT", "FORWARD", "OUTPUT"} {
			f.chains[binary+" filter "+chain] = nil
		}
	}
	f.chains["iptables nat POSTROUTING"] = nil
	oldRule, oldChain := iptablesRuleExists, iptablesChainExists
	t.Cleanup(func() { iptablesRuleExists, iptablesChainExists = oldRule, oldChain })
	iptablesRuleExists = func(_ context.Context, table, chain string, rule ...string) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Contains(f.chains["iptables "+table+" "+chain], strings.Join(rule, " "))
	}
	iptablesChainExists = func(_ context.Context, table, chain string) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		_, ok := f.chains["iptables "+table+" "+chain]
		return ok
	}
	s := NewServerWithRunner(nil, "srv", f.run)
	s.stateDir, s.output = t.TempDir(), f.output
	return f, s
}

func (f *fakeFirewall) run(_ context.Context, name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	switch name {
	case "iptables", "ip6tables":
		defer f.notify()
		key := name + " " + args[1] + " " + args[3]
		rules, ok := f.chains[key]
		if !ok && args[2] != "-N" {
			return errors.New("iptables: No chain/target/match by that name.")
		}
		spec := strings.Join(args[4:], " ")
		switch args[2] {
		case "-N":
			if ok {
				return errors.New("iptables: Chain already exists.")
			}
			f.chains[key] = nil
		case "-F":
			f.chains[key] = nil
		case "-X":
			delete(f.chains, key)
		case "-A":
			f.chains[key] = append(rules, spec)
		case "-I":
			f.chains[key] = append([]string{strings.Join(args[5:], " ")}, rules...)
		case "-D":
			if n, err := strconv.Atoi(spec); err == nil {
				if n < 1 || n > len(rules) {
					return errors.New("iptables: Index of deletion too big.")
				}
				f.chains[key] = slices.Delete(rules, n-1, n)
				break
			}
			i := slices.Index(rules, spec)
			if i < 0 {
				return errors.New("iptables: Bad rule (does a matching rule exist in that chain?).")
			}
			f.chains[key] = slices.Delete(rules, i, i+1)
		}
	case "ip":
		defer f.notify()
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "tuntap add dev "):
			f.links[args[3]] = false
		case strings.HasPrefix(joined, "tuntap del dev "):
			delete(f.links, args[3])
		case strings.HasPrefix(joined, "link set dev "):
			if _, ok := f.links[args[3]]; !ok {
				return errors.New("Cannot find device")
			}
			f.links[args[3]] = args[4] == "up"
		}
	}
	return nil
}

func (f *fakeFirewall) notify() {
	if f.after != nil {
		f.after()
	}
}

func (f *fakeFirewall) output(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case name == "ip" && len(args) == 4 && args[1] == "show":
		up, ok := f.links[args[3]]
		if !ok {
			return nil, errors.New("does not exist")
		}
		flags := "BROADCAST,MULTICAST"
		if up {
			flags += ",UP,LOWER_UP"
		}
		return []byte("7: " + args[3] + ": <" + flags + "> mtu 1500 state UNKNOWN\n"), nil
	case (name == "iptables" || name == "ip6tables") && args[2] == "-S":
		rules, ok := f.chains[name+" "+args[1]+" "+args[3]]
		if !ok {
			return nil, errors.New("No chain")
		}
		lines := []string{"-N " + args[3]}
		for _, rule := range rules {
			lines = append(lines, "-A "+args[3]+" "+rule)
		}
		return []byte(strings.Join(lines, "\n") + "\n"), nil
	case (name == "iptables" || name == "ip6tables") && args[2] == "-C":
		if !slices.Contains(f.chains[name+" "+args[1]+" "+args[3]], strings.Join(args[4:], " ")) {
			return nil, errors.New("Bad rule")
		}
		return nil, nil
	}
	return nil, errors.New("unexpected query " + name)
}

// rulesFor returns every rule that names the TAP, in any chain.
func (f *fakeFirewall) rulesFor(tap string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for key, rules := range f.chains {
		if strings.HasSuffix(key, externalInstanceChain(tap)) {
			out = append(out, key)
		}
		for _, rule := range rules {
			if slices.Contains(strings.Fields(rule), tap) {
				out = append(out, key+": "+rule)
			}
		}
	}
	return out
}

func externalRequest(tap, network, guest string) SetupRequest {
	host := netip.MustParsePrefix(network).Addr().Next().String() + "/30"
	return SetupRequest{InstanceID: uuid.NewString(), TapDevice: tap, HostAddr: host, NetworkCIDR: network, OutboundInterface: "eth0",
		GuestAddr: guest, ExternalUplinks: []string{"wan0"}, DenyCIDRs: []string{"172.28.0.0/16", "198.51.100.7/24"}}
}

func TestExternalSetupConfinesGuestAheadOfOtherRules(t *testing.T) {
	f, s := newFakeFirewall(t)
	ctx := context.Background()
	// A normal VM's broad accept and a host rule already exist.
	f.chains["iptables filter FORWARD"] = []string{"-i tap-normal -j ACCEPT"}
	req := externalRequest("tap-ext", "172.28.0.4/30", "172.28.0.6/30")
	if w := networkPost(ctx, s, "/network/setup-external-v1", req); w.Code != http.StatusNoContent {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	if got := f.chains["iptables filter FORWARD"]; len(got) != 2 || got[0] != "-j "+externalForwardChain {
		t.Fatalf("FORWARD must jump to srv first: %q", got)
	}
	for hook, chain := range externalHooks {
		if got := f.chains["iptables filter "+hook]; len(got) == 0 || got[0] != "-j "+chain {
			t.Fatalf("%s must jump to %s first: %q", hook, chain, got)
		}
	}
	own := f.chains["iptables filter "+externalInstanceChain("tap-ext")]
	// Bound to the single guest address, not the /30; the canonical denial
	// of a non-canonical operator prefix; terminal drop after the uplink.
	if own[0] != "! -s 172.28.0.6/32 -j DROP" || !slices.Contains(own, "-d 198.51.100.0/24 -j DROP") ||
		!slices.Contains(own, "-d 100.64.0.0/10 -j DROP") || own[len(own)-2] != "-o wan0 -j ACCEPT" || own[len(own)-1] != "-j DROP" {
		t.Fatalf("instance chain: %q", own)
	}
	for key, rules := range f.chains {
		for _, rule := range rules {
			if rule == "-i tap-ext -j ACCEPT" || strings.Contains(rule, "172.28.0.4/30 -o eth0 -j MASQUERADE") {
				t.Fatalf("open legacy rule %s: %s", key, rule)
			}
		}
	}
	if !slices.Contains(f.chains["ip6tables filter FORWARD"], "-i tap-ext -j DROP") || !slices.Contains(f.chains["ip6tables filter INPUT"], "-i tap-ext -j DROP") {
		t.Fatalf("IPv6 is not dropped: %q", f.chains)
	}
	if in := f.chains["iptables filter "+externalInputChain]; len(in) != 2 || in[0] != "-i tap-ext -s 172.28.0.6/32 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT" {
		t.Fatalf("host replies must be bound to the guest address: %q", in)
	}
	if !f.links["tap-ext"] {
		t.Fatal("TAP is not up")
	}
	up := slices.Index(f.calls, "ip link set dev tap-ext up")
	for i, call := range f.calls {
		if strings.HasPrefix(call, "ip") && strings.Contains(call, "tables") && i > up {
			t.Fatalf("firewall changed after the TAP came up: %s", call)
		}
	}

	// Legacy endpoints never accept a confinement request and vice versa.
	if w := networkPost(ctx, s, "/network/setup-v2", req); w.Code != http.StatusBadRequest {
		t.Fatalf("legacy endpoint accepted external-only setup: %d", w.Code)
	}
	plain := req
	plain.ExternalUplinks, plain.GuestAddr, plain.DenyCIDRs = nil, "", nil
	if w := networkPost(ctx, s, "/network/setup-external-v1", plain); w.Code != http.StatusBadRequest {
		t.Fatalf("external endpoint accepted unconfined setup: %d", w.Code)
	}
	for _, guest := range []string{"172.28.0.5/30", "172.28.0.4/30", "172.28.0.7/30", "172.28.0.10/30"} {
		bad := req
		bad.GuestAddr = guest
		if w := networkPost(ctx, s, "/network/setup-external-v1", bad); w.Code != http.StatusBadRequest {
			t.Fatalf("accepted guest address %s: %d", guest, w.Code)
		}
	}
}

func TestExternalVerifyQuarantinesOnPolicyLoss(t *testing.T) {
	ctx := context.Background()
	req := externalRequest("tap-ext", "172.28.0.4/30", "172.28.0.6/30")
	for name, damage := range map[string]func(f *fakeFirewall){
		"tool inserted ahead": func(f *fakeFirewall) {
			f.chains["iptables filter FORWARD"] = append([]string{"-j DOCKER-USER"}, f.chains["iptables filter FORWARD"]...)
		},
		"hook flushed": func(f *fakeFirewall) { f.chains["iptables filter INPUT"] = nil },
		"instance chain reordered": func(f *fakeFirewall) {
			key := "iptables filter " + externalInstanceChain("tap-ext")
			rules := f.chains[key]
			f.chains[key] = append(rules[1:len(rules):len(rules)], rules[0])
		},
		"ipv6 rule removed": func(f *fakeFirewall) { f.chains["ip6tables filter OUTPUT"] = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f, s := newFakeFirewall(t)
			if w := networkPost(ctx, s, "/network/setup-external-v1", req); w.Code != http.StatusNoContent {
				t.Fatalf("setup: %d %s", w.Code, w.Body)
			}
			if w := networkPost(ctx, s, "/network/verify-external-v1", req); w.Code != http.StatusNoContent {
				t.Fatalf("intact policy: %d %s", w.Code, w.Body)
			}
			damage(f)
			if w := networkPost(ctx, s, "/network/verify-external-v1", req); w.Code != http.StatusConflict {
				t.Fatalf("damaged policy verified: %d", w.Code)
			}
			if f.links["tap-ext"] {
				t.Fatal("TAP stayed up after policy loss")
			}
			// The quarantine persists across checks until the guest is stopped,
			// so a deferred stop is retried even if the rules are restored.
			if w := networkPost(ctx, s, "/network/setup-external-v1", req); w.Code != http.StatusNoContent {
				t.Fatalf("restore rules: %d %s", w.Code, w.Body)
			}
			f.mu.Lock()
			f.links["tap-ext"] = false
			f.mu.Unlock()
			if w := networkPost(ctx, s, "/network/verify-external-v1", req); w.Code != http.StatusConflict {
				t.Fatalf("quarantined TAP verified: %d", w.Code)
			}
		})
	}
}

func TestExternalCleanupRemovesOnlyItsInstance(t *testing.T) {
	f, s := newFakeFirewall(t)
	ctx := context.Background()
	guest, other := externalRequest("tap-ext", "172.28.0.4/30", "172.28.0.6/30"), externalRequest("tap-other", "172.28.0.8/30", "172.28.0.10/30")
	for _, req := range []SetupRequest{guest, other} {
		if w := networkPost(ctx, s, "/network/setup-external-v1", req); w.Code != http.StatusNoContent {
			t.Fatalf("setup %s: %d %s", req.TapDevice, w.Code, w.Body)
		}
	}
	otherBefore := slices.Clone(f.rulesFor("tap-other"))
	cleanup := CleanupRequest{InstanceID: guest.InstanceID, TapDevice: guest.TapDevice, NetworkCIDR: guest.NetworkCIDR, OutboundInterface: "eth0"}
	if err := s.cleanupNetwork(ctx, cleanup); err != nil {
		t.Fatal(err)
	}
	if left := f.rulesFor("tap-ext"); len(left) != 0 || slices.Contains(f.chains["iptables nat POSTROUTING"], "-s 172.28.0.4/30 -j "+externalInstanceChain("tap-ext")) {
		t.Fatalf("residual rules: %q", left)
	}
	slices.Sort(otherBefore)
	otherAfter := f.rulesFor("tap-other")
	slices.Sort(otherAfter)
	if !slices.Equal(otherBefore, otherAfter) {
		t.Fatalf("cleanup changed another instance:\n%q\n%q", otherBefore, otherAfter)
	}
	if w := networkPost(ctx, s, "/network/verify-external-v1", other); w.Code != http.StatusNoContent {
		t.Fatalf("other instance lost its policy: %d %s", w.Code, w.Body)
	}
	// Retrying cleanup of an already-clean instance succeeds.
	if err := s.cleanupNetwork(ctx, cleanup); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupProofFindsEmptyExternalChain(t *testing.T) {
	q := CleanupRequest{TapDevice: "tap-ext", NetworkCIDR: "172.28.0.4/30"}
	output := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		switch name {
		case "ip":
			return []byte("[]"), nil
		case "nft":
			return []byte(`{"nftables":[]}`), nil
		case "iptables-save":
			return []byte("*filter\n:SRVX-tap-ext - [0:0]\nCOMMIT\n"), nil
		}
		return nil, nil
	}
	if err := verifyNetworkAbsent(context.Background(), q, output); err == nil {
		t.Fatal("empty per-instance chain was not reported")
	}
}

func TestExternalReapplyReplacesRunningPolicyWithoutGap(t *testing.T) {
	ctx := context.Background()
	for _, up := range []bool{true, false} {
		t.Run(fmt.Sprintf("tap-up=%v", up), func(t *testing.T) {
			f, s := newFakeFirewall(t)
			req := externalRequest("tap-ext", "172.28.0.4/30", "172.28.0.6/30")
			if w := networkPost(ctx, s, "/network/setup-external-v1", req); w.Code != http.StatusNoContent {
				t.Fatalf("setup: %d %s", w.Code, w.Body)
			}
			f.links["tap-ext"] = up // false: quarantined earlier
			own := "iptables filter " + externalInstanceChain("tap-ext")
			// While the running guest's chain is replaced, its first rule is
			// always a drop: either the temporary one or the source binding.
			f.after = func() {
				if rules := f.chains[own]; len(rules) == 0 || (rules[0] != "-j DROP" && rules[0] != "! -s 172.28.0.6/32 -j DROP") {
					t.Errorf("policy gap during reapply: %q", rules)
				}
			}
			changed := req
			changed.DenyCIDRs = []string{"172.28.0.0/16", "192.0.2.77/32"}
			if w := networkPost(ctx, s, "/network/reapply-external-v1", changed); w.Code != http.StatusNoContent {
				t.Fatalf("reapply: %d %s", w.Code, w.Body)
			}
			f.after = nil
			rules := f.chains[own]
			if !slices.Contains(rules, "-d 192.0.2.77/32 -j DROP") || rules[0] != "! -s 172.28.0.6/32 -j DROP" || rules[len(rules)-1] != "-j DROP" {
				t.Fatalf("policy not replaced: %q", rules)
			}
			if n := len(f.chains["iptables nat "+externalInstanceChain("tap-ext")]); n != 1 {
				t.Fatalf("nat chain has %d rules", n)
			}
			if f.links["tap-ext"] != up {
				t.Fatal("reapply changed the TAP link state")
			}
			if err := s.verifyExternalNetwork(ctx, changed); err != nil {
				t.Fatalf("reapplied policy does not verify: %v", err)
			}
		})
	}
	_, s := newFakeFirewall(t)
	if w := networkPost(ctx, s, "/network/reapply-external-v1", externalRequest("tap-none", "172.28.0.4/30", "172.28.0.6/30")); w.Code != http.StatusConflict {
		t.Fatalf("reapply without a TAP: %d", w.Code)
	}
}
