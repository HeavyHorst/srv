package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"srv/internal/nethelper"
)

func main() {
	var (
		socketPath  string
		tapUser     string
		clientGroup string
	)

	flag.StringVar(&socketPath, "socket", getenv("SRV_NET_HELPER_SOCKET", nethelper.DefaultSocketPath), "unix socket path for the privileged network helper")
	flag.StringVar(&tapUser, "tap-user", getenv("SRV_NET_HELPER_TAP_USER", "srv"), "user that should own newly created TAP devices")
	flag.StringVar(&clientGroup, "client-group", getenv("SRV_NET_HELPER_CLIENT_GROUP", "srv"), "group allowed to connect to the helper socket")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	server := nethelper.NewServer(logger, tapUser)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := server.ServeUnix(ctx, socketPath, clientGroup); err != nil {
		logger.Error("network helper exited", "err", err)
		os.Exit(1)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
