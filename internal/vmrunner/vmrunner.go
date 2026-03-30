package vmrunner

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
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

const DefaultSocketPath = "/run/srv-vm-runner/vm-runner.sock"

var (
	validName           = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	ifaceName           = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,15}$`)
	vmContextForRequest = func(context.Context) context.Context {
		return context.Background()
	}
	currentCgroupPath = func() (string, error) {
		return readUnifiedCgroupPath("/proc/self/cgroup")
	}
	cgroupFSRoot = "/sys/fs/cgroup"
)

type Bootstrap struct {
	Version             int      `json:"version"`
	Hostname            string   `json:"hostname"`
	TailscaleAuthKey    string   `json:"tailscale_auth_key,omitempty"`
	TailscaleControlURL string   `json:"tailscale_control_url,omitempty"`
	TailscaleTags       []string `json:"tailscale_tags,omitempty"`
}

type Metadata struct {
	SRV Bootstrap `json:"srv"`
}

type StartRequest struct {
	Name        string    `json:"name"`
	TapDevice   string    `json:"tap_device"`
	GuestMAC    string    `json:"guest_mac"`
	GuestAddr   string    `json:"guest_addr"`
	GatewayAddr string    `json:"gateway_addr"`
	Nameservers []string  `json:"nameservers,omitempty"`
	VCPUCount   int64     `json:"vcpu_count"`
	MemoryMiB   int64     `json:"memory_mib"`
	KernelArgs  string    `json:"kernel_args,omitempty"`
	Bootstrap   Bootstrap `json:"bootstrap"`
}

type StartResponse struct {
	PID int `json:"pid"`
}

type StopRequest struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`
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
		baseURL:    "http://srv-vm-runner",
	}
}

func (c *Client) StartInstanceVM(ctx context.Context, req StartRequest) (StartResponse, error) {
	if err := req.Validate(); err != nil {
		return StartResponse{}, err
	}
	var resp StartResponse
	if err := c.post(ctx, "/vm/start", req, &resp); err != nil {
		return StartResponse{}, err
	}
	return resp, nil
}

func (c *Client) StopInstanceVM(ctx context.Context, req StopRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	return c.post(ctx, "/vm/stop", req, nil)
}

func (c *Client) post(ctx context.Context, path string, payload any, out any) error {
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
		return fmt.Errorf("call vm runner on %s: %w", c.socketPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode helper response: %w", err)
		}
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
	return fmt.Errorf("vm runner request failed: %s", message)
}

type StartFunc func(context.Context, StartRequest) (StartResponse, error)
type StopFunc func(context.Context, StopRequest) error

type ServerConfig struct {
	FirecrackerBinary string
	JailerBinary      string
	JailerBaseDir     string
	JailerUID         int
	JailerGID         int
	InstancesDir      string
	KernelPath        string
	InitrdPath        string
}

type instanceRuntimePaths struct {
	SocketPath string
	LogPath    string
	SerialLog  string
	RootFSPath string
}

type jailerRuntimePaths struct {
	WorkspaceDir string
	RootDir      string
	SocketPath   string
	LogPath      string
}

type Server struct {
	log    *slog.Logger
	config ServerConfig
	start  StartFunc
	stop   StopFunc
}

func NewServer(logger *slog.Logger, cfg ServerConfig) *Server {
	return NewServerWithHandlers(logger, cfg, nil, nil)
}

func NewServerWithHandlers(logger *slog.Logger, cfg ServerConfig, start StartFunc, stop StopFunc) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{log: logger, config: cfg.normalized()}
	if start != nil {
		s.start = start
	} else {
		s.start = s.startVM
	}
	if stop != nil {
		s.stop = stop
	} else {
		s.stop = s.stopVM
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/vm/start", s.handleStart)
	mux.HandleFunc("/vm/stop", s.handleStop)
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

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s is not allowed", r.Method))
		return
	}
	var req StartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, fmt.Errorf("decode start request: %w", err))
		return
	}
	if err := req.Validate(); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	resp, err := s.start(r.Context(), req)
	if err != nil {
		s.log.Error("start vm", "name", req.Name, "err", err)
		respondError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.log.Error("encode start response", "name", req.Name, "err", err)
	}
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s is not allowed", r.Method))
		return
	}
	var req StopRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, fmt.Errorf("decode stop request: %w", err))
		return
	}
	if err := req.Validate(); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.stop(r.Context(), req); err != nil {
		s.log.Error("stop vm", "name", req.Name, "pid", req.PID, "err", err)
		respondError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) startVM(ctx context.Context, req StartRequest) (StartResponse, error) {
	if err := s.config.Validate(); err != nil {
		return StartResponse{}, err
	}
	paths, err := resolveInstanceRuntimePaths(s.config.InstancesDir, req.Name)
	if err != nil {
		return StartResponse{}, err
	}
	jailerPaths, err := resolveJailerRuntimePaths(s.config.JailerBaseDir, s.config.FirecrackerBinary, req.Name)
	if err != nil {
		return StartResponse{}, err
	}
	if err := validateStartRuntimeFiles(s.config, paths); err != nil {
		return StartResponse{}, err
	}
	if err := os.MkdirAll(s.config.JailerBaseDir, 0o755); err != nil {
		return StartResponse{}, fmt.Errorf("create jailer base dir: %w", err)
	}
	if err := cleanupJailedRuntimePaths(paths, jailerPaths); err != nil {
		return StartResponse{}, err
	}
	serialLog, err := os.OpenFile(paths.SerialLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o660)
	if err != nil {
		return StartResponse{}, fmt.Errorf("open serial log: %w", err)
	}
	defer serialLog.Close()

	cleanupRuntime := true
	defer func() {
		if cleanupRuntime {
			_ = cleanupJailedRuntimePaths(paths, jailerPaths)
		}
	}()

	guestIP, guestNet, err := net.ParseCIDR(req.GuestAddr)
	if err != nil {
		return StartResponse{}, fmt.Errorf("parse guest addr: %w", err)
	}

	rootDriveID := "rootfs"
	isReadOnly := false
	isRootDevice := true
	vcpus := req.VCPUCount
	mem := req.MemoryMiB
	uid := s.config.JailerUID
	gid := s.config.JailerGID
	numaNode := 0

	fcCfg := firecracker.Config{
		SocketPath:      filepath.Base(paths.SocketPath),
		LogPath:         filepath.Base(paths.LogPath),
		KernelImagePath: s.config.KernelPath,
		InitrdPath:      s.config.InitrdPath,
		KernelArgs:      req.KernelArgs,
		Drives: []models.Drive{{
			DriveID:      &rootDriveID,
			PathOnHost:   &paths.RootFSPath,
			IsReadOnly:   &isReadOnly,
			IsRootDevice: &isRootDevice,
		}},
		NetworkInterfaces: firecracker.NetworkInterfaces{{
			StaticConfiguration: &firecracker.StaticNetworkConfiguration{
				MacAddress:  req.GuestMAC,
				HostDevName: req.TapDevice,
				IPConfiguration: &firecracker.IPConfiguration{
					IPAddr:      net.IPNet{IP: guestIP, Mask: guestNet.Mask},
					Gateway:     net.ParseIP(req.GatewayAddr),
					Nameservers: req.Nameservers,
				},
			},
			AllowMMDS: true,
		}},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  &vcpus,
			MemSizeMib: &mem,
		},
		JailerCfg: &firecracker.JailerConfig{
			GID:            &gid,
			UID:            &uid,
			ID:             req.Name,
			NumaNode:       &numaNode,
			ExecFile:       s.config.FirecrackerBinary,
			JailerBinary:   s.config.JailerBinary,
			ChrootBaseDir:  s.config.JailerBaseDir,
			ChrootStrategy: firecracker.NewNaiveChrootStrategy(s.config.KernelPath),
			Stdout:         serialLog,
			Stderr:         serialLog,
		},
		MmdsAddress: net.ParseIP("169.254.169.254"),
	}

	vmCtx := vmContextForRequest(ctx)
	machine, err := firecracker.NewMachine(vmCtx, fcCfg)
	if err != nil {
		return StartResponse{}, fmt.Errorf("create firecracker machine: %w", err)
	}
	machine.Handlers.FcInit = machine.Handlers.FcInit.Swap(prepareJailedRuntimeHandler(paths, jailerPaths, s.config.JailerGID))
	machine.Handlers.FcInit = machine.Handlers.FcInit.Append(firecracker.NewSetMetadataHandler(Metadata{SRV: req.Bootstrap}))

	if err := machine.Start(vmCtx); err != nil {
		return StartResponse{}, fmt.Errorf("start firecracker machine: %w", err)
	}
	pid, err := machine.PID()
	if err != nil {
		return StartResponse{}, fmt.Errorf("read firecracker pid: %w", err)
	}
	if err := assignFirecrackerToCgroup(req.Name, pid); err != nil {
		_ = stopProcess(pid)
		_ = cleanupFirecrackerCgroup(req.Name)
		return StartResponse{}, err
	}
	cleanupRuntime = false
	return StartResponse{PID: pid}, nil
}

func (s *Server) stopVM(_ context.Context, req StopRequest) error {
	var errs []error
	if req.PID > 0 {
		if err := stopProcess(req.PID); err != nil {
			errs = append(errs, err)
		}
	}
	if err := cleanupFirecrackerCgroup(req.Name); err != nil {
		errs = append(errs, err)
	}
	if err := s.cleanupVMRuntime(req.Name); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (r StartRequest) Validate() error {
	if !validName.MatchString(strings.TrimSpace(r.Name)) {
		return fmt.Errorf("invalid instance name %q", r.Name)
	}
	if !ifaceName.MatchString(strings.TrimSpace(r.TapDevice)) {
		return fmt.Errorf("tap device %q is invalid", r.TapDevice)
	}
	if _, err := net.ParseMAC(strings.TrimSpace(r.GuestMAC)); err != nil {
		return fmt.Errorf("guest mac %q is invalid: %w", r.GuestMAC, err)
	}
	if _, _, err := net.ParseCIDR(strings.TrimSpace(r.GuestAddr)); err != nil {
		return fmt.Errorf("guest addr %q is invalid: %w", r.GuestAddr, err)
	}
	if ip := net.ParseIP(strings.TrimSpace(r.GatewayAddr)); ip == nil {
		return fmt.Errorf("gateway addr %q is invalid", r.GatewayAddr)
	}
	for _, ns := range r.Nameservers {
		if ip := net.ParseIP(strings.TrimSpace(ns)); ip == nil {
			return fmt.Errorf("nameserver %q is invalid", ns)
		}
	}
	if r.VCPUCount < 1 {
		return errors.New("vcpu count must be >= 1")
	}
	if r.MemoryMiB < 1 {
		return errors.New("memory MiB must be >= 1")
	}
	if strings.TrimSpace(r.Bootstrap.Hostname) == "" {
		return errors.New("bootstrap hostname is required")
	}
	return nil
}

func (c ServerConfig) normalized() ServerConfig {
	return ServerConfig{
		FirecrackerBinary: strings.TrimSpace(c.FirecrackerBinary),
		JailerBinary:      strings.TrimSpace(c.JailerBinary),
		JailerBaseDir:     strings.TrimSpace(c.JailerBaseDir),
		JailerUID:         c.JailerUID,
		JailerGID:         c.JailerGID,
		InstancesDir:      strings.TrimSpace(c.InstancesDir),
		KernelPath:        strings.TrimSpace(c.KernelPath),
		InitrdPath:        strings.TrimSpace(c.InitrdPath),
	}
}

func (c ServerConfig) Validate() error {
	c = c.normalized()
	for label, path := range map[string]string{
		"firecracker binary path": c.FirecrackerBinary,
		"jailer binary path":      c.JailerBinary,
		"jailer base dir":         c.JailerBaseDir,
		"instances dir":           c.InstancesDir,
		"kernel path":             c.KernelPath,
	} {
		if err := validateAbsolutePath(label, path, false); err != nil {
			return err
		}
	}
	if err := validateAbsolutePath("initrd path", c.InitrdPath, true); err != nil {
		return err
	}
	if c.JailerUID < 0 {
		return fmt.Errorf("jailer uid %d is invalid", c.JailerUID)
	}
	if c.JailerGID < 0 {
		return fmt.Errorf("jailer gid %d is invalid", c.JailerGID)
	}
	return nil
}

func resolveInstanceRuntimePaths(instancesDir, name string) (instanceRuntimePaths, error) {
	instanceDir, err := directChildPath(instancesDir, name)
	if err != nil {
		return instanceRuntimePaths{}, fmt.Errorf("resolve instance directory for %q: %w", name, err)
	}
	return instanceRuntimePaths{
		SocketPath: filepath.Join(instanceDir, "firecracker.sock"),
		LogPath:    filepath.Join(instanceDir, "firecracker.log"),
		SerialLog:  filepath.Join(instanceDir, "serial.log"),
		RootFSPath: filepath.Join(instanceDir, "rootfs.img"),
	}, nil
}

func resolveJailerRuntimePaths(jailerBaseDir, firecrackerBinary, name string) (jailerRuntimePaths, error) {
	execName := strings.TrimSpace(filepath.Base(strings.TrimSpace(firecrackerBinary)))
	if execName == "" || execName == "." || execName == string(filepath.Separator) {
		return jailerRuntimePaths{}, fmt.Errorf("resolve jailer workspace for %q: firecracker binary path %q is invalid", name, firecrackerBinary)
	}
	baseDir, err := directChildPath(jailerBaseDir, execName)
	if err != nil {
		return jailerRuntimePaths{}, fmt.Errorf("resolve jailer workspace base for %q: %w", name, err)
	}
	workspaceDir, err := directChildPath(baseDir, name)
	if err != nil {
		return jailerRuntimePaths{}, fmt.Errorf("resolve jailer workspace for %q: %w", name, err)
	}
	rootDir := filepath.Join(workspaceDir, "root")
	return jailerRuntimePaths{
		WorkspaceDir: workspaceDir,
		RootDir:      rootDir,
		SocketPath:   filepath.Join(rootDir, "firecracker.sock"),
		LogPath:      filepath.Join(rootDir, "firecracker.log"),
	}, nil
}

func validateStartRuntimeFiles(cfg ServerConfig, paths instanceRuntimePaths) error {
	for label, path := range map[string]string{
		"firecracker binary": cfg.FirecrackerBinary,
		"jailer binary":      cfg.JailerBinary,
		"kernel":             cfg.KernelPath,
		"rootfs":             paths.RootFSPath,
	} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("stat %s %s: %w", label, path, err)
		}
	}
	if cfg.InitrdPath != "" {
		if _, err := os.Stat(cfg.InitrdPath); err != nil {
			return fmt.Errorf("stat initrd %s: %w", cfg.InitrdPath, err)
		}
	}
	return nil
}

func prepareJailedRuntimeHandler(hostPaths instanceRuntimePaths, jailerPaths jailerRuntimePaths, jailerGID int) firecracker.Handler {
	return firecracker.Handler{
		Name: firecracker.CreateLogFilesHandlerName,
		Fn: func(_ context.Context, m *firecracker.Machine) error {
			if err := prepareInstanceLogFile(hostPaths.LogPath, jailerGID); err != nil {
				return err
			}
			if err := linkFileIntoJail(hostPaths.LogPath, jailerPaths.LogPath); err != nil {
				return err
			}
			if err := exposeJailedSocket(hostPaths.SocketPath, m.Cfg.SocketPath); err != nil {
				return err
			}
			m.Cfg.LogPath = filepath.Base(jailerPaths.LogPath)
			return nil
		},
	}
}

func prepareInstanceLogFile(path string, gid int) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove stale log symlink %s: %w", path, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat log file %s: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o660)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close log file %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		return fmt.Errorf("chmod log file %s: %w", path, err)
	}
	if err := os.Chown(path, -1, gid); err != nil {
		return fmt.Errorf("chown log file %s: %w", path, err)
	}
	return nil
}

func linkFileIntoJail(srcPath, jailedPath string) error {
	if err := os.Remove(jailedPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale jailed file %s: %w", jailedPath, err)
	}
	if err := os.Link(srcPath, jailedPath); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("link %s into jail at %s: %w (keep SRV_JAILER_BASE_DIR on the same filesystem as SRV_DATA_DIR)", srcPath, jailedPath, err)
		}
		return fmt.Errorf("link %s into jail at %s: %w", srcPath, jailedPath, err)
	}
	return nil
}

func exposeJailedSocket(hostSocketPath, jailedSocketPath string) error {
	if err := os.Remove(hostSocketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale firecracker socket alias %s: %w", hostSocketPath, err)
	}
	if err := os.Symlink(jailedSocketPath, hostSocketPath); err != nil {
		return fmt.Errorf("link firecracker socket %s -> %s: %w", hostSocketPath, jailedSocketPath, err)
	}
	return nil
}

func cleanupJailedRuntimePaths(hostPaths instanceRuntimePaths, jailerPaths jailerRuntimePaths) error {
	var errs []error
	if err := os.Remove(hostPaths.SocketPath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove firecracker socket alias %s: %w", hostPaths.SocketPath, err))
	}
	if err := os.RemoveAll(jailerPaths.WorkspaceDir); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove jailer workspace %s: %w", jailerPaths.WorkspaceDir, err))
	}
	return errors.Join(errs...)
}

func (s *Server) cleanupVMRuntime(name string) error {
	hostPaths, err := resolveInstanceRuntimePaths(s.config.InstancesDir, name)
	if err != nil {
		return err
	}
	jailerPaths, err := resolveJailerRuntimePaths(s.config.JailerBaseDir, s.config.FirecrackerBinary, name)
	if err != nil {
		return err
	}
	return cleanupJailedRuntimePaths(hostPaths, jailerPaths)
}

func (r StopRequest) Validate() error {
	if !validName.MatchString(strings.TrimSpace(r.Name)) {
		return fmt.Errorf("invalid instance name %q", r.Name)
	}
	if r.PID < 0 {
		return fmt.Errorf("pid %d is invalid", r.PID)
	}
	return nil
}

func validateAbsolutePath(label, path string, allowEmpty bool) error {
	path = strings.TrimSpace(path)
	if path == "" {
		if allowEmpty {
			return nil
		}
		return fmt.Errorf("%s is required", label)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s %q must be absolute", label, path)
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
		if gid != os.Getegid() {
			if err := os.Chown(socketPath, os.Geteuid(), gid); err != nil {
				listener.Close()
				return nil, fmt.Errorf("chown helper socket %s: %w", socketPath, err)
			}
		}
		mode = 0o660
	}
	if err := os.Chmod(socketPath, mode); err != nil {
		listener.Close()
		return nil, fmt.Errorf("chmod helper socket %s: %w", socketPath, err)
	}
	return listener, nil
}

func respondError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: err.Error()})
}

func stopProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal firecracker pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill firecracker pid %d: %w", pid, err)
	}
	return nil
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil
}

func firecrackerCgroupPath(name string) (string, error) {
	cgroupPath, err := currentCgroupPath()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(cgroupPath) {
		return "", fmt.Errorf("current cgroup path %q is not absolute", cgroupPath)
	}
	vmRoot := filepath.Join(cgroupFSRoot, strings.TrimPrefix(cgroupPath, "/"), "firecracker-vms")
	child, err := directChildPath(vmRoot, name)
	if err != nil {
		return "", fmt.Errorf("resolve firecracker cgroup for %q: %w", name, err)
	}
	return child, nil
}

func assignFirecrackerToCgroup(name string, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("assign firecracker cgroup for %q: invalid pid %d", name, pid)
	}
	cgroupPath, err := firecrackerCgroupPath(name)
	if err != nil {
		return fmt.Errorf("assign firecracker cgroup for %q: %w", name, err)
	}
	if err := os.MkdirAll(cgroupPath, 0o755); err != nil {
		return fmt.Errorf("assign firecracker cgroup for %q: create %s: %w", name, cgroupPath, err)
	}
	if err := os.WriteFile(filepath.Join(cgroupPath, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Errorf("assign firecracker cgroup for %q: move pid %d: %w", name, pid, err)
	}
	return nil
}

func cleanupFirecrackerCgroup(name string) error {
	cgroupPath, err := firecrackerCgroupPath(name)
	if err != nil {
		return fmt.Errorf("cleanup firecracker cgroup for %q: %w", name, err)
	}
	if err := os.Remove(cgroupPath); err != nil && !os.IsNotExist(err) {
		if errors.Is(err, syscall.ENOTEMPTY) {
			_ = os.Remove(filepath.Join(cgroupPath, "cgroup.procs"))
			err = os.Remove(cgroupPath)
		}
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("cleanup firecracker cgroup for %q: remove %s: %w", name, cgroupPath, err)
		}
	}
	vmRoot := filepath.Dir(cgroupPath)
	if err := os.Remove(vmRoot); err != nil && !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTEMPTY) {
		return fmt.Errorf("cleanup firecracker cgroup root %s: %w", vmRoot, err)
	}
	return nil
}

func readUnifiedCgroupPath(path string) (string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(payload), "\n") {
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		cgroupPath := strings.TrimSpace(strings.TrimPrefix(line, "0::"))
		if cgroupPath == "" {
			return "/", nil
		}
		if !filepath.IsAbs(cgroupPath) {
			return "", fmt.Errorf("unified cgroup path %q is not absolute", cgroupPath)
		}
		return cgroupPath, nil
	}
	return "", fmt.Errorf("could not find a unified cgroup entry in %s", path)
}

func directChildPath(base, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name is empty")
	}
	if name == "." || name == ".." || filepath.Base(name) != name {
		return "", fmt.Errorf("name %q must be a single path segment", name)
	}
	return filepath.Join(filepath.Clean(base), name), nil
}
