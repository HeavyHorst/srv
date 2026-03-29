package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "tailscale.com/feature/condregister/oauthkey"

	"srv/internal/config"
	"srv/internal/service"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	app, err := service.New(cfg, logger)
	if err != nil {
		logger.Error("init service", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx); err != nil {
		logger.Error("service exited", "err", err)
		os.Exit(1)
	}
}
