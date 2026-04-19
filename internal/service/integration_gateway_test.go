package service

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"srv/internal/model"
)

func TestIntegrationGatewayHandlerInjectsHeadersAndAuth(t *testing.T) {
	t.Setenv("SRV_SECRET_OPENAI_PROD", "secret-openai")
	t.Setenv("SRV_SECRET_HEADER_TOKEN", "secret-header")
	t.Setenv("SRV_SECRET_BASIC_PASSWORD", "secret-password")

	var seenPath atomic.Value
	seenPath.Store("")
	var seenRawPath atomic.Value
	seenRawPath.Store("")
	var seenRequestURI atomic.Value
	seenRequestURI.Store("")
	var seenAuth atomic.Value
	seenAuth.Store("")
	var seenProxyAuth atomic.Value
	seenProxyAuth.Store("")
	var seenStaticHeader atomic.Value
	seenStaticHeader.Store("")
	var seenEnvHeader atomic.Value
	seenEnvHeader.Store("")
	var upstreamCalls atomic.Int64

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		seenPath.Store(r.URL.Path)
		seenRawPath.Store(r.URL.RawPath)
		seenRequestURI.Store(r.RequestURI)
		seenAuth.Store(r.Header.Get("Authorization"))
		seenProxyAuth.Store(r.Header.Get("Proxy-Authorization"))
		seenStaticHeader.Store(r.Header.Get("X-App"))
		seenEnvHeader.Store(r.Header.Get("X-Token"))
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	manager := &integrationGatewayManager{
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		port:      11435,
		transport: http.DefaultTransport.(*http.Transport).Clone(),
	}
	handler := manager.newHandler(integrationGatewaySpec{
		Name:    "demo",
		HostIP:  "172.28.0.1",
		GuestIP: "172.28.0.2",
		Routes: map[string]integrationRouteSpec{
			"openai": {
				Name:      "openai",
				TargetURL: upstream.URL + "/api",
				AuthMode:  model.IntegrationAuthBearerEnv,
				BearerEnv: "SRV_SECRET_OPENAI_PROD",
				Headers: []model.IntegrationHeader{
					{Name: "X-App", Value: "srv"},
					{Name: "X-Token", Env: "SRV_SECRET_HEADER_TOKEN"},
				},
			},
			"basic": {
				Name:             "basic",
				TargetURL:        upstream.URL + "/basic-root",
				AuthMode:         model.IntegrationAuthBasicEnv,
				BasicUser:        "alice",
				BasicPasswordEnv: "SRV_SECRET_BASIC_PASSWORD",
			},
			"public": {
				Name:      "public",
				TargetURL: upstream.URL + "/public",
				AuthMode:  model.IntegrationAuthNone,
			},
		},
	})

	t.Run("bearer auth and env headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://172.28.0.1:11435/integrations/openai/v1/models?stream=true", nil)
		req.RemoteAddr = "172.28.0.2:4242"
		req.Header.Set("Authorization", "Bearer guest-supplied")
		req.Header.Set("Proxy-Authorization", "Basic should-be-removed")
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		if got := seenPath.Load().(string); got != "/api/v1/models" {
			t.Fatalf("upstream path = %q, want %q", got, "/api/v1/models")
		}
		if got := seenAuth.Load().(string); got != "Bearer secret-openai" {
			t.Fatalf("Authorization = %q, want %q", got, "Bearer secret-openai")
		}
		if got := seenStaticHeader.Load().(string); got != "srv" {
			t.Fatalf("X-App = %q, want %q", got, "srv")
		}
		if got := seenEnvHeader.Load().(string); got != "secret-header" {
			t.Fatalf("X-Token = %q, want %q", got, "secret-header")
		}
		if got := seenProxyAuth.Load().(string); got != "" {
			t.Fatalf("Proxy-Authorization = %q, want empty", got)
		}
	})

	t.Run("basic auth is synthesized on host", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://172.28.0.1:11435/integrations/basic", nil)
		req.RemoteAddr = "172.28.0.2:4242"
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		if got := seenPath.Load().(string); got != "/basic-root/" {
			t.Fatalf("upstream path = %q, want %q", got, "/basic-root/")
		}
		if got := seenAuth.Load().(string); got != "Basic YWxpY2U6c2VjcmV0LXBhc3N3b3Jk" {
			t.Fatalf("Authorization = %q, want synthesized basic auth", got)
		}
	})

	t.Run("preserves encoded path separators for upstream APIs", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://172.28.0.1:11435/integrations/openai/v1/files/a%2Fb", nil)
		req.RemoteAddr = "172.28.0.2:4242"
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		if got := seenPath.Load().(string); got != "/api/v1/files/a/b" {
			t.Fatalf("upstream path = %q, want %q", got, "/api/v1/files/a/b")
		}
		if got := seenRawPath.Load().(string); got != "/api/v1/files/a%2Fb" {
			t.Fatalf("upstream raw path = %q, want %q", got, "/api/v1/files/a%2Fb")
		}
		if got := seenRequestURI.Load().(string); got != "/api/v1/files/a%2Fb" {
			t.Fatalf("upstream request URI = %q, want %q", got, "/api/v1/files/a%2Fb")
		}
	})

	t.Run("path traversal is rejected before proxying upstream", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://172.28.0.1:11435/integrations/openai/../../admin", nil)
		req.RemoteAddr = "172.28.0.2:4242"
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusNotFound, w.Body.String())
		}
	})

	t.Run("auth mode none strips guest auth headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://172.28.0.1:11435/integrations/public/health", nil)
		req.RemoteAddr = "172.28.0.2:4242"
		req.Header.Set("Authorization", "Bearer guest-supplied")
		req.Header.Set("Proxy-Authorization", "Basic guest-supplied")
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
		}
		if got := seenPath.Load().(string); got != "/public/health" {
			t.Fatalf("upstream path = %q, want %q", got, "/public/health")
		}
		if got := seenAuth.Load().(string); got != "" {
			t.Fatalf("Authorization = %q, want empty", got)
		}
		if got := seenProxyAuth.Load().(string); got != "" {
			t.Fatalf("Proxy-Authorization = %q, want empty", got)
		}
	})

	t.Run("rejects cross guest access", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://172.28.0.1:11435/integrations/openai/v1/models", nil)
		req.RemoteAddr = "172.28.0.99:4242"
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})

	if got := upstreamCalls.Load(); got != 4 {
		t.Fatalf("upstream call count = %d, want 4", got)
	}
}

func TestIntegrationGatewayHandlerMissingSecretReturnsBadGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	manager := &integrationGatewayManager{
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		port:      11435,
		transport: http.DefaultTransport.(*http.Transport).Clone(),
	}
	handler := manager.newHandler(integrationGatewaySpec{
		Name:    "demo",
		HostIP:  "172.28.0.1",
		GuestIP: "172.28.0.2",
		Routes: map[string]integrationRouteSpec{
			"openai": {
				Name:      "openai",
				TargetURL: upstream.URL,
				AuthMode:  model.IntegrationAuthBearerEnv,
				BearerEnv: "SRV_SECRET_MISSING",
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://172.28.0.1:11435/integrations/openai/v1/models", nil)
	req.RemoteAddr = "172.28.0.2:4242"
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusBadGateway, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "secret env SRV_SECRET_MISSING is not set") {
		t.Fatalf("body = %q", w.Body.String())
	}
}
