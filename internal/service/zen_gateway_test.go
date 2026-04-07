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

func TestZenGatewayHandlerProxiesOnlyOwningGuest(t *testing.T) {
	var seenPath atomic.Value
	seenPath.Store("")
	var seenAuth atomic.Value
	seenAuth.Store("")
	var seenAPIKey atomic.Value
	seenAPIKey.Store("")
	var seenGoogKey atomic.Value
	seenGoogKey.Store("")
	var upstreamCalls atomic.Int64

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		seenPath.Store(r.URL.Path)
		seenAuth.Store(r.Header.Get("Authorization"))
		seenAPIKey.Store(r.Header.Get("X-API-Key"))
		seenGoogKey.Store(r.Header.Get("X-Goog-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL + "/zen")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	manager := &zenGatewayManager{
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		apiKey:    "zen-test-key",
		port:      11434,
		upstream:  upstreamURL,
		transport: http.DefaultTransport.(*http.Transport).Clone(),
	}
	handler := manager.newHandler(zenGatewaySpec{name: "demo", hostIP: "172.28.0.1", guestIP: "172.28.0.2"})

	for _, tc := range []struct {
		name        string
		path        string
		wantPath    string
		wantAuth    string
		wantAPIKey  string
		wantGoogKey string
	}{
		{
			name:        "openai compatible requests use bearer auth",
			path:        "/v1/chat/completions?stream=true",
			wantPath:    "/zen/v1/chat/completions",
			wantAuth:    "Bearer zen-test-key",
			wantAPIKey:  "",
			wantGoogKey: "",
		},
		{
			name:        "anthropic requests use x-api-key",
			path:        "/v1/messages",
			wantPath:    "/zen/v1/messages",
			wantAuth:    "",
			wantAPIKey:  "zen-test-key",
			wantGoogKey: "",
		},
		{
			name:        "google requests use x-goog-api-key",
			path:        "/v1/models/gemini-3-flash",
			wantPath:    "/zen/v1/models/gemini-3-flash",
			wantAuth:    "",
			wantAPIKey:  "",
			wantGoogKey: "zen-test-key",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://172.28.0.1:11434"+tc.path, strings.NewReader(`{"model":"big-pickle"}`))
			req.RemoteAddr = "172.28.0.2:4242"
			req.Header.Set("Authorization", "Bearer guest-supplied")
			req.Header.Set("X-API-Key", "guest-key")
			req.Header.Set("X-Goog-Api-Key", "guest-goog-key")
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
			}
			if got := seenPath.Load().(string); got != tc.wantPath {
				t.Fatalf("upstream path = %q, want %q", got, tc.wantPath)
			}
			if got := seenAuth.Load().(string); got != tc.wantAuth {
				t.Fatalf("Authorization = %q, want %q", got, tc.wantAuth)
			}
			if got := seenAPIKey.Load().(string); got != tc.wantAPIKey {
				t.Fatalf("X-API-Key = %q, want %q", got, tc.wantAPIKey)
			}
			if got := seenGoogKey.Load().(string); got != tc.wantGoogKey {
				t.Fatalf("X-Goog-Api-Key = %q, want %q", got, tc.wantGoogKey)
			}
		})
	}

	if got := upstreamCalls.Load(); got != 3 {
		t.Fatalf("upstream call count = %d, want 3", got)
	}

	t.Run("rejects cross guest access", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://172.28.0.1:11434/v1/models", nil)
		req.RemoteAddr = "172.28.0.99:4242"
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
		}
		if got := upstreamCalls.Load(); got != 3 {
			t.Fatalf("upstream call count = %d, want 3", got)
		}
	})
}
