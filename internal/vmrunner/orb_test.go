package vmrunner

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestOrbSocketExposure(t *testing.T) {
	root := t.TempDir()
	jail := filepath.Join(root, "jail")
	instance := filepath.Join(root, "instance")
	for _, dir := range []string{jail, instance} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	l, err := net.Listen("unix", filepath.Join(jail, "orb.vsock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := exposeOrbSocket(instance, jail); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(instance, "orb.vsock")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o660 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("mode = %v", info.Mode())
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := cleanupJailedRuntimePaths(instanceRuntimePaths{SocketPath: filepath.Join(instance, "firecracker.sock")}, jailerRuntimePaths{WorkspaceDir: jail}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("stale socket: %v", err)
	}
}

func TestOrbStartKindValidation(t *testing.T) {
	req := StartRequest{Name: "test", TapDevice: "tap0", GuestMAC: "02:00:00:00:00:01", GuestAddr: "10.0.0.2/24", GatewayAddr: "10.0.0.1", VCPUCount: 1, MemoryMiB: 128}
	for _, kind := range []string{"", "vm", "orb", "other"} {
		req.Kind = kind
		req.Bootstrap.Hostname = "guest"
		if err := req.Validate(); (err == nil) != (kind != "other") {
			t.Fatalf("kind %q: %v", kind, err)
		}
	}
	req.Kind = "orb"
	req.Bootstrap.Hostname = ""
	if err := req.Validate(); err != nil {
		t.Fatalf("orb needs no native metadata: %v", err)
	}
}
