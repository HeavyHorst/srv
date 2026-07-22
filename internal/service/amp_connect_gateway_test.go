package service

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"srv/internal/config"
	"srv/internal/model"
)

func TestAmpConnectGatewayRejectsInvalidRequests(t *testing.T) {
	manager := &ampConnectGatewayManager{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	handler := manager.newHandler(providerGatewaySpec{name: "vm", guestIP: "172.28.0.2"})
	tests := []struct {
		name, method, target, remote string
		want                         int
	}{
		{"wrong guest", http.MethodConnect, "ampcode.com:443", "172.28.0.6:1234", http.StatusForbidden},
		{"non CONNECT", http.MethodGet, "ampcode.com:443", "172.28.0.2:1234", http.StatusMethodNotAllowed},
		{"missing host", http.MethodConnect, ":443", "172.28.0.2:1234", http.StatusForbidden},
		{"wrong port", http.MethodConnect, "ampcode.com:444", "172.28.0.2:1234", http.StatusForbidden},
		{"missing port", http.MethodConnect, "example.com", "172.28.0.2:1234", http.StatusForbidden},
		{"zone identifier", http.MethodConnect, "[2001:4860:4860::8888%eth0]:443", "172.28.0.2:1234", http.StatusForbidden},
		{"invalid DNS name", http.MethodConnect, "bad_name.example:443", "172.28.0.2:1234", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/", nil)
			req.Host = tt.target
			req.RemoteAddr = tt.remote
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != tt.want {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.want)
			}
		})
	}
}

func TestAmpConnectGatewayTunnelsBidirectionally(t *testing.T) {
	proxyUpstream, testUpstream := net.Pipe()
	var dialAddress string
	manager := &ampConnectGatewayManager{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		dial: func(_ context.Context, _, address string) (net.Conn, error) {
			dialAddress = address
			return proxyUpstream, nil
		},
	}
	server := httptest.NewServer(manager.newHandler(providerGatewaySpec{name: "vm", guestIP: "127.0.0.1"}))
	defer server.Close()
	defer testUpstream.Close()

	proxyAddr := strings.TrimPrefix(server.URL, "http://")
	guest, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	_ = guest.SetDeadline(time.Now().Add(5 * time.Second))
	_ = testUpstream.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(guest, "CONNECT downloads.example.com:443 HTTP/1.1\r\nHost: downloads.example.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(guest), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if dialAddress != "downloads.example.com:443" {
		t.Fatalf("dial address = %q", dialAddress)
	}

	if _, err := guest.Write([]byte("to-upstream")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("to-upstream"))
	if _, err := io.ReadFull(testUpstream, buf); err != nil || string(buf) != "to-upstream" {
		t.Fatalf("guest to upstream = %q, err=%v", buf, err)
	}
	if _, err := testUpstream.Write([]byte("to-guest")); err != nil {
		t.Fatal(err)
	}
	buf = make([]byte, len("to-guest"))
	if _, err := io.ReadFull(guest, buf); err != nil || string(buf) != "to-guest" {
		t.Fatalf("upstream to guest = %q, err=%v", buf, err)
	}
}

func TestPublicDestinationPolicy(t *testing.T) {
	local := map[netip.Addr]struct{}{netip.MustParseAddr("8.8.4.4"): {}}
	tests := []struct {
		address string
		want    bool
	}{
		{"8.8.8.8", true},
		{"2606:4700:4700::1111", true},
		{"8.8.4.4", false}, // even public addresses assigned to this host
		{"10.0.0.1", false}, {"127.0.0.1", false}, {"169.254.169.254", false},
		{"100.64.0.1", false}, {"100.100.100.100", false},
		{"192.0.2.1", false}, {"198.18.0.1", false}, {"203.0.113.1", false},
		{"::ffff:10.0.0.1", false}, {"fc00::1", false}, {"fe80::1", false},
		{"64:ff9b::808:808", false}, {"2001:db8::1", false}, {"3fff::1", false}, {"2001:2::1", false}, {"2002:0808:0808::1", false},
	}
	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			if got := publicDestination(netip.MustParseAddr(tt.address), local); got != tt.want {
				t.Fatalf("publicDestination(%s) = %v, want %v", tt.address, got, tt.want)
			}
		})
	}
}

func TestValidConnectAuthority(t *testing.T) {
	for _, authority := range []string{"example.com:443", "[2606:4700:4700::1111]:443", "8.8.8.8:443"} {
		if _, ok := validConnectAuthority(authority); !ok {
			t.Errorf("validConnectAuthority(%q) rejected", authority)
		}
	}
	for _, authority := range []string{"example.com", "example.com:0443", ":443", "bad_name:443", "[fe80::1%eth0]:443"} {
		if _, ok := validConnectAuthority(authority); ok {
			t.Errorf("validConnectAuthority(%q) accepted", authority)
		}
	}
}

func TestAmpConnectGatewayURL(t *testing.T) {
	app := &App{
		cfg:               config.Config{AmpConnectGatewayPort: 43128},
		ampConnectGateway: &ampConnectGatewayManager{},
	}
	inst := model.Instance{State: model.StateProvisioning, HostAddr: "172.28.0.1/30"}
	if got := app.ampConnectGatewayURL(inst); got != "http://172.28.0.1:43128" {
		t.Fatalf("ampConnectGatewayURL() = %q", got)
	}
	inst.State = model.StateStopped
	if got := app.ampConnectGatewayURL(inst); got != "" {
		t.Fatalf("stopped ampConnectGatewayURL() = %q", got)
	}
}
