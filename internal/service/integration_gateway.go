package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"srv/internal/config"
	"srv/internal/model"
)

type integrationGatewayManager struct {
	log       *slog.Logger
	port      int
	transport *http.Transport

	mu        sync.Mutex
	listeners map[string]*integrationGatewayListener
}

type integrationGatewayListener struct {
	spec   integrationGatewaySpec
	server *http.Server
	ln     net.Listener
}

type integrationGatewaySpec struct {
	Name    string
	HostIP  string
	GuestIP string
	Routes  map[string]integrationRouteSpec
}

type integrationRouteSpec struct {
	Name             string
	TargetURL        string
	AuthMode         model.IntegrationAuthMode
	BearerEnv        string
	BasicUser        string
	BasicPasswordEnv string
	Headers          []model.IntegrationHeader
}

func newIntegrationGatewayManager(cfg config.Config, logger *slog.Logger) (*integrationGatewayManager, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &integrationGatewayManager{
		log:       logger,
		port:      cfg.IntegrationGatewayPort,
		transport: transport,
		listeners: make(map[string]*integrationGatewayListener),
	}, nil
}

func (m *integrationGatewayManager) Reconcile(ctx context.Context, desired map[string]integrationGatewaySpec) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, listener := range m.listeners {
		spec, keep := desired[name]
		if keep && reflect.DeepEqual(listener.spec, spec) {
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
			errs = append(errs, fmt.Errorf("start integration gateway for %s: %w", name, err))
			continue
		}
		m.listeners[name] = listener
	}
	return errors.Join(errs...)
}

func (m *integrationGatewayManager) startListener(spec integrationGatewaySpec) (*integrationGatewayListener, error) {
	addr := net.JoinHostPort(spec.HostIP, strconv.Itoa(m.port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	server := &http.Server{
		Handler:           m.newHandler(spec),
		ReadHeaderTimeout: gatewayReadHeaderTimeout,
	}
	listener := &integrationGatewayListener{spec: spec, server: server, ln: ln}
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.log.Error("serve integration gateway", "instance", spec.Name, "listen_addr", addr, "err", err)
		}
	}()
	m.log.Info("integration gateway ready", "instance", spec.Name, "listen_addr", addr, "guest_ip", spec.GuestIP, "routes", len(spec.Routes))
	return listener, nil
}

func (m *integrationGatewayManager) newHandler(spec integrationGatewaySpec) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !remoteAddrMatchesGuest(r.RemoteAddr, spec.GuestIP) {
			http.Error(w, "integration gateway is only reachable from the owning guest", http.StatusForbidden)
			return
		}
		integrationName, upstreamPath, ok := parseIntegrationGatewayPath(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		route, ok := spec.Routes[integrationName]
		if !ok {
			http.NotFound(w, r)
			return
		}
		targetURL, err := url.Parse(route.TargetURL)
		if err != nil {
			http.Error(w, fmt.Sprintf("integration target is invalid: %v", err), http.StatusBadGateway)
			return
		}
		if err := applyIntegrationRequestHeaders(r, route); err != nil {
			http.Error(w, fmt.Sprintf("integration configuration error: %v", err), http.StatusBadGateway)
			return
		}
		proxy := &httputil.ReverseProxy{
			Transport: m.transport,
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme = targetURL.Scheme
				pr.Out.URL.Host = targetURL.Host
				pr.Out.URL.Path, pr.Out.URL.RawPath = integrationGatewayTargetPaths(targetURL, pr.In.URL, integrationName, upstreamPath)
				pr.Out.URL.RawQuery = pr.In.URL.RawQuery
				pr.Out.Host = targetURL.Host
				pr.SetXForwarded()
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				http.Error(w, fmt.Sprintf("integration gateway proxy error: %v", err), http.StatusBadGateway)
			},
		}
		proxy.ServeHTTP(w, r)
	})
}

func (l *integrationGatewayListener) close() {
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

func (m *integrationGatewayManager) Close() {
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

func parseIntegrationGatewayPath(rawPath string) (name, upstreamPath string, ok bool) {
	trimmed := cleanIntegrationGatewayPath(rawPath)
	if !strings.HasPrefix(trimmed, "/integrations/") {
		return "", "", false
	}
	rest := strings.TrimPrefix(trimmed, "/integrations/")
	name, remainder, _ := strings.Cut(rest, "/")
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", false
	}
	if remainder == "" {
		return name, "/", true
	}
	return name, "/" + remainder, true
}

func cleanIntegrationGatewayPath(rawPath string) string {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return "/"
	}
	cleaned := path.Clean("/" + strings.TrimLeft(trimmed, "/"))
	if strings.HasSuffix(trimmed, "/") && cleaned != "/" && !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}
	return cleaned
}

func integrationGatewayTargetPaths(targetURL, requestURL *url.URL, integrationName, upstreamPath string) (string, string) {
	pathValue := joinProviderGatewayPath(targetURL.Path, upstreamPath)
	rawUpstreamPath := (&url.URL{Path: upstreamPath}).EscapedPath()
	if preservedRawPath, ok := preservedIntegrationGatewayRawPath(requestURL, integrationName, upstreamPath); ok {
		rawUpstreamPath = preservedRawPath
	}
	rawPathValue := joinProviderGatewayPath(targetURL.EscapedPath(), rawUpstreamPath)
	if rawPathValue == pathValue {
		return pathValue, ""
	}
	return pathValue, rawPathValue
}

func preservedIntegrationGatewayRawPath(requestURL *url.URL, integrationName, upstreamPath string) (string, bool) {
	if requestURL == nil || requestURL.RawPath == "" {
		return "", false
	}
	rawPrefix := "/integrations/" + integrationName
	rawUpstreamPath := strings.TrimPrefix(requestURL.RawPath, rawPrefix)
	if requestURL.RawPath != rawPrefix && !strings.HasPrefix(requestURL.RawPath, rawPrefix+"/") {
		return "", false
	}
	if rawUpstreamPath == "" {
		rawUpstreamPath = "/"
	}
	decodedRawPath, err := url.PathUnescape(requestURL.RawPath)
	if err != nil {
		return "", false
	}
	if cleanIntegrationGatewayPath(decodedRawPath) != integrationGatewayFullPath(integrationName, upstreamPath) {
		return "", false
	}
	return rawUpstreamPath, true
}

func integrationGatewayFullPath(name, upstreamPath string) string {
	fullPath := "/integrations/" + name
	if upstreamPath == "/" {
		return fullPath
	}
	return fullPath + upstreamPath
}

func applyIntegrationRequestHeaders(r *http.Request, route integrationRouteSpec) error {
	r.Header.Del("Proxy-Authorization")
	r.Header.Del("Authorization")
	for _, header := range route.Headers {
		if header.Env != "" {
			value, ok := os.LookupEnv(header.Env)
			if !ok || value == "" {
				return fmt.Errorf("secret env %s is not set", header.Env)
			}
			r.Header.Set(header.Name, value)
			continue
		}
		r.Header.Set(header.Name, header.Value)
	}
	switch route.AuthMode {
	case model.IntegrationAuthNone:
		return nil
	case model.IntegrationAuthBearerEnv:
		secret, ok := os.LookupEnv(route.BearerEnv)
		if !ok || secret == "" {
			return fmt.Errorf("secret env %s is not set", route.BearerEnv)
		}
		r.Header.Set("Authorization", "Bearer "+secret)
		return nil
	case model.IntegrationAuthBasicEnv:
		password, ok := os.LookupEnv(route.BasicPasswordEnv)
		if !ok || password == "" {
			return fmt.Errorf("secret env %s is not set", route.BasicPasswordEnv)
		}
		r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(route.BasicUser+":"+password)))
		return nil
	default:
		return fmt.Errorf("unsupported auth mode %q", route.AuthMode)
	}
}
