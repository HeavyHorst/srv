package nethelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const DefaultSocketPath = "/run/srv/net-helper.sock"

var ifaceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,15}$`)

var iptablesRuleExists = func(ctx context.Context, table, chain string, rule ...string) bool {
	checkArgs := append([]string{"-t", table, "-C", chain}, rule...)
	return exec.CommandContext(ctx, "iptables", checkArgs...).Run() == nil
}

type SetupRequest struct {
	TapDevice         string `json:"tap_device"`
	HostAddr          string `json:"host_addr"`
	NetworkCIDR       string `json:"network_cidr"`
	OutboundInterface string `json:"outbound_interface"`
}

type CleanupRequest struct {
	TapDevice         string `json:"tap_device"`
	NetworkCIDR       string `json:"network_cidr"`
	OutboundInterface string `json:"outbound_interface"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type Client struct {
	socketPath string
	httpClient *http.Client
	baseURL    string
}

func NewClient(socketPath string) *Client {
	if strings.TrimSpace(socketPath) == "" {
		socketPath = DefaultSocketPath
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		socketPath: socketPath,
		httpClient: &http.Client{Transport: transport},
		baseURL:    "http://srv-net-helper",
	}
}

func (c *Client) SetupInstanceNetwork(ctx context.Context, req SetupRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	return c.post(ctx, "/network/setup", req)
}

func (c *Client) CleanupInstanceNetwork(ctx context.Context, req CleanupRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	return c.post(ctx, "/network/cleanup", req)
}

func (c *Client) post(ctx context.Context, path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal helper request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build helper request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call network helper on %s: %w", c.socketPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		io.Copy(io.Discard, resp.Body)
		return nil
	}

	var helperErr errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&helperErr); err == nil && strings.TrimSpace(helperErr.Error) != "" {
		return errors.New(helperErr.Error)
	}
	text, _ := io.ReadAll(resp.Body)
	message := strings.TrimSpace(string(text))
	if message == "" {
		message = resp.Status
	}
	return fmt.Errorf("network helper request failed: %s", message)
}

type Runner func(ctx context.Context, name string, args ...string) error

type Server struct {
	log     *slog.Logger
	tapUser string
	runner  Runner
}

func NewServer(logger *slog.Logger, tapUser string) *Server {
	return NewServerWithRunner(logger, tapUser, defaultRunner)
}

func NewServerWithRunner(logger *slog.Logger, tapUser string, runner Runner) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if strings.TrimSpace(tapUser) == "" {
		tapUser = "srv"
	}
	return &Server{log: logger, tapUser: tapUser, runner: runner}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/network/setup", s.handleSetup)
	mux.HandleFunc("/network/cleanup", s.handleCleanup)
	return mux
}

func (s *Server) ServeUnix(ctx context.Context, socketPath, clientGroup string) error {
	listener, err := listenUnixSocket(socketPath, clientGroup)
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()

	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	err = server.Serve(listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s is not allowed", r.Method))
		return
	}
	var req SetupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, fmt.Errorf("decode setup request: %w", err))
		return
	}
	if err := req.Validate(); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.setupNetwork(r.Context(), req); err != nil {
		s.log.Error("setup instance network", "tap", req.TapDevice, "err", err)
		respondError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s is not allowed", r.Method))
		return
	}
	var req CleanupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, fmt.Errorf("decode cleanup request: %w", err))
		return
	}
	if err := req.Validate(); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.cleanupNetwork(r.Context(), req); err != nil {
		s.log.Error("cleanup instance network", "tap", req.TapDevice, "err", err)
		respondError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setupNetwork(ctx context.Context, req SetupRequest) error {
	if err := s.runner(ctx, "ip", "tuntap", "add", "dev", req.TapDevice, "mode", "tap", "user", s.tapUser); err != nil {
		return err
	}
	if err := s.runner(ctx, "ip", "addr", "add", req.HostAddr, "dev", req.TapDevice); err != nil {
		return err
	}
	if err := s.runner(ctx, "ip", "link", "set", "dev", req.TapDevice, "up"); err != nil {
		return err
	}
	if err := ensureIPTablesRule(ctx, s.runner, "nat", "POSTROUTING", "-s", req.NetworkCIDR, "-o", req.OutboundInterface, "-j", "MASQUERADE"); err != nil {
		return err
	}
	if err := ensureIPTablesRule(ctx, s.runner, "filter", "FORWARD", "-i", req.TapDevice, "-j", "ACCEPT"); err != nil {
		return err
	}
	if err := ensureIPTablesRule(ctx, s.runner, "filter", "FORWARD", "-o", req.TapDevice, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"); err != nil {
		return err
	}
	return nil
}

func (s *Server) cleanupNetwork(ctx context.Context, req CleanupRequest) error {
	var errs []string
	if req.OutboundInterface != "" {
		if err := deleteIPTablesRule(ctx, s.runner, "nat", "POSTROUTING", "-s", req.NetworkCIDR, "-o", req.OutboundInterface, "-j", "MASQUERADE"); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if err := deleteIPTablesRule(ctx, s.runner, "filter", "FORWARD", "-i", req.TapDevice, "-j", "ACCEPT"); err != nil {
		errs = append(errs, err.Error())
	}
	if err := deleteIPTablesRule(ctx, s.runner, "filter", "FORWARD", "-o", req.TapDevice, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"); err != nil {
		errs = append(errs, err.Error())
	}
	if err := s.runner(ctx, "ip", "link", "set", "dev", req.TapDevice, "down"); err != nil && !isMissingNetworkDeviceError(err) {
		errs = append(errs, err.Error())
	}
	if err := s.runner(ctx, "ip", "tuntap", "del", "dev", req.TapDevice, "mode", "tap"); err != nil && !isMissingNetworkDeviceError(err) {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (r SetupRequest) Validate() error {
	if err := validateInterfaceName("tap device", r.TapDevice); err != nil {
		return err
	}
	if err := validateCIDR("host address", r.HostAddr); err != nil {
		return err
	}
	if err := validateCIDR("network cidr", r.NetworkCIDR); err != nil {
		return err
	}
	if err := validateInterfaceName("outbound interface", r.OutboundInterface); err != nil {
		return err
	}
	return nil
}

func (r CleanupRequest) Validate() error {
	if err := validateInterfaceName("tap device", r.TapDevice); err != nil {
		return err
	}
	if err := validateCIDR("network cidr", r.NetworkCIDR); err != nil {
		return err
	}
	if strings.TrimSpace(r.OutboundInterface) != "" {
		if err := validateInterfaceName("outbound interface", r.OutboundInterface); err != nil {
			return err
		}
	}
	return nil
}

func validateInterfaceName(label, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s is required", label)
	}
	if !ifaceNamePattern.MatchString(value) {
		return fmt.Errorf("%s %q is invalid", label, value)
	}
	return nil
}

func validateCIDR(label, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s is required", label)
	}
	if _, _, err := net.ParseCIDR(value); err != nil {
		return fmt.Errorf("%s %q is invalid: %w", label, value, err)
	}
	return nil
}

func listenUnixSocket(socketPath, clientGroup string) (net.Listener, error) {
	if strings.TrimSpace(socketPath) == "" {
		socketPath = DefaultSocketPath
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return nil, fmt.Errorf("create helper socket directory: %w", err)
	}
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("helper socket path %s already exists and is not a socket", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("remove stale helper socket %s: %w", socketPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat helper socket %s: %w", socketPath, err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on helper socket %s: %w", socketPath, err)
	}
	mode := os.FileMode(0o600)
	if strings.TrimSpace(clientGroup) != "" {
		group, err := user.LookupGroup(clientGroup)
		if err != nil {
			listener.Close()
			return nil, fmt.Errorf("lookup client group %q: %w", clientGroup, err)
		}
		gid, err := strconv.Atoi(group.Gid)
		if err != nil {
			listener.Close()
			return nil, fmt.Errorf("parse gid for group %q: %w", clientGroup, err)
		}
		if err := os.Chown(socketPath, 0, gid); err != nil {
			listener.Close()
			return nil, fmt.Errorf("chown helper socket %s: %w", socketPath, err)
		}
		mode = 0o660
	}
	if err := os.Chmod(socketPath, mode); err != nil {
		listener.Close()
		return nil, fmt.Errorf("chmod helper socket %s: %w", socketPath, err)
	}
	return listener, nil
}

func ensureIPTablesRule(ctx context.Context, runner Runner, table, chain string, rule ...string) error {
	if iptablesRuleExists(ctx, table, chain, rule...) {
		return nil
	}
	addArgs := append([]string{"-t", table, "-A", chain}, rule...)
	return runner(ctx, "iptables", addArgs...)
}

func deleteIPTablesRule(ctx context.Context, runner Runner, table, chain string, rule ...string) error {
	deleteArgs := append([]string{"-t", table, "-D", chain}, rule...)
	if err := runner(ctx, "iptables", deleteArgs...); err != nil {
		text := err.Error()
		if strings.Contains(text, "No chain/target/match") || strings.Contains(text, "Bad rule") {
			return nil
		}
		return fmt.Errorf("iptables delete rule: %w: %s", err, text)
	}
	return nil
}

func defaultRunner(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func isMissingNetworkDeviceError(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "Cannot find device") || strings.Contains(text, "No such device") || strings.Contains(text, "does not exist") || errors.Is(err, syscall.ENODEV)
}

func respondError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: err.Error()})
}
