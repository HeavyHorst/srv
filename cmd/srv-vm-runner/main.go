package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"srv/internal/vmrunner"
)

func main() {
	var (
		socketPath        string
		clientGroup       string
		instancesDir      string
		kernelPath        string
		initrdPath        string
		firecrackerBinary string
	)

	defaultDataDir := getenv("SRV_DATA_DIR", "/var/lib/srv")
	flag.StringVar(&socketPath, "socket", getenv("SRV_VM_RUNNER_SOCKET", vmrunner.DefaultSocketPath), "unix socket path for the Firecracker runner helper")
	flag.StringVar(&clientGroup, "client-group", getenv("SRV_VM_RUNNER_CLIENT_GROUP", "srv"), "group allowed to connect to the VM runner socket")
	flag.StringVar(&instancesDir, "instances-dir", filepath.Join(defaultDataDir, "instances"), "base directory containing per-instance runtime artifacts")
	flag.StringVar(&kernelPath, "base-kernel", getenv("SRV_BASE_KERNEL", ""), "path to the Firecracker kernel image")
	flag.StringVar(&initrdPath, "base-initrd", getenv("SRV_BASE_INITRD", ""), "path to the optional initrd image")
	flag.StringVar(&firecrackerBinary, "firecracker-bin", getenv("SRV_FIRECRACKER_BIN", "/usr/bin/firecracker"), "firecracker binary path")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := vmrunner.ServerConfig{
		FirecrackerBinary: firecrackerBinary,
		InstancesDir:      instancesDir,
		KernelPath:        kernelPath,
		InitrdPath:        initrdPath,
	}
	if err := cfg.Validate(); err != nil {
		logger.Error("invalid vm runner config", "err", err)
		os.Exit(2)
	}
	server := vmrunner.NewServer(logger, cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := server.ServeUnix(ctx, socketPath, clientGroup); err != nil {
		logger.Error("vm runner exited", "err", err)
		os.Exit(1)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
