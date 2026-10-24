package nethelper

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type recordedCall struct {
	name string
	args []string
}

func recordingRunner(calls *[]recordedCall, failAt int) Runner {
	index := 0
	return func(_ context.Context, name string, args ...string) error {
		current := index
		index++
		*calls = append(*calls, recordedCall{name: name, args: append([]string(nil), args...)})
		if current == failAt {
			return fmt.Errorf("injected runner failure at call %d", current)
		}
		return nil
	}
}

func callStrings(calls []recordedCall) []string {
	out := make([]string, len(calls))
	for i, call := range calls {
		out[i] = strings.TrimSpace(call.name + " " + strings.Join(call.args, " "))
	}
	return out
}

func failureSetupRequest() SetupRequest {
	return SetupRequest{TapDevice: "tap-demo", HostAddr: "10.0.0.1/30", NetworkCIDR: "10.0.0.0/30", OutboundInterface: "eth0"}
}

func failureCleanupRequest() CleanupRequest {
	return CleanupRequest{TapDevice: "tap-demo", NetworkCIDR: "10.0.0.0/30", OutboundInterface: "eth0"}
}

var failureSetupCalls = []string{
	"ip tuntap add dev tap-demo mode tap user srv",
	"ip addr add 10.0.0.1/30 dev tap-demo",
	"ip link set dev tap-demo up",
	"iptables -t nat -A POSTROUTING -s 10.0.0.0/30 -o eth0 -j MASQUERADE",
	"iptables -t filter -A FORWARD -i tap-demo -j ACCEPT",
	"iptables -t filter -A FORWARD -o tap-demo -m state --state RELATED,ESTABLISHED -j ACCEPT",
}

var failureCleanupCalls = []string{
	"iptables -t nat -D POSTROUTING -s 10.0.0.0/30 -o eth0 -j MASQUERADE",
	"iptables -t filter -D FORWARD -i tap-demo -j ACCEPT",
	"iptables -t filter -D FORWARD -o tap-demo -m state --state RELATED,ESTABLISHED -j ACCEPT",
	"ip link set dev tap-demo down",
	"ip tuntap del dev tap-demo mode tap",
}

func withoutExistingRules(t *testing.T) {
	t.Helper()
	old := iptablesRuleExists
	t.Cleanup(func() { iptablesRuleExists = old })
	iptablesRuleExists = func(context.Context, string, string, ...string) bool { return false }
}

func TestSetupFailureLeavesCleanupComplete(t *testing.T) {
	withoutExistingRules(t)
	ctx := context.Background()
	for failAt := 0; failAt < len(failureSetupCalls); failAt++ {
		t.Run(fmt.Sprintf("fail-at-%d", failAt), func(t *testing.T) {
			var setupCalls []recordedCall
			server := NewServerWithRunner(nil, "srv", recordingRunner(&setupCalls, failAt))
			if err := server.setupNetwork(ctx, failureSetupRequest()); err == nil {
				t.Fatalf("setup ignored failure at call %d", failAt)
			}
			if got := callStrings(setupCalls); !reflect.DeepEqual(got, failureSetupCalls[:failAt+1]) {
				t.Fatalf("setup calls = %#v, want %#v", got, failureSetupCalls[:failAt+1])
			}

			// Cleanup cannot know which setup calls succeeded; it must attempt
			// every teardown operation anyway.
			var cleanupCalls []recordedCall
			cleanup := NewServerWithRunner(nil, "srv", recordingRunner(&cleanupCalls, -1))
			if err := cleanup.cleanupNetwork(ctx, failureCleanupRequest()); err != nil {
				t.Fatalf("cleanup after partial setup: %v", err)
			}
			if got := callStrings(cleanupCalls); !reflect.DeepEqual(got, failureCleanupCalls) {
				t.Fatalf("cleanup calls = %#v, want %#v", got, failureCleanupCalls)
			}
		})
	}
}

func TestCleanupFailureRetriesEverySideEffect(t *testing.T) {
	ctx := context.Background()
	for failAt := 0; failAt < len(failureCleanupCalls); failAt++ {
		t.Run(fmt.Sprintf("fail-at-%d", failAt), func(t *testing.T) {
			var first []recordedCall
			server := NewServerWithRunner(nil, "srv", recordingRunner(&first, failAt))
			if err := server.cleanupNetwork(ctx, failureCleanupRequest()); err == nil {
				t.Fatalf("cleanup ignored failure at call %d", failAt)
			}
			// A failure in one rule must not skip later rules, duplicate an
			// earlier one, or omit the tap device.
			if got := callStrings(first); !reflect.DeepEqual(got, failureCleanupCalls) {
				t.Fatalf("cleanup calls = %#v, want %#v", got, failureCleanupCalls)
			}

			// A fresh runner proves the retry sequence is complete; it does not
			// model kernel state convergence.
			var retry []recordedCall
			retryServer := NewServerWithRunner(nil, "srv", recordingRunner(&retry, -1))
			if err := retryServer.cleanupNetwork(ctx, failureCleanupRequest()); err != nil {
				t.Fatalf("cleanup retry: %v", err)
			}
			if got := callStrings(retry); !reflect.DeepEqual(got, failureCleanupCalls) {
				t.Fatalf("cleanup retry calls = %#v, want %#v", got, failureCleanupCalls)
			}
		})
	}
}

func TestCleanupToleratesMissingState(t *testing.T) {
	var calls int
	server := NewServerWithRunner(nil, "srv", func(_ context.Context, name string, _ ...string) error {
		calls++
		if name == "iptables" {
			return errors.New("iptables: Bad rule (does a matching rule exist in that chain?).")
		}
		return errors.New(`Cannot find device "tap-demo"`)
	})
	if err := server.cleanupNetwork(context.Background(), failureCleanupRequest()); err != nil {
		t.Fatalf("cleanup of already-absent state: %v", err)
	}
	if calls != len(failureCleanupCalls) {
		t.Fatalf("cleanup attempted %d calls, want %d", calls, len(failureCleanupCalls))
	}
}

func TestCleanupWithoutOutboundInterfaceSkipsNAT(t *testing.T) {
	var calls []recordedCall
	server := NewServerWithRunner(nil, "srv", recordingRunner(&calls, -1))
	req := failureCleanupRequest()
	req.OutboundInterface = ""
	if err := server.cleanupNetwork(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	want := failureCleanupCalls[1:]
	if got := callStrings(calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanup calls = %#v, want %#v", got, want)
	}
}

func TestSetupSkipsExistingRules(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		exists    func(table string) bool
		wantCalls []string
	}{
		{name: "none", exists: func(string) bool { return false }, wantCalls: failureSetupCalls},
		{name: "nat-only", exists: func(table string) bool { return table == "nat" }, wantCalls: append(append([]string(nil), failureSetupCalls[:3]...), failureSetupCalls[4:]...)},
		{name: "all", exists: func(string) bool { return true }, wantCalls: failureSetupCalls[:3]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := iptablesRuleExists
			t.Cleanup(func() { iptablesRuleExists = old })
			iptablesRuleExists = func(_ context.Context, table, _ string, _ ...string) bool { return tc.exists(table) }

			var calls []recordedCall
			server := NewServerWithRunner(nil, "srv", recordingRunner(&calls, -1))
			if err := server.setupNetwork(ctx, failureSetupRequest()); err != nil {
				t.Fatal(err)
			}
			if got := callStrings(calls); !reflect.DeepEqual(got, tc.wantCalls) {
				t.Fatalf("setup calls = %#v, want %#v", got, tc.wantCalls)
			}
		})
	}
}
