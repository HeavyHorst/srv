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

	"srv/internal/model"
)

const gatewayReadHeaderTimeout = 5 * time.Second

type providerGatewayAuthFunc func(headers http.Header, requestPath, apiKey string)

type providerGatewayConfig struct {
	name      string
	apiKey    string
	port      int
	baseURL   string
	applyAuth providerGatewayAuthFunc
}

type providerGatewayManager struct {
	name      string
	log       *slog.Logger
	apiKey    string
	port      int
	upstream  *url.URL
	transport *http.Transport
	applyAuth providerGatewayAuthFunc

	mu        sync.Mutex
	listeners map[string]*providerGatewayListener
}

type providerGatewayListener struct {
	spec   providerGatewaySpec
	server *http.Server
	ln     net.Listener
}

type providerGatewaySpec struct {
	name    string
	hostIP  string
	guestIP string
}

func newProviderGatewayManager(cfg providerGatewayConfig, logger *slog.Logger) (*providerGatewayManager, error) {
	if strings.TrimSpace(cfg.apiKey) == "" {
		return nil, nil
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	baseURL, err := url.Parse(cfg.baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse %s base url: %w", cfg.name, err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &providerGatewayManager{
		name:      cfg.name,
		log:       logger,
		apiKey:    cfg.apiKey,
		port:      cfg.port,
		upstream:  baseURL,
		transport: transport,
		applyAuth: cfg.applyAuth,
		listeners: make(map[string]*providerGatewayListener),
	}, nil
}

func (m *providerGatewayManager) Reconcile(ctx context.Context, instances []model.Instance) error {
	if m == nil {
		return nil
	}
	desired := desiredProviderGatewayInstances(instances)

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
			errs = append(errs, fmt.Errorf("start %s gateway for %s: %w", m.name, name, err))
			continue
		}
		m.listeners[name] = listener
	}

	return errors.Join(errs...)
}

func (m *providerGatewayManager) startListener(spec providerGatewaySpec) (*providerGatewayListener, error) {
	addr := net.JoinHostPort(spec.hostIP, strconv.Itoa(m.port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	server := &http.Server{
		Handler:           m.newHandler(spec),
		ReadHeaderTimeout: gatewayReadHeaderTimeout,
	}
	listener := &providerGatewayListener{spec: spec, server: server, ln: ln}
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.log.Error("serve "+m.name+" gateway", "instance", spec.name, "listen_addr", addr, "err", err)
		}
	}()
	m.log.Info(m.name+" gateway ready", "instance", spec.name, "listen_addr", addr, "guest_ip", spec.guestIP)
	return listener, nil
}

func (m *providerGatewayManager) newHandler(spec providerGatewaySpec) http.Handler {
	proxy := &httputil.ReverseProxy{
		Transport: m.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = m.upstream.Scheme
			pr.Out.URL.Host = m.upstream.Host
			pr.Out.URL.Path = joinProviderGatewayPath(m.upstream.Path, pr.In.URL.Path)
			pr.Out.URL.RawPath = pr.Out.URL.EscapedPath()
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = m.upstream.Host
			pr.SetXForwarded()

			m.applyAuth(pr.Out.Header, pr.In.URL.Path, m.apiKey)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, fmt.Sprintf("%s gateway proxy error: %v", m.name, err), http.StatusBadGateway)
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
			http.Error(w, m.name+" gateway is only reachable from the owning guest", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

func (l *providerGatewayListener) close() {
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

func (m *providerGatewayManager) Close() {
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

func desiredProviderGatewayInstances(instances []model.Instance) map[string]providerGatewaySpec {
	desired := make(map[string]providerGatewaySpec)
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
		desired[inst.Name] = providerGatewaySpec{name: inst.Name, hostIP: hostIP, guestIP: guestIP}
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

func joinProviderGatewayPath(basePath, requestPath string) string {
	basePath = strings.TrimRight(basePath, "/")
	requestPath = "/" + strings.TrimLeft(requestPath, "/")
	if basePath == "" {
		return requestPath
	}
	return basePath + requestPath
}

func providerGatewayBaseURL(inst model.Instance, port int) string {
	if !shouldExposeGateway(inst) {
		return ""
	}
	hostIP, ok := stripInstanceIP(inst.HostAddr)
	if !ok {
		return ""
	}
	return fmt.Sprintf("http://%s:%d/v1", hostIP, port)
}
