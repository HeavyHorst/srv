package service

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"srv/internal/config"
	"srv/internal/model"
)

type zenGatewayManager = providerGatewayManager
type zenGatewaySpec = providerGatewaySpec

func newZenGatewayManager(cfg config.Config, logger *slog.Logger) (*zenGatewayManager, error) {
	return newProviderGatewayManager(providerGatewayConfig{
		name:      "zen",
		apiKey:    cfg.ZenAPIKey,
		port:      cfg.ZenGatewayPort,
		baseURL:   cfg.ZenBaseURL,
		applyAuth: applyZenGatewayAuth,
	}, logger)
}

func applyZenGatewayAuth(headers http.Header, requestPath, apiKey string) {
	headers.Del("Authorization")
	headers.Del("X-API-Key")
	headers.Del("X-Goog-Api-Key")

	trimmedPath := "/" + strings.TrimLeft(strings.TrimSpace(requestPath), "/")
	switch {
	case trimmedPath == "/v1/messages" || strings.HasPrefix(trimmedPath, "/v1/messages/"):
		headers.Set("X-API-Key", apiKey)
	case strings.HasPrefix(trimmedPath, "/v1/models/"):
		headers.Set("X-Goog-Api-Key", apiKey)
	default:
		// Keep OpenAI-style and OpenAI-compatible families on bearer auth, which
		// matches direct OpenCode traffic for `/v1/responses`, `/v1/chat/completions`,
		// and generic Zen discovery like `/v1/models`.
		headers.Set("Authorization", "Bearer "+apiKey)
	}
}

func (a *App) syncZenGatewayBestEffort() {
	if a == nil || a.zenGateway == nil || a.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	instances, err := a.store.ListInstances(ctx, false)
	if err != nil {
		a.log.Error("list instances for zen gateway sync", "err", err)
		return
	}
	if err := a.zenGateway.Reconcile(ctx, instances); err != nil {
		a.log.Error("sync zen gateways", "err", err)
	}
}

func (a *App) zenGatewayBaseURL(inst model.Instance) string {
	if a == nil || a.zenGateway == nil {
		return ""
	}
	return providerGatewayBaseURL(inst, a.cfg.ZenGatewayPort)
}
