package service

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDeepseekGatewayHandlerProxiesOnlyOwningGuest(t *testing.T) {
	var seenPath atomic.Value
	seenPath.Store("")
	var seenAuth atomic.Value
	seenAuth.Store("")
	var upstreamCalls atomic.Int64

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		seenPath.Store(r.URL.Path)
		seenAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	manager := &deepseekGatewayManager{
		name:      "deepseek",
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		apiKey:    "deepseek-test-key",
		port:      11436,
		upstream:  upstreamURL,
		transport: http.DefaultTransport.(*http.Transport).Clone(),
		applyAuth: applyDeepseekGatewayAuth,
	}
	handler := manager.newHandler(deepseekGatewaySpec{name: "demo", hostIP: "172.28.0.1", guestIP: "172.28.0.2"})

	t.Run("requests use bearer auth", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://172.28.0.1:11436/v1/chat/completions", strings.NewReader(`{"model":"deepseek-chat","messages":[]}`))
		req.RemoteAddr = "172.28.0.2:4242"
		req.Header.Set("Authorization", "Bearer guest-supplied")
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		if got := seenPath.Load().(string); got != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q, want %q", got, "/v1/chat/completions")
		}
		if got := seenAuth.Load().(string); got != "Bearer deepseek-test-key" {
			t.Fatalf("Authorization = %q, want %q", got, "Bearer deepseek-test-key")
		}
	})

	t.Run("rejects cross guest access", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://172.28.0.1:11436/v1/models", nil)
		req.RemoteAddr = "172.28.0.99:4242"
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})

	t.Run("returns healthz", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://172.28.0.1:11436/healthz", nil)
		req.RemoteAddr = "172.28.0.2:4242"
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
		}
	})

	t.Run("returns 404 for non v1 path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://172.28.0.1:11436/other", nil)
		req.RemoteAddr = "172.28.0.2:4242"
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
	})
}
