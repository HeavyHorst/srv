package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"srv/internal/vmrunner"
)

func main() {
	var (
		socketPath        string
		clientGroup       string
		firecrackerBinary string
	)

	flag.StringVar(&socketPath, "socket", getenv("SRV_VM_RUNNER_SOCKET", vmrunner.DefaultSocketPath), "unix socket path for the Firecracker runner helper")
	flag.StringVar(&clientGroup, "client-group", getenv("SRV_VM_RUNNER_CLIENT_GROUP", "srv"), "group allowed to connect to the VM runner socket")
	flag.StringVar(&firecrackerBinary, "firecracker-bin", getenv("SRV_FIRECRACKER_BIN", "/usr/bin/firecracker"), "firecracker binary path")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	server := vmrunner.NewServer(logger, firecrackerBinary)

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
