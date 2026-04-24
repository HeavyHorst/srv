package vmrunner

import (
	"bytes"
	"context"
	"debug/elf"
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
	"sync"
	"syscall"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"

	"srv/internal/model"
)

const (
	DefaultSocketPath = "/run/srv-vm-runner/vm-runner.sock"
	// firecracker-go-sdk requires NumaNode to be set, but a negative value keeps
	// the SDK from synthesizing cpuset cgroup arguments for every jailed launch.
	defaultCgroupCPUQuotaPeriodMicros  = int64(100000)
	defaultVMPIDsMax                   = int64(512)
	firecrackerSupervisorCgroupName    = "supervisor"
	firecrackerVMRootCgroupName        = "firecracker-vms"
	firecrackerPoolRootCgroupName      = "firecracker-pools"
	pooledBalloonStatsIntervalSeconds  = int64(5)
	pooledBalloonReconcileInterval     = 5 * time.Second
	pooledBalloonRequestTimeout        = 2 * time.Second
	pooledBalloonTargetStepMiB         = int64(64)
	pooledBalloonGuestHeadroomFloorMiB = int64(256)
	pooledBalloonGuestHeadroomDivisor  = int64(8)
	pooledBalloonMaxTargetDivisor      = int64(2)
	miBBytes                           = int64(1024 * 1024)
	disabledJailerNumaNode             = -1
	gracefulStopRequestTimeout         = 2 * time.Second
	gracefulStopTimeout                = 10 * time.Second
	forcedStopTimeout                  = 10 * time.Second
	postKillWaitTimeout                = 2 * time.Second
)

var (
	validName           = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	ifaceName           = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,15}$`)
	vmContextForRequest = func(context.Context) context.Context {
		return context.Background()
	}
	currentCgroupPath = func() (string, error) {
		return readUnifiedCgroupPath("/proc/self/cgroup")
	}
	cgroupFSRoot         = "/sys/fs/cgroup"
	createDirAll         = os.MkdirAll
	readTextFile         = os.ReadFile
	removePath           = os.Remove
	writeTextFile        = os.WriteFile
	requestGuestShutdown = func(ctx context.Context, socketPath string) error {
		action := models.InstanceActionInfoActionTypeSendCtrlAltDel
		_, err := firecracker.NewClient(socketPath, nil, false).CreateSyncAction(ctx, &models.InstanceActionInfo{ActionType: &action})
		if err != nil {
			return fmt.Errorf("request guest shutdown via %s: %w", socketPath, err)
		}
		return nil
	}
	waitForProcessExit = waitForProcessExitByPolling
	forceStopProcess   = func(pid int) error { return stopProcessWithGrace(pid, forcedStopTimeout) }
	killProcessNow     = func(pid int) error { return stopProcessWithGrace(pid, 0) }
)

var errProcessExitTimeout = errors.New("process did not exit before timeout")

type Bootstrap struct {
	Version             int      `json:"version"`
	Hostname            string   `json:"hostname"`
	TailscaleAuthKey    string   `json:"tailscale_auth_key,omitempty"`
	TailscaleControlURL string   `json:"tailscale_control_url,omitempty"`
	TailscaleTags       []string `json:"tailscale_tags,omitempty"`
	ZenGatewayPort      int      `json:"zen_gateway_port,omitempty"`
}

type Metadata struct {
	SRV Bootstrap `json:"srv"`
}

type StartRequest struct {
	Name                string    `json:"name"`
	TapDevice           string    `json:"tap_device"`
	GuestMAC            string    `json:"guest_mac"`
	GuestAddr           string    `json:"guest_addr"`
	GatewayAddr         string    `json:"gateway_addr"`
	Nameservers         []string  `json:"nameservers,omitempty"`
	VCPUCount           int64     `json:"vcpu_count"`
	MemoryMiB           int64     `json:"memory_mib"`
	MemoryMode          string    `json:"memory_mode,omitempty"`
	MemoryPoolName      string    `json:"memory_pool_name,omitempty"`
	MemoryPoolSizeBytes int64     `json:"memory_pool_size_bytes,omitempty"`
	KernelArgs          string    `json:"kernel_args,omitempty"`
	Bootstrap           Bootstrap `json:"bootstrap"`
}

type StartResponse struct {
	PID int `json:"pid"`
}

type StopRequest struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`
}

type MetricsRequest struct {
	Name string `json:"name"`
}

type MemoryPoolRequest struct {
	Name string `json:"name"`
}

type MetricsResponse struct {
	CPUUsageUsec       uint64 `json:"cpu_usage_usec"`
	MemoryCurrentBytes int64  `json:"memory_current_bytes"`
	MemoryLimitBytes   int64  `json:"memory_limit_bytes"`
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

func (c *Client) ReadInstanceMetrics(ctx context.Context, req MetricsRequest) (MetricsResponse, error) {
	if err := req.Validate(); err != nil {
		return MetricsResponse{}, err
	}
	var resp MetricsResponse
	if err := c.post(ctx, "/vm/metrics", req, &resp); err != nil {
		return MetricsResponse{}, err
	}
	return resp, nil
}

func (c *Client) DeleteMemoryPool(ctx context.Context, req MemoryPoolRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	return c.post(ctx, "/pool/delete", req, nil)
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
type MetricsFunc func(context.Context, MetricsRequest) (MetricsResponse, error)
type DeleteMemoryPoolFunc func(context.Context, MemoryPoolRequest) error

type ServerConfig struct {
	FirecrackerBinary string
	JailerBinary      string
	JailerBaseDir     string
	JailerUID         int
	JailerGID         int
	InstancesDir      string
	KernelPath        string
	InitrdPath        string
	VMPIDsMax         int64
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

type pooledBalloonVM struct {
	Name       string
	CgroupPath string
	SocketPath string
}

type firecrackerBalloonStats struct {
	ActualMiB       *int64 `json:"actual_mib"`
	AvailableMemory int64  `json:"available_memory,omitempty"`
	DiskCaches      int64  `json:"disk_caches,omitempty"`
	FreeMemory      int64  `json:"free_memory,omitempty"`
	TargetMiB       *int64 `json:"target_mib"`
	TotalMemory     int64  `json:"total_memory,omitempty"`
}

type firecrackerSocketClient struct {
	httpClient *http.Client
	transport  *http.Transport
}

type Server struct {
	log              *slog.Logger
	config           ServerConfig
	start            StartFunc
	stop             StopFunc
	metrics          MetricsFunc
	deleteMemoryPool DeleteMemoryPoolFunc

	cgroupMu           sync.Mutex
	delegatedCgroupRel string
	firecrackerClients sync.Map
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
	s.metrics = s.readInstanceMetrics
	s.deleteMemoryPool = s.deleteMemoryPoolCgroup
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/vm/start", s.handleStart)
	mux.HandleFunc("/vm/stop", s.handleStop)
	mux.HandleFunc("/vm/metrics", s.handleMetrics)
	mux.HandleFunc("/pool/delete", s.handleDeleteMemoryPool)
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
	go s.runPooledBalloonLoop(ctx)

	err = server.Serve(listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) runPooledBalloonLoop(ctx context.Context) {
	s.reconcilePooledBalloons(ctx)
	ticker := time.NewTicker(pooledBalloonReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcilePooledBalloons(ctx)
		}
	}
}

func (s *Server) reconcilePooledBalloons(ctx context.Context) {
	rootRel, err := s.delegatedCgroupRoot()
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("resolve delegated cgroup root for pooled balloon reconcile", "err", err)
		}
		return
	}
	vms, err := s.listPooledBalloonVMs(rootRel)
	if err != nil {
		if ctx.Err() == nil && !os.IsNotExist(err) {
			s.log.Warn("list pooled vms for balloon reconcile", "err", err)
		}
		return
	}
	var wg sync.WaitGroup
	for _, vm := range vms {
		vm := vm
		wg.Add(1)
		go func() {
			defer wg.Done()
			reqCtx, cancel := context.WithTimeout(ctx, pooledBalloonRequestTimeout)
			defer cancel()
			err := s.reconcilePooledBalloon(reqCtx, vm)
			if err == nil || errors.Is(err, os.ErrNotExist) || ctx.Err() != nil {
				return
			}
			s.log.Warn("reconcile pooled balloon", "name", vm.Name, "err", err)
		}()
	}
	wg.Wait()
}

func (s *Server) listPooledBalloonVMs(rootRel string) ([]pooledBalloonVM, error) {
	poolRoot := filepath.Join(cgroupPathOnHost(rootRel), firecrackerPoolRootCgroupName)
	poolEntries, err := os.ReadDir(poolRoot)
	if err != nil {
		return nil, err
	}
	vms := make([]pooledBalloonVM, 0)
	for _, poolEntry := range poolEntries {
		if !poolEntry.IsDir() {
			continue
		}
		poolPath := filepath.Join(poolRoot, poolEntry.Name())
		vmEntries, err := os.ReadDir(poolPath)
		if err != nil {
			return nil, fmt.Errorf("list pooled vm cgroups under %s: %w", poolPath, err)
		}
		for _, vmEntry := range vmEntries {
			if !vmEntry.IsDir() {
				continue
			}
			paths, err := resolveInstanceRuntimePaths(s.config.InstancesDir, vmEntry.Name())
			if err != nil {
				return nil, err
			}
			vms = append(vms, pooledBalloonVM{
				Name:       vmEntry.Name(),
				CgroupPath: filepath.Join(poolPath, vmEntry.Name()),
				SocketPath: paths.SocketPath,
			})
		}
	}
	return vms, nil
}

func (s *Server) reconcilePooledBalloon(ctx context.Context, vm pooledBalloonVM) error {
	residentBytes, err := readInt64File(filepath.Join(vm.CgroupPath, "memory.current"))
	if err != nil {
		return err
	}
	stats, err := s.readPooledBalloonStats(ctx, vm.SocketPath)
	if err != nil {
		return err
	}
	desiredTargetMiB, _, ok := desiredPooledBalloonTargetMiB(residentBytes, stats)
	if !ok {
		return nil
	}
	currentTargetMiB := *stats.TargetMiB
	if desiredTargetMiB == currentTargetMiB {
		return nil
	}
	if err := s.patchPooledBalloonTarget(ctx, vm.SocketPath, desiredTargetMiB); err != nil {
		return err
	}
	s.log.Debug(
		"updated pooled balloon target",
		"name", vm.Name,
		"target_mib", desiredTargetMiB,
		"previous_target_mib", currentTargetMiB,
		"resident_bytes", residentBytes,
		"available_memory_bytes", stats.AvailableMemory,
		"disk_caches_bytes", stats.DiskCaches,
	)
	return nil
}

func desiredPooledBalloonTargetMiB(residentBytes int64, stats firecrackerBalloonStats) (int64, int64, bool) {
	if stats.TargetMiB == nil || stats.TotalMemory < 1 {
		return 0, 0, false
	}
	guestTotalMiB := bytesToMiBCeil(stats.TotalMemory)
	if guestTotalMiB < 1 {
		return 0, 0, false
	}
	headroomMiB := maxInt64(pooledBalloonGuestHeadroomFloorMiB, guestTotalMiB/pooledBalloonGuestHeadroomDivisor)
	availableBytes := stats.AvailableMemory
	if availableBytes <= 0 {
		availableBytes = stats.FreeMemory + stats.DiskCaches
	}
	if availableBytes < 0 {
		availableBytes = 0
	}
	reclaimableGuestMiB := maxInt64(bytesToMiBFloor(availableBytes)-headroomMiB, 0)
	reclaimableResidentMiB := maxInt64(bytesToMiBFloor(residentBytes)-headroomMiB, 0)
	maxTargetMiB := maxInt64(guestTotalMiB/pooledBalloonMaxTargetDivisor, 1)
	stepMiB := pooledBalloonAdjustmentStepMiB(guestTotalMiB)
	desiredTargetMiB := minInt64(reclaimableGuestMiB, reclaimableResidentMiB)
	desiredTargetMiB = minInt64(desiredTargetMiB, maxTargetMiB)
	desiredTargetMiB = roundDownToMultiple(desiredTargetMiB, stepMiB)
	return desiredTargetMiB, stepMiB, true
}

func pooledBalloonAdjustmentStepMiB(guestTotalMiB int64) int64 {
	return maxInt64(1, minInt64(pooledBalloonTargetStepMiB, maxInt64(guestTotalMiB/8, 1)))
}

func bytesToMiBFloor(bytes int64) int64 {
	if bytes <= 0 {
		return 0
	}
	return bytes / miBBytes
}

func bytesToMiBCeil(bytes int64) int64 {
	if bytes <= 0 {
		return 0
	}
	return (bytes + miBBytes - 1) / miBBytes
}

func roundDownToMultiple(value, multiple int64) int64 {
	if value <= 0 || multiple <= 1 {
		return maxInt64(value, 0)
	}
	return value - (value % multiple)
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
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

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s is not allowed", r.Method))
		return
	}
	var req MetricsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, fmt.Errorf("decode metrics request: %w", err))
		return
	}
	if err := req.Validate(); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	resp, err := s.metrics(r.Context(), req)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			respondError(w, http.StatusNotFound, err)
			return
		}
		s.log.Error("read vm metrics", "name", req.Name, "err", err)
		respondError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.log.Error("encode metrics response", "name", req.Name, "err", err)
	}
}

func (s *Server) handleDeleteMemoryPool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s is not allowed", r.Method))
		return
	}
	var req MemoryPoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, fmt.Errorf("decode memory pool request: %w", err))
		return
	}
	if err := req.Validate(); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.deleteMemoryPool(r.Context(), req); err != nil {
		s.log.Error("delete memory pool cgroup", "name", req.Name, "err", err)
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
			s.closeFirecrackerClient(paths.SocketPath)
			_ = cleanupJailedRuntimePaths(paths, jailerPaths)
		}
	}()

	guestIP, guestNet, err := net.ParseCIDR(req.GuestAddr)
	if err != nil {
		return StartResponse{}, fmt.Errorf("parse guest addr: %w", err)
	}

	vcpus := req.VCPUCount
	mem := req.MemoryMiB
	uid := s.config.JailerUID
	gid := s.config.JailerGID
	numaNode := disabledJailerNumaNode

	fcCfg := firecracker.Config{
		SocketPath:      filepath.Base(paths.SocketPath),
		LogPath:         filepath.Base(paths.LogPath),
		KernelImagePath: s.config.KernelPath,
		InitrdPath:      s.config.InitrdPath,
		KernelArgs:      req.KernelArgs,
		Drives:          []models.Drive{newRootDrive(paths.RootFSPath)},
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
			CgroupVersion:  detectJailerCgroupVersion(),
			Stdout:         serialLog,
			Stderr:         serialLog,
		},
		MmdsAddress: net.ParseIP("169.254.169.254"),
	}

	vmCtx := vmContextForRequest(ctx)
	processRunner, err := s.processRunnerForStart(vmCtx, req, fcCfg.SocketPath, serialLog)
	if err != nil {
		return StartResponse{}, err
	}
	machine, err := firecracker.NewMachine(vmCtx, fcCfg, firecracker.WithProcessRunner(processRunner))
	if err != nil {
		return StartResponse{}, fmt.Errorf("create firecracker machine: %w", err)
	}
	machine.Handlers.FcInit = machine.Handlers.FcInit.Swap(prepareJailedRuntimeHandler(paths, jailerPaths, s.config.JailerGID))
	machine.Handlers.FcInit = machine.Handlers.FcInit.Append(firecracker.NewSetMetadataHandler(Metadata{SRV: req.Bootstrap}))
	if model.NormalizeMemoryMode(req.MemoryMode) == model.MemoryModePool {
		machine.Handlers.FcInit = machine.Handlers.FcInit.Append(firecracker.Handler{
			Name: "srv.CreateBalloonDevice",
			Fn: func(ctx context.Context, _ *firecracker.Machine) error {
				if err := s.createPooledBalloonDevice(ctx, paths.SocketPath); err != nil {
					return fmt.Errorf("create balloon device: %w", err)
				}
				return nil
			},
		})
	}

	if err := machine.Start(vmCtx); err != nil {
		return StartResponse{}, fmt.Errorf("start firecracker machine: %w", err)
	}
	pid, err := machine.PID()
	if err != nil {
		return StartResponse{}, fmt.Errorf("read firecracker pid: %w", err)
	}
	cleanupRuntime = false
	return StartResponse{PID: pid}, nil
}

func newRootDrive(path string) models.Drive {
	rootDriveID := "rootfs"
	cacheType := models.DriveCacheTypeWriteback
	isReadOnly := false
	isRootDevice := true

	return models.Drive{
		CacheType:    &cacheType,
		DriveID:      &rootDriveID,
		PathOnHost:   &path,
		IsReadOnly:   &isReadOnly,
		IsRootDevice: &isRootDevice,
	}
}

func (s *Server) createPooledBalloonDevice(ctx context.Context, socketPath string) error {
	amountMiB := int64(0)
	deflateOnOOM := true
	return s.doFirecrackerAPIRequest(ctx, socketPath, http.MethodPut, "/balloon", struct {
		AmountMiB                   *int64 `json:"amount_mib"`
		DeflateOnOOM                *bool  `json:"deflate_on_oom"`
		StatsPollingIntervalSeconds int64  `json:"stats_polling_interval_s,omitempty"`
		FreePageReporting           bool   `json:"free_page_reporting"`
	}{
		AmountMiB:                   &amountMiB,
		DeflateOnOOM:                &deflateOnOOM,
		StatsPollingIntervalSeconds: pooledBalloonStatsIntervalSeconds,
		FreePageReporting:           true,
	}, http.StatusNoContent, nil)
}

func (s *Server) readPooledBalloonStats(ctx context.Context, socketPath string) (firecrackerBalloonStats, error) {
	var stats firecrackerBalloonStats
	if err := s.doFirecrackerAPIRequest(ctx, socketPath, http.MethodGet, "/balloon/statistics", nil, http.StatusOK, &stats); err != nil {
		return firecrackerBalloonStats{}, err
	}
	return stats, nil
}

func (s *Server) patchPooledBalloonTarget(ctx context.Context, socketPath string, amountMiB int64) error {
	return s.doFirecrackerAPIRequest(ctx, socketPath, http.MethodPatch, "/balloon", struct {
		AmountMiB *int64 `json:"amount_mib"`
	}{AmountMiB: &amountMiB}, http.StatusNoContent, nil)
}

func (s *Server) firecrackerClientForSocket(socketPath string) *firecrackerSocketClient {
	if cached, ok := s.firecrackerClients.Load(socketPath); ok {
		return cached.(*firecrackerSocketClient)
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	client := &firecrackerSocketClient{
		httpClient: &http.Client{Transport: transport},
		transport:  transport,
	}
	actual, loaded := s.firecrackerClients.LoadOrStore(socketPath, client)
	if loaded {
		transport.CloseIdleConnections()
		return actual.(*firecrackerSocketClient)
	}
	return client
}

func (s *Server) closeFirecrackerClient(socketPath string) {
	client, ok := s.firecrackerClients.LoadAndDelete(socketPath)
	if !ok {
		return
	}
	client.(*firecrackerSocketClient).transport.CloseIdleConnections()
}

func (s *Server) doFirecrackerAPIRequest(ctx context.Context, socketPath, method, path string, payload any, successStatus int, out any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal firecracker request payload: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	client := s.firecrackerClientForSocket(socketPath)

	req, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+path, body)
	if err != nil {
		return fmt.Errorf("build firecracker request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call firecracker %s %s on %s: %w", method, path, socketPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == successStatus {
		if out == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode firecracker response for %s %s: %w", method, path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	text, _ := io.ReadAll(resp.Body)
	var fcErr struct {
		FaultMessage string `json:"fault_message"`
	}
	if err := json.Unmarshal(text, &fcErr); err == nil && strings.TrimSpace(fcErr.FaultMessage) != "" {
		return fmt.Errorf("firecracker %s %s on %s: %s", method, path, socketPath, fcErr.FaultMessage)
	}
	message := strings.TrimSpace(string(text))
	if message == "" {
		message = resp.Status
	}
	return fmt.Errorf("firecracker %s %s on %s: %s", method, path, socketPath, message)
}

func (s *Server) stopVM(ctx context.Context, req StopRequest) error {
	var errs []error
	if req.PID > 0 {
		stoppedGracefully, err := s.tryGracefulStop(ctx, req)
		if err != nil {
			s.log.Warn("graceful guest shutdown failed; falling back to forced stop", "name", req.Name, "pid", req.PID, "err", err)
		}
		if !stoppedGracefully {
			stop := forceStopProcess
			if errors.Is(err, errProcessExitTimeout) {
				stop = killProcessNow
			}
			if err := stop(req.PID); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := s.cleanupFirecrackerCgroup(req.Name); err != nil {
		errs = append(errs, err)
	}
	if err := s.cleanupVMRuntime(req.Name); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *Server) tryGracefulStop(ctx context.Context, req StopRequest) (bool, error) {
	if req.PID <= 0 || !processExists(req.PID) {
		return true, nil
	}
	paths, err := resolveInstanceRuntimePaths(s.config.InstancesDir, req.Name)
	if err != nil {
		return false, err
	}
	stopCtx, cancel := context.WithTimeout(vmContextForRequest(ctx), gracefulStopRequestTimeout)
	defer cancel()
	if err := requestGuestShutdown(stopCtx, paths.SocketPath); err != nil {
		return false, err
	}
	if err := waitForProcessExit(req.PID, gracefulStopTimeout); err != nil {
		return false, err
	}
	return true, nil
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
	if mode := model.NormalizeMemoryMode(r.MemoryMode); mode == model.MemoryModePool {
		if !validName.MatchString(strings.TrimSpace(r.MemoryPoolName)) {
			return fmt.Errorf("memory pool name %q is invalid", r.MemoryPoolName)
		}
		if r.MemoryPoolSizeBytes < 1 {
			return errors.New("memory pool size bytes must be >= 1")
		}
	}
	if strings.TrimSpace(r.Bootstrap.Hostname) == "" {
		return errors.New("bootstrap hostname is required")
	}
	return nil
}

func (r MetricsRequest) Validate() error {
	if !validName.MatchString(strings.TrimSpace(r.Name)) {
		return fmt.Errorf("invalid instance name %q", r.Name)
	}
	return nil
}

func (r MemoryPoolRequest) Validate() error {
	if !validName.MatchString(strings.TrimSpace(r.Name)) {
		return fmt.Errorf("invalid memory pool name %q", r.Name)
	}
	return nil
}

func (s *Server) processRunnerForStart(ctx context.Context, req StartRequest, apiSocketPath string, serialLog io.Writer) (*exec.Cmd, error) {
	parentCgroup, err := s.prepareCgroupParentForStart(req)
	if err != nil {
		return nil, err
	}
	return s.buildJailedVMCommand(ctx, req, apiSocketPath, parentCgroup, serialLog)
}

func (s *Server) buildJailedVMCommand(ctx context.Context, req StartRequest, apiSocketPath, parentCgroup string, serialLog io.Writer) (*exec.Cmd, error) {
	if strings.TrimSpace(parentCgroup) == "" {
		return nil, errors.New("parent cgroup is required")
	}
	fcArgs := []string{"--no-seccomp", "--api-sock", apiSocketPath}
	builder := firecracker.NewJailerCommandBuilder().
		WithID(req.Name).
		WithUID(s.config.JailerUID).
		WithGID(s.config.JailerGID).
		WithNumaNode(disabledJailerNumaNode).
		WithExecFile(s.config.FirecrackerBinary).
		WithChrootBaseDir(s.config.JailerBaseDir).
		WithCgroupVersion("2").
		WithFirecrackerArgs(fcArgs...).
		WithStdout(serialLog).
		WithStderr(serialLog)
	if s.config.JailerBinary != "" {
		builder = builder.WithBin(s.config.JailerBinary)
	}
	args := builder.Args()
	extra := []string{"--parent-cgroup", parentCgroup}
	for _, setting := range jailerCgroupSettings(req, s.config.VMPIDsMax) {
		extra = append(extra, "--cgroup", setting)
	}
	args = insertBeforeDoubleDash(args, extra...)
	cmd := exec.CommandContext(ctx, builder.Bin(), args...)
	cmd.Stdout = serialLog
	cmd.Stderr = serialLog
	return cmd, nil
}

func jailerCgroupSettings(req StartRequest, pidsMax int64) []string {
	settings := []string{
		fmt.Sprintf("cpu.max=%d %d", req.VCPUCount*defaultCgroupCPUQuotaPeriodMicros, defaultCgroupCPUQuotaPeriodMicros),
	}
	if model.NormalizeMemoryMode(req.MemoryMode) == model.MemoryModeFixed {
		settings = append(settings,
			fmt.Sprintf("memory.max=%d", req.MemoryMiB*miBBytes),
			"memory.swap.max=0",
		)
	}
	settings = append(settings, fmt.Sprintf("pids.max=%d", pidsMax))
	return settings
}

func insertBeforeDoubleDash(args []string, insert ...string) []string {
	idx := len(args)
	for i, arg := range args {
		if arg == "--" {
			idx = i
			break
		}
	}
	withInsert := append([]string{}, args[:idx]...)
	withInsert = append(withInsert, insert...)
	withInsert = append(withInsert, args[idx:]...)
	return withInsert
}

func (c ServerConfig) normalized() ServerConfig {
	pidsMax := c.VMPIDsMax
	if pidsMax == 0 {
		pidsMax = defaultVMPIDsMax
	}
	return ServerConfig{
		FirecrackerBinary: strings.TrimSpace(c.FirecrackerBinary),
		JailerBinary:      strings.TrimSpace(c.JailerBinary),
		JailerBaseDir:     strings.TrimSpace(c.JailerBaseDir),
		JailerUID:         c.JailerUID,
		JailerGID:         c.JailerGID,
		InstancesDir:      strings.TrimSpace(c.InstancesDir),
		KernelPath:        strings.TrimSpace(c.KernelPath),
		InitrdPath:        strings.TrimSpace(c.InitrdPath),
		VMPIDsMax:         pidsMax,
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
	if c.VMPIDsMax < 1 {
		return fmt.Errorf("vm pids max %d is invalid", c.VMPIDsMax)
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
	if err := validateJailedFirecrackerBinary(cfg.FirecrackerBinary); err != nil {
		return err
	}
	if cfg.InitrdPath != "" {
		if _, err := os.Stat(cfg.InitrdPath); err != nil {
			return fmt.Errorf("stat initrd %s: %w", cfg.InitrdPath, err)
		}
	}
	return nil
}

func validateJailedFirecrackerBinary(path string) error {
	interp, err := elfInterpreter(path)
	if err != nil {
		return fmt.Errorf("inspect firecracker binary %s: %w", path, err)
	}
	if interp == "" {
		return nil
	}
	return fmt.Errorf("firecracker binary %s is dynamically linked via %s; Firecracker jailer requires a statically linked firecracker binary (default musl build)", path, interp)
}

func elfInterpreter(path string) (string, error) {
	binary, err := elf.Open(path)
	if err != nil {
		return "", err
	}
	defer binary.Close()

	for _, prog := range binary.Progs {
		if prog.Type != elf.PT_INTERP {
			continue
		}
		payload, err := io.ReadAll(prog.Open())
		if err != nil {
			return "", fmt.Errorf("read PT_INTERP: %w", err)
		}
		return strings.TrimRight(string(payload), "\x00\n"), nil
	}

	return "", nil
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
	s.closeFirecrackerClient(hostPaths.SocketPath)
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

func stopProcessWithGrace(pid int, timeout time.Duration) error {
	if pid <= 0 {
		return nil
	}
	if timeout > 0 {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("signal firecracker pid %d: %w", pid, err)
		}
		if err := waitForProcessExit(pid, timeout); err == nil {
			return nil
		}
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill firecracker pid %d: %w", pid, err)
	}
	if err := waitForProcessExit(pid, postKillWaitTimeout); err != nil {
		return err
	}
	return nil
}

func waitForProcessExitByPolling(pid int, timeout time.Duration) error {
	if pid <= 0 || !processExists(pid) {
		return nil
	}
	if timeout <= 0 {
		return fmt.Errorf("wait for firecracker pid %d to exit: %w", pid, errProcessExitTimeout)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !processExists(pid) {
		return nil
	}
	return fmt.Errorf("wait for firecracker pid %d to exit after %s: %w", pid, timeout, errProcessExitTimeout)
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil
}

func detectJailerCgroupVersion() string {
	if _, err := currentCgroupPath(); err == nil {
		return "2"
	}
	return "1"
}

func (s *Server) prepareCgroupParentForStart(req StartRequest) (string, error) {
	if model.NormalizeMemoryMode(req.MemoryMode) != model.MemoryModePool {
		return s.prepareFirecrackerCgroupParent()
	}
	return s.prepareMemoryPoolCgroupParent(req.MemoryPoolName, req.MemoryPoolSizeBytes)
}

func (s *Server) prepareFirecrackerCgroupParent() (string, error) {
	rootRel, rootPath, err := s.prepareFirecrackerCgroupRoots()
	if err != nil {
		return "", err
	}
	vmRootPath := filepath.Join(rootPath, firecrackerVMRootCgroupName)
	if err := createDirAll(vmRootPath, 0o755); err != nil {
		return "", fmt.Errorf("create firecracker cgroup root %s: %w", vmRootPath, err)
	}
	if err := enableCgroupControllers(vmRootPath, "cpu", "memory", "pids"); err != nil {
		return "", err
	}
	return strings.TrimPrefix(filepath.Join(rootRel, firecrackerVMRootCgroupName), "/"), nil
}

func (s *Server) prepareMemoryPoolCgroupParent(poolName string, poolSizeBytes int64) (string, error) {
	if !validName.MatchString(strings.TrimSpace(poolName)) {
		return "", fmt.Errorf("invalid memory pool name %q", poolName)
	}
	if poolSizeBytes < 1 {
		return "", fmt.Errorf("memory pool size bytes %d is invalid", poolSizeBytes)
	}
	rootRel, rootPath, err := s.prepareFirecrackerCgroupRoots()
	if err != nil {
		return "", err
	}
	poolRootPath := filepath.Join(rootPath, firecrackerPoolRootCgroupName)
	poolPath, err := directChildPath(poolRootPath, poolName)
	if err != nil {
		return "", fmt.Errorf("resolve firecracker memory pool cgroup for %q: %w", poolName, err)
	}
	if err := createDirAll(poolPath, 0o755); err != nil {
		return "", fmt.Errorf("create firecracker memory pool cgroup %s: %w", poolPath, err)
	}
	if err := enableCgroupControllers(poolPath, "cpu", "memory", "pids"); err != nil {
		return "", err
	}
	if err := writeCgroupValue(poolPath, "memory.max", strconv.FormatInt(poolSizeBytes, 10)); err != nil {
		return "", err
	}
	if err := writeCgroupValue(poolPath, "memory.swap.max", "0"); err != nil {
		return "", err
	}
	return strings.TrimPrefix(filepath.Join(rootRel, firecrackerPoolRootCgroupName, poolName), "/"), nil
}

func (s *Server) prepareFirecrackerCgroupRoots() (string, string, error) {
	rootRel, err := s.delegatedCgroupRoot()
	if err != nil {
		return "", "", err
	}
	rootPath := cgroupPathOnHost(rootRel)
	supervisorPath := filepath.Join(rootPath, firecrackerSupervisorCgroupName)
	poolRootPath := filepath.Join(rootPath, firecrackerPoolRootCgroupName)
	if err := createDirAll(supervisorPath, 0o755); err != nil {
		return "", "", fmt.Errorf("create cgroup supervisor %s: %w", supervisorPath, err)
	}
	if err := createDirAll(poolRootPath, 0o755); err != nil {
		return "", "", fmt.Errorf("create firecracker pool root %s: %w", poolRootPath, err)
	}
	currentRel, err := currentCgroupPath()
	if err != nil {
		return "", "", fmt.Errorf("read current cgroup path: %w", err)
	}
	supervisorRel := filepath.Join(rootRel, firecrackerSupervisorCgroupName)
	if currentRel != supervisorRel {
		if err := moveProcessToCgroup(os.Getpid(), supervisorPath); err != nil {
			return "", "", err
		}
	}
	if err := enableCgroupControllers(rootPath, "cpu", "memory", "pids"); err != nil {
		return "", "", err
	}
	if err := enableCgroupControllers(poolRootPath, "cpu", "memory", "pids"); err != nil {
		return "", "", err
	}
	return rootRel, rootPath, nil
}

func (s *Server) delegatedCgroupRoot() (string, error) {
	s.cgroupMu.Lock()
	defer s.cgroupMu.Unlock()
	if s.delegatedCgroupRel != "" {
		return s.delegatedCgroupRel, nil
	}
	currentRel, err := currentCgroupPath()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(currentRel) {
		return "", fmt.Errorf("current cgroup path %q is not absolute", currentRel)
	}
	if filepath.Base(currentRel) == firecrackerSupervisorCgroupName {
		currentRel = filepath.Dir(currentRel)
	}
	s.delegatedCgroupRel = currentRel
	return s.delegatedCgroupRel, nil
}

func (s *Server) readInstanceMetrics(_ context.Context, req MetricsRequest) (MetricsResponse, error) {
	rootRel, err := s.delegatedCgroupRoot()
	if err != nil {
		return MetricsResponse{}, err
	}
	cgroupPath, _, err := resolveFirecrackerCgroupPath(rootRel, req.Name)
	if err != nil {
		return MetricsResponse{}, err
	}
	cpuUsageUsec, err := readCPUUsageUsec(filepath.Join(cgroupPath, "cpu.stat"))
	if err != nil {
		return MetricsResponse{}, err
	}
	memoryCurrentBytes, err := readInt64File(filepath.Join(cgroupPath, "memory.current"))
	if err != nil {
		return MetricsResponse{}, err
	}
	memoryLimitBytes, err := readInt64File(filepath.Join(cgroupPath, "memory.max"))
	if err != nil {
		return MetricsResponse{}, err
	}
	return MetricsResponse{
		CPUUsageUsec:       cpuUsageUsec,
		MemoryCurrentBytes: memoryCurrentBytes,
		MemoryLimitBytes:   memoryLimitBytes,
	}, nil
}

func moveProcessToCgroup(pid int, cgroupPath string) error {
	if pid <= 0 {
		return fmt.Errorf("move process to cgroup %s: invalid pid %d", cgroupPath, pid)
	}
	if err := writeTextFile(filepath.Join(cgroupPath, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Errorf("move pid %d into cgroup %s: %w", pid, cgroupPath, err)
	}
	return nil
}

func enableCgroupControllers(cgroupPath string, controllers ...string) error {
	available, err := readCgroupControllerFile(filepath.Join(cgroupPath, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("read cgroup controllers for %s: %w", cgroupPath, err)
	}
	enabled, err := readCgroupControllerFile(filepath.Join(cgroupPath, "cgroup.subtree_control"))
	if err != nil {
		return fmt.Errorf("read cgroup subtree control for %s: %w", cgroupPath, err)
	}
	var ops []string
	for _, controller := range controllers {
		if _, ok := available[controller]; !ok {
			return fmt.Errorf("cgroup %s does not expose delegated %s controller", cgroupPath, controller)
		}
		if _, ok := enabled[controller]; ok {
			continue
		}
		ops = append(ops, "+"+controller)
	}
	if len(ops) == 0 {
		return nil
	}
	if err := writeTextFile(filepath.Join(cgroupPath, "cgroup.subtree_control"), []byte(strings.Join(ops, " ")), 0o644); err != nil {
		return fmt.Errorf("enable cgroup controllers %v for %s: %w", controllers, cgroupPath, err)
	}
	return nil
}

func writeCgroupValue(cgroupPath, fileName, value string) error {
	if err := writeTextFile(filepath.Join(cgroupPath, fileName), []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %s for %s: %w", fileName, cgroupPath, err)
	}
	return nil
}

func readCgroupControllerFile(path string) (map[string]struct{}, error) {
	payload, err := readTextFile(path)
	if err != nil {
		return nil, err
	}
	values := make(map[string]struct{})
	for _, controller := range strings.Fields(string(payload)) {
		values[strings.TrimPrefix(controller, "+")] = struct{}{}
	}
	return values, nil
}

func readCPUUsageUsec(path string) (uint64, error) {
	payload, err := readTextFile(path)
	if err != nil {
		return 0, fmt.Errorf("read cpu stat %s: %w", path, err)
	}
	for _, line := range strings.Split(string(payload), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "usage_usec" {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse cpu usage from %s: %w", path, err)
		}
		return value, nil
	}
	return 0, fmt.Errorf("usage_usec missing from %s", path)
}

func readInt64File(path string) (int64, error) {
	payload, err := readTextFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "max" {
		return 0, nil
	}
	value, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	return value, nil
}

func cgroupPathOnHost(rel string) string {
	return filepath.Join(cgroupFSRoot, strings.TrimPrefix(rel, "/"))
}

func firecrackerFixedCgroupPathUnder(cgroupRel, name string) (string, error) {
	if !filepath.IsAbs(cgroupRel) {
		return "", fmt.Errorf("current cgroup path %q is not absolute", cgroupRel)
	}
	vmRoot := filepath.Join(cgroupPathOnHost(cgroupRel), firecrackerVMRootCgroupName)
	child, err := directChildPath(vmRoot, name)
	if err != nil {
		return "", fmt.Errorf("resolve firecracker cgroup for %q: %w", name, err)
	}
	return child, nil
}

func resolveFirecrackerCgroupPath(cgroupRel, name string) (string, string, error) {
	child, err := firecrackerFixedCgroupPathUnder(cgroupRel, name)
	if err != nil {
		return "", "", err
	}
	if _, err := os.Stat(child); err == nil {
		return child, "", nil
	} else if !os.IsNotExist(err) {
		return "", "", fmt.Errorf("stat firecracker cgroup for %q: %w", name, err)
	}
	return findExistingPooledFirecrackerCgroupPath(cgroupRel, name)
}

func findExistingPooledFirecrackerCgroupPath(cgroupRel, name string) (string, string, error) {
	if !filepath.IsAbs(cgroupRel) {
		return "", "", fmt.Errorf("current cgroup path %q is not absolute", cgroupRel)
	}
	poolRoot := filepath.Join(cgroupPathOnHost(cgroupRel), firecrackerPoolRootCgroupName)
	entries, err := os.ReadDir(poolRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", os.ErrNotExist
		}
		return "", "", fmt.Errorf("list firecracker memory pool cgroups: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		poolPath, err := directChildPath(poolRoot, entry.Name())
		if err != nil {
			return "", "", fmt.Errorf("resolve firecracker memory pool cgroup for %q: %w", entry.Name(), err)
		}
		child, err := directChildPath(poolPath, name)
		if err != nil {
			return "", "", fmt.Errorf("resolve pooled firecracker cgroup for %q: %w", name, err)
		}
		if _, err := os.Stat(child); err == nil {
			return child, poolPath, nil
		} else if !os.IsNotExist(err) {
			return "", "", fmt.Errorf("stat pooled firecracker cgroup for %q: %w", name, err)
		}
	}
	return "", "", os.ErrNotExist
}

func (s *Server) cleanupFirecrackerCgroup(name string) error {
	rootRel, err := s.delegatedCgroupRoot()
	if err != nil {
		return fmt.Errorf("cleanup firecracker cgroup for %q: %w", name, err)
	}
	return cleanupFirecrackerCgroupUnder(rootRel, name)
}

func cleanupFirecrackerCgroupUnder(rootRel, name string) error {
	cgroupPath, _, err := resolveFirecrackerCgroupPath(rootRel, name)
	if err != nil {
		return fmt.Errorf("cleanup firecracker cgroup for %q: %w", name, err)
	}
	if err := removePath(cgroupPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cleanup firecracker cgroup for %q: remove %s: %w", name, cgroupPath, err)
	}
	return nil
}

func (s *Server) deleteMemoryPoolCgroup(_ context.Context, req MemoryPoolRequest) error {
	rootRel, err := s.delegatedCgroupRoot()
	if err != nil {
		return fmt.Errorf("delete memory pool cgroup for %q: %w", req.Name, err)
	}
	poolRoot := filepath.Join(cgroupPathOnHost(rootRel), firecrackerPoolRootCgroupName)
	poolPath, err := directChildPath(poolRoot, req.Name)
	if err != nil {
		return fmt.Errorf("delete memory pool cgroup for %q: %w", req.Name, err)
	}
	if err := removePath(poolPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete memory pool cgroup for %q: remove %s: %w", req.Name, poolPath, err)
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
