package orbguest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func serve(t *testing.T, fn func(net.Conn)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orb.vsock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		fn(c)
	}()
	return path
}

func TestProtocol(t *testing.T) {
	for _, action := range []string{"ping", "bootstrap"} {
		t.Run(action, func(t *testing.T) {
			requests := make(chan map[string]any, 1)
			path := serve(t, func(c net.Conn) {
				r := bufio.NewReader(c)
				line, _ := r.ReadString('\n')
				if line != "CONNECT 4050\n" {
					t.Errorf("handshake = %q", line)
					return
				}
				fmt.Fprint(c, "OK 1234\n")
				line, _ = r.ReadString('\n')
				var req map[string]any
				if err := json.Unmarshal([]byte(line), &req); err != nil {
					t.Error(err)
				}
				requests <- req
				fmt.Fprint(c, "{\"ok\":true,\"generation\":7}\n")
			})
			var err error
			if action == "ping" {
				err = Ping(context.Background(), path)
			} else {
				err = Bootstrap(context.Background(), path, Request{Token: "fresh-secret", AMPURL: "https://ampcode.com", Proxy: "http://proxy", Generation: 7})
			}
			if err != nil {
				t.Fatal(err)
			}
			req := <-requests
			if req["action"] != action {
				t.Fatal(req)
			}
			if action == "bootstrap" && req["token"] != "fresh-secret" {
				t.Fatal("missing token")
			}
			if action == "ping" && len(req) != 1 {
				t.Fatal("ping includes credentials")
			}
		})
	}
}

func TestRejectResponses(t *testing.T) {
	for _, response := range []string{"bad\n", "{\"ok\":false,\"error\":\"secret\"}\n", strings.Repeat("x", MaxMessage) + "\n"} {
		path := serve(t, func(c net.Conn) {
			r := bufio.NewReader(c)
			r.ReadString('\n')
			fmt.Fprint(c, "OK 1\n")
			r.ReadString('\n')
			fmt.Fprint(c, response)
		})
		if err := Ping(context.Background(), path); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe result: %v", err)
		}
	}
	if err := Bootstrap(context.Background(), "unused", Request{Token: strings.Repeat("x", MaxMessage), Generation: 1}); err == nil || err.Error() != "invalid orb request" {
		t.Fatalf("oversized request validation: %v", err)
	}
}

func TestBootstrapProxyBypassField(t *testing.T) {
	// The field is optional on the wire: omitted when empty so older guest
	// images that reject unknown bootstrap keys still accept the request.
	for _, value := range []string{"", ".corp.example", "localhost,.corp.example,.corp.example,a,b,c,d,e,f", strings.Repeat("a,", 255) + "aa"} {
		requests := make(chan map[string]any, 1)
		path := serve(t, func(c net.Conn) {
			r := bufio.NewReader(c)
			line, _ := r.ReadString('\n')
			if line != "CONNECT 4050\n" {
				t.Errorf("handshake = %q", line)
				return
			}
			fmt.Fprint(c, "OK 1234\n")
			line, _ = r.ReadString('\n')
			var req map[string]any
			if err := json.Unmarshal([]byte(line), &req); err != nil {
				t.Error(err)
			}
			requests <- req
			fmt.Fprint(c, "{\"ok\":true,\"generation\":7}\n")
		})
		err := Bootstrap(context.Background(), path, Request{Token: "fresh-secret", Generation: 7, ProxyBypass: value})
		if err != nil {
			t.Fatal(err)
		}
		req := <-requests
		_, present := req["proxy_bypass"]
		if present != (value != "") {
			t.Fatalf("proxy_bypass presence = %v for %q", present, value)
		}
		if present && req["proxy_bypass"] != value {
			t.Fatalf("proxy_bypass changed in transit: got %q, want %q", req["proxy_bypass"], value)
		}
	}
}

func TestRejectGenerationAcknowledgment(t *testing.T) {
	for _, response := range []string{`{"ok":true}`, `{"ok":true,"generation":6}`, `{"ok":true,"generation":8}`} {
		t.Run(response, func(t *testing.T) {
			path := serve(t, func(c net.Conn) {
				r := bufio.NewReader(c)
				r.ReadString('\n')
				fmt.Fprint(c, "OK 1\n")
				r.ReadString('\n')
				fmt.Fprintln(c, response)
			})
			err := Bootstrap(context.Background(), path, Request{Token: "fresh-secret", Generation: 7})
			if err == nil || err.Error() != "orb guest generation acknowledgment mismatch" {
				t.Fatalf("generation acknowledgment validation: %v", err)
			}
		})
	}
}

func TestCancellation(t *testing.T) {
	accepted := make(chan struct{})
	path := serve(t, func(c net.Conn) { close(accepted); bufio.NewReader(c).ReadString('!') })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-accepted; cancel() }()
	start := time.Now()
	if err := Ping(ctx, path); err == nil {
		t.Fatal("cancellation accepted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation did not close transport")
	}
}
