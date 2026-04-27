package service

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"srv/internal/config"
	"srv/internal/model"
)

type deepseekGatewayManager = providerGatewayManager
type deepseekGatewaySpec = providerGatewaySpec

func newDeepseekGatewayManager(cfg config.Config, logger *slog.Logger) (*deepseekGatewayManager, error) {
	return newProviderGatewayManager(providerGatewayConfig{
		name:      "deepseek",
		apiKey:    cfg.DeepSeekAPIKey,
		port:      cfg.DeepSeekGatewayPort,
		baseURL:   cfg.DeepSeekBaseURL,
		applyAuth: applyDeepseekGatewayAuth,
	}, logger)
}

func applyDeepseekGatewayAuth(headers http.Header, _ string, apiKey string) {
	headers.Del("Authorization")
	headers.Set("Authorization", "Bearer "+apiKey)
}

func (a *App) syncDeepseekGatewayBestEffort() {
	if a == nil || a.deepseekGateway == nil || a.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	instances, err := a.store.ListInstances(ctx, false)
	if err != nil {
		a.log.Error("list instances for deepseek gateway sync", "err", err)
		return
	}
	if err := a.deepseekGateway.Reconcile(ctx, instances); err != nil {
		a.log.Error("sync deepseek gateways", "err", err)
	}
}

func (a *App) deepseekGatewayBaseURL(inst model.Instance) string {
	if a == nil || a.deepseekGateway == nil {
		return ""
	}
	return providerGatewayBaseURL(inst, a.cfg.DeepSeekGatewayPort)
}
