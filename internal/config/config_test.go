package config

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadUsesEnvironmentValues(t *testing.T) {
	resetFlagsForTest(t, []string{"srv.test"})

	relDataDir := filepath.Join("testdata", "..", "srv-data")
	expectedAbs, err := filepath.Abs(relDataDir)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}

	t.Setenv("SRV_DATA_DIR", relDataDir)
	t.Setenv("SRV_HOSTNAME", "control")
	t.Setenv("SRV_LISTEN_ADDR", ":2222")
	t.Setenv("SRV_ALLOWED_USERS", " alice@example.com, bob@example.com ")
	t.Setenv("SRV_ADMIN_USERS", " ops@example.com, root@example.com ")
	t.Setenv("SRV_ADVERTISE_TAGS", "tag:control, tag:ssh")
	t.Setenv("SRV_VM_DNS", "1.1.1.1, 8.8.8.8")
	t.Setenv("SRV_GUEST_AUTH_TAGS", "tag:microvm")
	t.Setenv("SRV_GUEST_AUTH_EXPIRY", "20m")
	t.Setenv("SRV_GUEST_READY_TIMEOUT", "3m")
	t.Setenv("SRV_NET_HELPER_SOCKET", "/run/srv/custom-net-helper.sock")
	t.Setenv("SRV_VM_RUNNER_SOCKET", "/run/srv-vm-runner/custom-vm-runner.sock")
	t.Setenv("SRV_ZEN_API_KEY", "zen-key")
	t.Setenv("SRV_ZEN_BASE_URL", "https://zen.example.test/base")
	t.Setenv("SRV_ZEN_GATEWAY_PORT", "12456")
	t.Setenv("SRV_INTEGRATION_GATEWAY_PORT", "12457")
	t.Setenv("SRV_VM_VCPUS", "2")
	t.Setenv("SRV_VM_MEMORY_MIB", "2048")
	t.Setenv("SRV_LOG_LEVEL", "debug")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}

	if cfg.Hostname != "control" {
		t.Fatalf("Hostname = %q, want %q", cfg.Hostname, "control")
	}
	if cfg.ListenAddr != ":2222" {
		t.Fatalf("ListenAddr = %q, want %q", cfg.ListenAddr, ":2222")
	}
	if cfg.DataDir != relDataDir {
		t.Fatalf("DataDir = %q, want %q", cfg.DataDir, relDataDir)
	}
	if cfg.DataDirAbs() != expectedAbs {
		t.Fatalf("DataDirAbs = %q, want %q", cfg.DataDirAbs(), expectedAbs)
	}
	if cfg.SnapshotsDir() != filepath.Join(expectedAbs, ".snapshots") {
		t.Fatalf("SnapshotsDir = %q, want %q", cfg.SnapshotsDir(), filepath.Join(expectedAbs, ".snapshots"))
	}
	if cfg.NetHelperSocketPath != "/run/srv/custom-net-helper.sock" {
		t.Fatalf("NetHelperSocketPath = %q, want %q", cfg.NetHelperSocketPath, "/run/srv/custom-net-helper.sock")
	}
	if cfg.VMRunnerSocketPath != "/run/srv-vm-runner/custom-vm-runner.sock" {
		t.Fatalf("VMRunnerSocketPath = %q, want %q", cfg.VMRunnerSocketPath, "/run/srv-vm-runner/custom-vm-runner.sock")
	}
	if cfg.ZenAPIKey != "zen-key" {
		t.Fatalf("ZenAPIKey = %q, want %q", cfg.ZenAPIKey, "zen-key")
	}
	if cfg.ZenBaseURL != "https://zen.example.test/base" {
		t.Fatalf("ZenBaseURL = %q, want %q", cfg.ZenBaseURL, "https://zen.example.test/base")
	}
	if cfg.ZenGatewayPort != 12456 {
		t.Fatalf("ZenGatewayPort = %d, want 12456", cfg.ZenGatewayPort)
	}
	if cfg.IntegrationGatewayPort != 12457 {
		t.Fatalf("IntegrationGatewayPort = %d, want 12457", cfg.IntegrationGatewayPort)
	}
	if !reflect.DeepEqual(cfg.AllowedUsers, []string{"alice@example.com", "bob@example.com"}) {
		t.Fatalf("AllowedUsers = %#v", cfg.AllowedUsers)
	}
	if !reflect.DeepEqual(cfg.AdminUsers, []string{"ops@example.com", "root@example.com"}) {
		t.Fatalf("AdminUsers = %#v", cfg.AdminUsers)
	}
	if !reflect.DeepEqual(cfg.TailscaleAdvertiseTags, []string{"tag:control", "tag:ssh"}) {
		t.Fatalf("TailscaleAdvertiseTags = %#v", cfg.TailscaleAdvertiseTags)
	}
	if !reflect.DeepEqual(cfg.VMDNSServers, []string{"1.1.1.1", "8.8.8.8"}) {
		t.Fatalf("VMDNSServers = %#v", cfg.VMDNSServers)
	}
	if !reflect.DeepEqual(cfg.GuestAuthTags, []string{"tag:microvm"}) {
		t.Fatalf("GuestAuthTags = %#v", cfg.GuestAuthTags)
	}
	if cfg.GuestAuthExpiry != 20*time.Minute {
		t.Fatalf("GuestAuthExpiry = %s, want %s", cfg.GuestAuthExpiry, 20*time.Minute)
	}
	if cfg.GuestReadyTimeout != 3*time.Minute {
		t.Fatalf("GuestReadyTimeout = %s, want %s", cfg.GuestReadyTimeout, 3*time.Minute)
	}
	if cfg.VCPUCount != 2 {
		t.Fatalf("VCPUCount = %d, want 2", cfg.VCPUCount)
	}
	if cfg.MemoryMiB != 2048 {
		t.Fatalf("MemoryMiB = %d, want 2048", cfg.MemoryMiB)
	}
	if !cfg.GuestTailscaleAuthOnce {
		t.Fatalf("GuestTailscaleAuthOnce = false, want true")
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
}

func TestValidateRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "missing hostname",
			mutate:  func(cfg *Config) { cfg.Hostname = "" },
			wantErr: "hostname is required",
		},
		{
			name:    "missing listen address",
			mutate:  func(cfg *Config) { cfg.ListenAddr = "" },
			wantErr: "listen address is required",
		},
		{
			name:    "missing data dir",
			mutate:  func(cfg *Config) { cfg.DataDir = "" },
			wantErr: "data dir is required",
		},
		{
			name:    "missing helper socket",
			mutate:  func(cfg *Config) { cfg.NetHelperSocketPath = "" },
			wantErr: "network helper socket path is required",
		},
		{
			name:    "missing vm runner socket",
			mutate:  func(cfg *Config) { cfg.VMRunnerSocketPath = "" },
			wantErr: "vm runner socket path is required",
		},
		{
			name:    "zero vcpus",
			mutate:  func(cfg *Config) { cfg.VCPUCount = 0 },
			wantErr: "vm vcpu count must be >= 1",
		},
		{
			name:    "odd vcpus",
			mutate:  func(cfg *Config) { cfg.VCPUCount = 3 },
			wantErr: "vm vcpu count must be 1 or an even number",
		},
		{
			name:    "too many vcpus",
			mutate:  func(cfg *Config) { cfg.VCPUCount = MaxVCPUCount + 1 },
			wantErr: "vm vcpu count must be <= 32",
		},
		{
			name:    "low memory",
			mutate:  func(cfg *Config) { cfg.MemoryMiB = 64 },
			wantErr: "vm memory must be at least 128 MiB",
		},
		{
			name:    "non-positive auth expiry",
			mutate:  func(cfg *Config) { cfg.GuestAuthExpiry = 0 },
			wantErr: "guest auth expiry must be positive",
		},
		{
			name:    "non-positive ready timeout",
			mutate:  func(cfg *Config) { cfg.GuestReadyTimeout = 0 },
			wantErr: "guest ready timeout must be positive",
		},
		{
			name:    "invalid zen gateway port",
			mutate:  func(cfg *Config) { cfg.ZenGatewayPort = 70000 },
			wantErr: "zen gateway port must be between 1 and 65535",
		},
		{
			name:    "invalid zen base url",
			mutate:  func(cfg *Config) { cfg.ZenBaseURL = "mailto:zen@example.com" },
			wantErr: "zen base url must use http or https",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() returned nil error, want %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestSplitCSVTrimsAndDropsEmptyValues(t *testing.T) {
	got := splitCSV(" alpha, , beta ,, gamma ")
	want := []string{"alpha", "beta", "gamma"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitCSV() = %#v, want %#v", got, want)
	}

	if got := splitCSV("   "); got != nil {
		t.Fatalf("splitCSV(blank) = %#v, want nil", got)
	}
}

func resetFlagsForTest(t *testing.T, args []string) {
	t.Helper()
	oldArgs := os.Args
	oldCommandLine := flag.CommandLine

	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flag.CommandLine = fs
	os.Args = args

	t.Cleanup(func() {
		flag.CommandLine = oldCommandLine
		os.Args = oldArgs
	})
}

func validConfig() Config {
	return Config{
		DataDir:                "/tmp/srv",
		Hostname:               "srv",
		ListenAddr:             ":22",
		NetHelperSocketPath:    "/run/srv/net-helper.sock",
		VMRunnerSocketPath:     "/run/srv-vm-runner/vm-runner.sock",
		ZenBaseURL:             "https://opencode.ai/zen",
		ZenGatewayPort:         11434,
		IntegrationGatewayPort: 11435,
		VCPUCount:              2,
		MemoryMiB:              1024,
		GuestAuthExpiry:        15 * time.Minute,
		GuestReadyTimeout:      2 * time.Minute,
	}
}
