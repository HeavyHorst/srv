package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"srv/internal/config"
	"srv/internal/model"
)

type ampConnectGatewayManager struct {
	log       *slog.Logger
	port      int
	dial      func(context.Context, string, string) (net.Conn, error)
	mu        sync.Mutex
	listeners map[string]*providerGatewayListener
}

func newAmpConnectGatewayManager(cfg config.Config, logger *slog.Logger) *ampConnectGatewayManager {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	m := &ampConnectGatewayManager{log: logger, port: cfg.AmpConnectGatewayPort, listeners: make(map[string]*providerGatewayListener)}
	m.dial = m.dialPublicHTTPS
	return m
}

func (m *ampConnectGatewayManager) dialPublicHTTPS(ctx context.Context, network, address string) (net.Conn, error) {
	local, err := localInterfaceAddresses()
	if err != nil {
		return nil, fmt.Errorf("enumerate local addresses: %w", err)
	}
	validate := func(address string) error {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" {
			return errors.New("invalid resolved CONNECT target")
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !publicDestination(ip, local) {
			return fmt.Errorf("resolved CONNECT target %q is not public", host)
		}
		return nil
	}
	dialer := net.Dialer{Timeout: 10 * time.Second, ControlContext: func(_ context.Context, _, address string, _ syscall.RawConn) error {
		return validate(address)
	}}
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	if err := validate(conn.RemoteAddr().String()); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func localInterfaceAddresses() (map[netip.Addr]struct{}, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	result := make(map[netip.Addr]struct{}, len(addrs))
	for _, addr := range addrs {
		prefix, err := netip.ParsePrefix(addr.String())
		if err == nil {
			result[prefix.Addr().Unmap()] = struct{}{}
		}
	}
	return result, nil
}

var deniedIPv4Prefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.31.196.0/24"),
	netip.MustParsePrefix("192.52.193.0/24"), netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("192.175.48.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
}

func publicDestination(ip netip.Addr, local map[netip.Addr]struct{}) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	if _, found := local[ip]; found || !ip.IsGlobalUnicast() {
		return false
	}
	if ip.Is4() {
		for _, prefix := range deniedIPv4Prefixes {
			if prefix.Contains(ip) {
				return false
			}
		}
		return true
	}
	return netip.MustParsePrefix("2000::/3").Contains(ip) && !netip.MustParsePrefix("2001::/23").Contains(ip) &&
		!netip.MustParsePrefix("2001:db8::/32").Contains(ip) && !netip.MustParsePrefix("2002::/16").Contains(ip) &&
		!netip.MustParsePrefix("3fff::/20").Contains(ip)
}

func validConnectAuthority(authority string) (string, bool) {
	host, port, err := net.SplitHostPort(authority)
	if err != nil || host == "" || port != "443" || strings.Contains(host, "%") {
		return "", false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return host, true
	}
	dns := strings.TrimSuffix(host, ".")
	if dns == "" || len(dns) > 253 {
		return "", false
	}
	for _, label := range strings.Split(dns, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
				return "", false
			}
		}
	}
	return host, true
}

func (m *ampConnectGatewayManager) Reconcile(ctx context.Context, instances []model.Instance) error {
	desired := desiredProviderGatewayInstances(instances)
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, listener := range m.listeners {
		if spec, ok := desired[name]; ok && listener.spec == spec {
			continue
		}
		listener.close()
		delete(m.listeners, name)
		m.log.Info("Amp CONNECT gateway stopped", "instance", name)
	}
	var errs []error
	for name, spec := range desired {
		if _, ok := m.listeners[name]; ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		addr := net.JoinHostPort(spec.hostIP, strconv.Itoa(m.port))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			errs = append(errs, fmt.Errorf("start Amp CONNECT gateway for %s: %w", name, err))
			continue
		}
		server := &http.Server{Handler: m.newHandler(spec), ReadHeaderTimeout: gatewayReadHeaderTimeout}
		listener := &providerGatewayListener{spec: spec, server: server, ln: ln}
		m.listeners[name] = listener
		go func() {
			if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				m.log.Error("serve Amp CONNECT gateway", "instance", spec.name, "listen_addr", addr, "err", err)
			}
		}()
		m.log.Info("Amp CONNECT gateway ready for public HTTPS destinations", "instance", name, "listen_addr", addr, "guest_ip", spec.guestIP)
	}
	return errors.Join(errs...)
}

func (m *ampConnectGatewayManager) newHandler(spec providerGatewaySpec) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !remoteAddrMatchesGuest(r.RemoteAddr, spec.guestIP) {
			http.Error(w, "Amp CONNECT gateway is only reachable from the owning guest", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		host, ok := validConnectAuthority(r.Host)
		if !ok {
			http.Error(w, "CONNECT target forbidden", http.StatusForbidden)
			return
		}
		upstream, err := m.dial(r.Context(), "tcp", net.JoinHostPort(host, "443"))
		if err != nil {
			m.log.Error("dial Amp CONNECT target", "instance", spec.name, "err", err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			upstream.Close()
			http.Error(w, "tunneling unsupported", http.StatusInternalServerError)
			return
		}
		guest, rw, err := hijacker.Hijack()
		if err != nil {
			upstream.Close()
			m.log.Error("hijack Amp CONNECT connection", "instance", spec.name, "err", err)
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if err := rw.Flush(); err != nil {
			guest.Close()
			upstream.Close()
			return
		}
		go tunnelAmpConnections(guest, upstream, rw.Reader)
	})
}

func tunnelAmpConnections(guest, upstream net.Conn, buffered io.Reader) {
	defer guest.Close()
	defer upstream.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, buffered)
		if c, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(guest, upstream)
		if c, ok := guest.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
	}()
	wg.Wait()
}

func (m *ampConnectGatewayManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, listener := range m.listeners {
		listener.close()
		delete(m.listeners, name)
		m.log.Info("Amp CONNECT gateway stopped", "instance", name)
	}
}

func (a *App) ampConnectGatewayURL(inst model.Instance) string {
	if a == nil || a.ampConnectGateway == nil {
		return ""
	}
	if !shouldExposeGateway(inst) {
		return ""
	}
	hostIP, ok := stripInstanceIP(inst.HostAddr)
	if !ok {
		return ""
	}
	return "http://" + net.JoinHostPort(hostIP, strconv.Itoa(a.cfg.AmpConnectGatewayPort))
}
