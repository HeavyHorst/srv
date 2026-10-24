package nethelper

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func validInterfaceName(value string) bool {
	if len(value) == 0 || len(value) > 15 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == ':' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// FuzzSetupHandler drives the real HTTP handler. A rejected request must not
// reach the privileged runner at all; an accepted request must produce exactly
// the six expected commands and satisfy the documented field constraints.
func FuzzSetupHandler(f *testing.F) {
	valid := `{"tap_device":"tap0","host_addr":"172.28.0.1/30","network_cidr":"172.28.0.0/30","outbound_interface":"eth0"}`
	f.Add([]byte(valid))
	// The handler uses json.Decoder, which accepts a second value. The oracle
	// must use the same decoder or a trailing value becomes a false failure.
	f.Add([]byte(valid + `{}`))
	f.Add([]byte(`{"tap_device":"tap0","outbound_interface":"eth0"}`))
	f.Add([]byte(`{"tap_device":"tap0"}`))
	f.Add([]byte(`not json`))
	f.Fuzz(func(t *testing.T, body []byte) {
		var calls [][]string
		server := NewServerWithRunner(nil, "srv", func(_ context.Context, name string, args ...string) error {
			calls = append(calls, append([]string{name}, args...))
			return nil
		})
		previous := iptablesRuleExists
		iptablesRuleExists = func(context.Context, string, string, ...string) bool { return false }
		defer func() { iptablesRuleExists = previous }()

		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/network/setup", bytes.NewReader(body)))

		if recorder.Code != http.StatusNoContent {
			if len(calls) != 0 {
				t.Fatalf("rejected request (%d) reached the runner %d times", recorder.Code, len(calls))
			}
			return
		}
		if len(calls) != 6 {
			t.Fatalf("accepted request ran %d commands, want 6", len(calls))
		}

		var req SetupRequest
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
			t.Fatalf("accepted request does not decode: %v", err)
		}
		for label, value := range map[string]string{"tap device": req.TapDevice, "outbound interface": req.OutboundInterface} {
			value = strings.TrimSpace(value)
			if !validInterfaceName(value) {
				t.Fatalf("accepted invalid %s %q", label, value)
			}
		}
		for label, value := range map[string]string{"host address": req.HostAddr, "network cidr": req.NetworkCIDR} {
			if _, _, err := net.ParseCIDR(strings.TrimSpace(value)); err != nil {
				t.Fatalf("accepted invalid %s %q: %v", label, value, err)
			}
		}
		want := [][]string{
			{"ip", "tuntap", "add", "dev", req.TapDevice, "mode", "tap", "user", "srv"},
			{"ip", "addr", "add", req.HostAddr, "dev", req.TapDevice},
			{"ip", "link", "set", "dev", req.TapDevice, "up"},
			{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", req.NetworkCIDR, "-o", req.OutboundInterface, "-j", "MASQUERADE"},
			{"iptables", "-t", "filter", "-A", "FORWARD", "-i", req.TapDevice, "-j", "ACCEPT"},
			{"iptables", "-t", "filter", "-A", "FORWARD", "-o", req.TapDevice, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("calls = %#v, want %#v", calls, want)
		}
	})
}
