package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"srv/internal/config"
	"srv/internal/model"
)

const zenGatewayReadHeaderTimeout = 5 * time.Second

type zenGatewayManager struct {
	log       *slog.Logger
	apiKey    string
	port      int
	upstream  *url.URL
	transport *http.Transport

	mu        sync.Mutex
	listeners map[string]*zenGatewayListener
}

type zenGatewayListener struct {
	spec   zenGatewaySpec
	server *http.Server
	ln     net.Listener
}

type zenGatewaySpec struct {
	name    string
	hostIP  string
	guestIP string
}

func newZenGatewayManager(cfg config.Config, logger *slog.Logger) (*zenGatewayManager, error) {
	if strings.TrimSpace(cfg.ZenAPIKey) == "" {
		return nil, nil
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	baseURL, err := url.Parse(cfg.ZenBaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse zen base url: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &zenGatewayManager{
		log:       logger,
		apiKey:    cfg.ZenAPIKey,
		port:      cfg.ZenGatewayPort,
		upstream:  baseURL,
		transport: transport,
		listeners: make(map[string]*zenGatewayListener),
	}, nil
}

func (m *zenGatewayManager) Reconcile(ctx context.Context, instances []model.Instance) error {
	if m == nil {
		return nil
	}
	desired := desiredZenGatewayInstances(instances)

	m.mu.Lock()
	defer m.mu.Unlock()

	for name, listener := range m.listeners {
		spec, keep := desired[name]
		if keep && listener.spec == spec {
			continue
		}
		listener.close()
		delete(m.listeners, name)
	}

	var errs []error
	for name, spec := range desired {
		if _, ok := m.listeners[name]; ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		listener, err := m.startListener(spec)
		if err != nil {
			errs = append(errs, fmt.Errorf("start zen gateway for %s: %w", name, err))
			continue
		}
		m.listeners[name] = listener
	}

	return errors.Join(errs...)
}

func (m *zenGatewayManager) startListener(spec zenGatewaySpec) (*zenGatewayListener, error) {
	addr := net.JoinHostPort(spec.hostIP, strconv.Itoa(m.port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	server := &http.Server{
		Handler:           m.newHandler(spec),
		ReadHeaderTimeout: zenGatewayReadHeaderTimeout,
	}
	listener := &zenGatewayListener{spec: spec, server: server, ln: ln}
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.log.Error("serve zen gateway", "instance", spec.name, "listen_addr", addr, "err", err)
		}
	}()
	m.log.Info("zen gateway ready", "instance", spec.name, "listen_addr", addr, "guest_ip", spec.guestIP)
	return listener, nil
}

func (m *zenGatewayManager) newHandler(spec zenGatewaySpec) http.Handler {
	proxy := &httputil.ReverseProxy{
		Transport: m.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = m.upstream.Scheme
			pr.Out.URL.Host = m.upstream.Host
			pr.Out.URL.Path = joinZenGatewayPath(m.upstream.Path, pr.In.URL.Path)
			pr.Out.URL.RawPath = pr.Out.URL.EscapedPath()
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = m.upstream.Host
			pr.SetXForwarded()

			applyZenGatewayAuth(pr.Out.Header, pr.In.URL.Path, m.apiKey)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, fmt.Sprintf("zen gateway proxy error: %v", err), http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/v1" && !strings.HasPrefix(r.URL.Path, "/v1/") {
			http.NotFound(w, r)
			return
		}
		if !remoteAddrMatchesGuest(r.RemoteAddr, spec.guestIP) {
			http.Error(w, "zen gateway is only reachable from the owning guest", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

func (l *zenGatewayListener) close() {
	if l == nil {
		return
	}
	if l.server != nil {
		_ = l.server.Close()
	}
	if l.ln != nil {
		_ = l.ln.Close()
	}
}

func (m *zenGatewayManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, listener := range m.listeners {
		listener.close()
		delete(m.listeners, name)
	}
}

func desiredZenGatewayInstances(instances []model.Instance) map[string]zenGatewaySpec {
	desired := make(map[string]zenGatewaySpec)
	for _, inst := range instances {
		if !shouldExposeGateway(inst) {
			continue
		}
		hostIP, ok := stripInstanceIP(inst.HostAddr)
		if !ok {
			continue
		}
		guestIP, ok := stripInstanceIP(inst.GuestAddr)
		if !ok {
			continue
		}
		desired[inst.Name] = zenGatewaySpec{name: inst.Name, hostIP: hostIP, guestIP: guestIP}
	}
	return desired
}

func shouldExposeGateway(inst model.Instance) bool {
	switch inst.State {
	case model.StateProvisioning, model.StateReady, model.StateAwaitingTailnet:
		return true
	default:
		return false
	}
}

func stripInstanceIP(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if ip, _, err := net.ParseCIDR(raw); err == nil {
		return ip.String(), true
	}
	if ip := net.ParseIP(raw); ip != nil {
		return ip.String(), true
	}
	return "", false
}

func remoteAddrMatchesGuest(remoteAddr, guestIP string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err != nil {
		host = strings.TrimSpace(remoteAddr)
	}
	remote := net.ParseIP(host)
	guest := net.ParseIP(strings.TrimSpace(guestIP))
	return remote != nil && guest != nil && remote.Equal(guest)
}

func joinZenGatewayPath(basePath, requestPath string) string {
	basePath = strings.TrimRight(basePath, "/")
	requestPath = "/" + strings.TrimLeft(requestPath, "/")
	if basePath == "" {
		return requestPath
	}
	return basePath + requestPath
}

func applyZenGatewayAuth(headers http.Header, requestPath, apiKey string) {
	headers.Del("Authorization")
	headers.Del("X-API-Key")
	headers.Del("X-Goog-Api-Key")

	trimmedPath := "/" + strings.TrimLeft(strings.TrimSpace(requestPath), "/")
	switch {
	case trimmedPath == "/v1/messages" || strings.HasPrefix(trimmedPath, "/v1/messages/"):
		headers.Set("X-API-Key", apiKey)
	case strings.HasPrefix(trimmedPath, "/v1/models/"):
		headers.Set("X-Goog-Api-Key", apiKey)
	default:
		// Keep OpenAI-style and OpenAI-compatible families on bearer auth, which
		// matches direct OpenCode traffic for `/v1/responses`, `/v1/chat/completions`,
		// and generic Zen discovery like `/v1/models`.
		headers.Set("Authorization", "Bearer "+apiKey)
	}
}

func (a *App) syncZenGatewayBestEffort() {
	if a == nil || a.zenGateway == nil || a.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	instances, err := a.store.ListInstances(ctx, false)
	if err != nil {
		a.log.Error("list instances for zen gateway sync", "err", err)
		return
	}
	if err := a.zenGateway.Reconcile(ctx, instances); err != nil {
		a.log.Error("sync zen gateways", "err", err)
	}
}

func (a *App) zenGatewayBaseURL(inst model.Instance) string {
	if a == nil || a.zenGateway == nil {
		return ""
	}
	if !shouldExposeGateway(inst) {
		return ""
	}
	hostIP, ok := stripInstanceIP(inst.HostAddr)
	if !ok {
		return ""
	}
	return fmt.Sprintf("http://%s:%d/v1", hostIP, a.cfg.ZenGatewayPort)
}
