package config

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultDataDir          = "/var/lib/srv"
	defaultHostname         = "srv"
	defaultListenAddr       = ":22"
	defaultNetHelperSocket  = "/run/srv/net-helper.sock"
	defaultVMRunnerSocket   = "/run/srv-vm-runner/vm-runner.sock"
	defaultFirecrackerBin   = "/usr/bin/firecracker"
	defaultVMNetworkCIDR    = "172.28.0.0/16"
	defaultZenBaseURL       = "https://opencode.ai/zen"
	defaultZenGatewayPort   = 11434
	defaultGuestAuthExpiry  = 15 * time.Minute
	defaultGuestReadyTimout = 2 * time.Minute
	MaxVCPUCount            = 32
	MinMemoryMiB            = 128
)

type Config struct {
	DataDir                string
	Hostname               string
	ListenAddr             string
	TailscaleAuthKey       string
	TailscaleClientID      string
	TailscaleClientSecret  string
	TailscaleControlURL    string
	TailscaleAPIBaseURL    string
	TailscaleAdvertiseTags []string
	Tailnet                string
	AllowedUsers           []string
	AdminUsers             []string

	BaseKernelPath           string
	BaseInitrdPath           string
	BaseRootFSPath           string
	NetHelperSocketPath      string
	VMRunnerSocketPath       string
	FirecrackerBinary        string
	OutboundInterface        string
	VMNetworkCIDR            string
	VMDNSServers             []string
	VCPUCount                int64
	MemoryMiB                int64
	GuestAuthExpiry          time.Duration
	GuestReadyTimeout        time.Duration
	GuestAuthTags            []string
	GuestTailscaleControlURL string
	GuestTailscaleAuthOnce   bool
	ExtraKernelArgs          string
	ZenAPIKey                string
	ZenBaseURL               string
	ZenGatewayPort           int

	LogLevel string

	dataDirAbs string
}

func Load() (Config, error) {
	var cfg Config

	flag.StringVar(&cfg.DataDir, "data-dir", getenv("SRV_DATA_DIR", defaultDataDir), "service state directory")
	flag.StringVar(&cfg.Hostname, "hostname", getenv("SRV_HOSTNAME", defaultHostname), "tailscale hostname for the control plane")
	flag.StringVar(&cfg.ListenAddr, "listen-addr", getenv("SRV_LISTEN_ADDR", defaultListenAddr), "tailnet TCP listen address for the SSH API")
	flag.StringVar(&cfg.TailscaleAuthKey, "tailscale-auth-key", os.Getenv("TS_AUTHKEY"), "tailscale auth key for the control-plane node")
	flag.StringVar(&cfg.TailscaleClientID, "tailscale-client-id", getenv("TS_CLIENT_ID", os.Getenv("TS_API_CLIENT_ID")), "tailscale OAuth client id")
	flag.StringVar(&cfg.TailscaleClientSecret, "tailscale-client-secret", getenv("TS_CLIENT_SECRET", os.Getenv("TS_API_CLIENT_SECRET")), "tailscale OAuth client secret")
	flag.StringVar(&cfg.TailscaleControlURL, "tailscale-control-url", os.Getenv("TS_CONTROL_URL"), "alternate tailscale coordination server")
	flag.StringVar(&cfg.TailscaleAPIBaseURL, "tailscale-api-base-url", getenv("TS_API_BASE_URL", getenv("TS_BASE_URL", "https://api.tailscale.com")), "tailscale control API base URL")
	flag.StringVar(&cfg.Tailnet, "tailnet", os.Getenv("TS_TAILNET"), "tailnet name used for API operations")
	allowedUsers := flag.String("allowed-users", getenv("SRV_ALLOWED_USERS", ""), "comma separated tailscale user logins allowed to invoke commands")
	adminUsers := flag.String("admin-users", getenv("SRV_ADMIN_USERS", ""), "comma separated tailscale user logins allowed to view and manage every instance")
	advertiseTags := flag.String("advertise-tags", getenv("SRV_ADVERTISE_TAGS", ""), "comma separated tailscale tags to advertise for the control-plane node")

	flag.StringVar(&cfg.BaseKernelPath, "base-kernel", getenv("SRV_BASE_KERNEL", ""), "path to the Firecracker kernel image")
	flag.StringVar(&cfg.BaseInitrdPath, "base-initrd", getenv("SRV_BASE_INITRD", ""), "path to the optional initrd image")
	flag.StringVar(&cfg.BaseRootFSPath, "base-rootfs", getenv("SRV_BASE_ROOTFS", ""), "path to the base rootfs image stored on the same reflink-capable filesystem as SRV_DATA_DIR, such as btrfs or reflink-enabled xfs")
	flag.StringVar(&cfg.NetHelperSocketPath, "net-helper-socket", getenv("SRV_NET_HELPER_SOCKET", defaultNetHelperSocket), "unix socket used to reach the privileged network helper")
	flag.StringVar(&cfg.VMRunnerSocketPath, "vm-runner-socket", getenv("SRV_VM_RUNNER_SOCKET", defaultVMRunnerSocket), "unix socket used to reach the Firecracker runner helper")
	flag.StringVar(&cfg.FirecrackerBinary, "firecracker-bin", getenv("SRV_FIRECRACKER_BIN", defaultFirecrackerBin), "firecracker binary path")
	flag.StringVar(&cfg.OutboundInterface, "outbound-interface", getenv("SRV_OUTBOUND_IFACE", ""), "host network interface used for VM NAT")
	flag.StringVar(&cfg.VMNetworkCIDR, "vm-network-cidr", getenv("SRV_VM_NETWORK_CIDR", defaultVMNetworkCIDR), "IPv4 network reserved for VM /30 allocations")
	vmDNS := flag.String("vm-dns", getenv("SRV_VM_DNS", "1.1.1.1,1.0.0.1"), "comma separated guest nameservers")
	guestTags := flag.String("guest-auth-tags", getenv("SRV_GUEST_AUTH_TAGS", ""), "comma separated tags applied to one-off guest auth keys")
	guestExpiry := flag.String("guest-auth-expiry", getenv("SRV_GUEST_AUTH_EXPIRY", defaultGuestAuthExpiry.String()), "one-off guest auth key TTL")
	guestReadyTimeout := flag.String("guest-ready-timeout", getenv("SRV_GUEST_READY_TIMEOUT", defaultGuestReadyTimout.String()), "time to wait for a guest to join the tailnet")
	flag.StringVar(&cfg.GuestTailscaleControlURL, "guest-tailscale-control-url", getenv("SRV_GUEST_TAILSCALE_CONTROL_URL", os.Getenv("TS_CONTROL_URL")), "optional alternate control URL injected into the guest bootstrap contract")
	flag.StringVar(&cfg.ExtraKernelArgs, "extra-kernel-args", getenv("SRV_EXTRA_KERNEL_ARGS", ""), "additional kernel arguments appended to the guest boot line")
	flag.StringVar(&cfg.ZenAPIKey, "zen-api-key", getenv("SRV_ZEN_API_KEY", ""), "optional OpenCode Zen API key used by the host-side guest gateway")
	flag.StringVar(&cfg.ZenBaseURL, "zen-base-url", getenv("SRV_ZEN_BASE_URL", defaultZenBaseURL), "base URL for the upstream OpenCode Zen API")
	flag.IntVar(&cfg.ZenGatewayPort, "zen-gateway-port", getenvInt("SRV_ZEN_GATEWAY_PORT", defaultZenGatewayPort), "TCP port exposed on each VM host/gateway IP for the host-side OpenCode Zen proxy")
	flag.Int64Var(&cfg.VCPUCount, "vm-vcpus", getenvInt64("SRV_VM_VCPUS", 1), "number of guest vCPUs")
	flag.Int64Var(&cfg.MemoryMiB, "vm-memory-mib", getenvInt64("SRV_VM_MEMORY_MIB", 1024), "guest memory in MiB")
	flag.StringVar(&cfg.LogLevel, "log-level", getenv("SRV_LOG_LEVEL", "info"), "log level")

	flag.Parse()

	cfg.AllowedUsers = splitCSV(*allowedUsers)
	cfg.AdminUsers = splitCSV(*adminUsers)
	cfg.TailscaleAdvertiseTags = splitCSV(*advertiseTags)
	cfg.VMDNSServers = splitCSV(*vmDNS)
	cfg.GuestAuthTags = splitCSV(*guestTags)
	cfg.GuestTailscaleAuthOnce = true

	var err error
	if cfg.GuestAuthExpiry, err = time.ParseDuration(*guestExpiry); err != nil {
		return Config{}, fmt.Errorf("parse guest auth expiry: %w", err)
	}
	if cfg.GuestReadyTimeout, err = time.ParseDuration(*guestReadyTimeout); err != nil {
		return Config{}, fmt.Errorf("parse guest ready timeout: %w", err)
	}

	cfg.dataDirAbs, err = filepath.Abs(cfg.DataDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve data dir: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c Config) Validate() error {
	if c.Hostname == "" {
		return errors.New("hostname is required")
	}
	if c.ListenAddr == "" {
		return errors.New("listen address is required")
	}
	if c.DataDir == "" {
		return errors.New("data dir is required")
	}
	if c.NetHelperSocketPath == "" {
		return errors.New("network helper socket path is required")
	}
	if c.VMRunnerSocketPath == "" {
		return errors.New("vm runner socket path is required")
	}
	if err := ValidateMachineShape(c.VCPUCount, c.MemoryMiB); err != nil {
		return err
	}
	if c.GuestAuthExpiry <= 0 {
		return errors.New("guest auth expiry must be positive")
	}
	if c.GuestReadyTimeout <= 0 {
		return errors.New("guest ready timeout must be positive")
	}
	if c.ZenGatewayPort < 1 || c.ZenGatewayPort > 65535 {
		return errors.New("zen gateway port must be between 1 and 65535")
	}
	if strings.TrimSpace(c.ZenBaseURL) == "" {
		return errors.New("zen base url is required")
	}
	if _, err := parseZenBaseURL(c.ZenBaseURL); err != nil {
		return err
	}
	return nil
}

func ValidateMachineShape(vcpus, memoryMiB int64) error {
	if vcpus < 1 {
		return errors.New("vm vcpu count must be >= 1")
	}
	if vcpus > MaxVCPUCount {
		return fmt.Errorf("vm vcpu count must be <= %d", MaxVCPUCount)
	}
	if vcpus != 1 && vcpus%2 != 0 {
		return errors.New("vm vcpu count must be 1 or an even number")
	}
	if memoryMiB < MinMemoryMiB {
		return fmt.Errorf("vm memory must be at least %d MiB", MinMemoryMiB)
	}
	return nil
}

func (c Config) DataDirAbs() string {
	return c.dataDirAbs
}

func (c Config) StateDir() string {
	return filepath.Join(c.dataDirAbs, "state")
}

func (c Config) DatabasePath() string {
	return filepath.Join(c.StateDir(), "app.db")
}

func (c Config) TSNetDir() string {
	return filepath.Join(c.StateDir(), "tsnet")
}

func (c Config) HostKeyPath() string {
	return filepath.Join(c.StateDir(), "host_key")
}

func (c Config) ImagesDir() string {
	return filepath.Join(c.dataDirAbs, "images")
}

func (c Config) InstancesDir() string {
	return filepath.Join(c.dataDirAbs, "instances")
}

func (c Config) BackupsDir() string {
	return filepath.Join(c.dataDirAbs, "backups")
}

func (c Config) SnapshotsDir() string {
	return filepath.Join(c.dataDirAbs, ".snapshots")
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvInt64(key string, fallback int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func getenvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return parsed
}

func parseZenBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("zen base url is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse zen base url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("zen base url must use http or https")
	}
	if parsed.Host == "" {
		return "", errors.New("zen base url must include a host")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

func splitCSV(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
