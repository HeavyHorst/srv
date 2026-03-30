package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
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
		jailerBinary      string
		jailerBaseDir     string
		jailerUser        string
		jailerGroup       string
	)

	defaultDataDir := getenv("SRV_DATA_DIR", "/var/lib/srv")
	flag.StringVar(&socketPath, "socket", getenv("SRV_VM_RUNNER_SOCKET", vmrunner.DefaultSocketPath), "unix socket path for the Firecracker runner helper")
	flag.StringVar(&clientGroup, "client-group", getenv("SRV_VM_RUNNER_CLIENT_GROUP", "srv"), "group allowed to connect to the VM runner socket")
	flag.StringVar(&instancesDir, "instances-dir", filepath.Join(defaultDataDir, "instances"), "base directory containing per-instance runtime artifacts")
	flag.StringVar(&kernelPath, "base-kernel", getenv("SRV_BASE_KERNEL", ""), "path to the Firecracker kernel image")
	flag.StringVar(&initrdPath, "base-initrd", getenv("SRV_BASE_INITRD", ""), "path to the optional initrd image")
	flag.StringVar(&firecrackerBinary, "firecracker-bin", getenv("SRV_FIRECRACKER_BIN", "/usr/bin/firecracker"), "firecracker binary path")
	flag.StringVar(&jailerBinary, "jailer-bin", getenv("SRV_JAILER_BIN", "/usr/bin/jailer"), "path to the Firecracker jailer binary")
	flag.StringVar(&jailerBaseDir, "jailer-base-dir", getenv("SRV_JAILER_BASE_DIR", filepath.Join(defaultDataDir, "jailer")), "base directory where Firecracker jailer workspaces are created")
	flag.StringVar(&jailerUser, "jailer-user", getenv("SRV_JAILER_USER", "srv-vm"), "user that the jailer drops the Firecracker process to")
	flag.StringVar(&jailerGroup, "jailer-group", getenv("SRV_JAILER_GROUP", "srv"), "group that the jailer drops the Firecracker process to")
	flag.Parse()

	uid, gid, err := resolveProcessIdentity(jailerUser, jailerGroup)
	if err != nil {
		logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
		logger.Error("resolve jailer identity", "err", err)
		os.Exit(2)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := vmrunner.ServerConfig{
		FirecrackerBinary: firecrackerBinary,
		JailerBinary:      jailerBinary,
		JailerBaseDir:     jailerBaseDir,
		JailerUID:         uid,
		JailerGID:         gid,
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

func resolveProcessIdentity(username, groupName string) (int, int, error) {
	userEntry, err := user.Lookup(username)
	if err != nil {
		return 0, 0, fmt.Errorf("lookup jailer user %q: %w", username, err)
	}
	uid, err := strconv.Atoi(userEntry.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse uid for jailer user %q: %w", username, err)
	}
	groupEntry, err := user.LookupGroup(groupName)
	if err != nil {
		return 0, 0, fmt.Errorf("lookup jailer group %q: %w", groupName, err)
	}
	gid, err := strconv.Atoi(groupEntry.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse gid for jailer group %q: %w", groupName, err)
	}
	return uid, gid, nil
}
