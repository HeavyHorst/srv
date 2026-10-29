package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"srv/internal/model"
	"srv/internal/store"
)

func cleanupError(err error) (commandResult, error) {
	return commandResult{stderr: err.Error() + "\n", exitCode: 75}, err
}

// delete NAME --expect REQUEST_ID UUID CREATED_AT BACKEND_ID IMAGE.
// The service owns identity, admission, and receipt publication under its locks.
func (a *App) cmdDeleteExpected(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	if len(args) != 8 || args[2] != "--expect" {
		return cleanupError(errors.New("usage: delete NAME --expect REQUEST_ID UUID CREATED_AT BACKEND_ID IMAGE"))
	}
	// Keep the durable request format, not an alias for the former SSH command.
	// Existing requests must retain their binding across the syntax change.
	args = append([]string{"delete-proof", args[3], args[1]}, args[4:]...)
	if _, err := uuid.Parse(args[1]); err != nil {
		return cleanupError(err)
	}
	if _, err := uuid.Parse(args[3]); err != nil {
		return cleanupError(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	unlock := a.lockInstance(args[2])
	defer unlock()
	encoded, _ := json.Marshal(args)
	request := store.LifecycleRequest{ID: args[1], Actor: actor.UserLogin, Command: "delete-proof", Name: args[2], Arguments: string(encoded)}
	old, err := a.store.GetLifecycleRequest(ctx, request.ID, request.Actor)
	if err == nil {
		if old.Command != request.Command || old.Name != request.Name || old.Arguments != request.Arguments {
			return cleanupError(errors.New("request binding mismatch"))
		}
		proof, err := a.store.CleanupReceipt(ctx, request.ID, request.Actor)
		if err == nil {
			return jsonResult(proof)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return cleanupError(err)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return cleanupError(err)
	}
	inst, err := a.lookupVisibleInstance(ctx, actor, args[2])
	if err != nil {
		return cleanupError(err)
	}
	if err := a.checkInstanceBinding(inst, args[3:]); err != nil {
		return cleanupError(err)
	}
	if _, err := a.store.AdmitLifecycleRequest(ctx, request); err != nil {
		return cleanupError(err)
	}
	proof, err := a.provisioner.DeleteWithProof(ctx, inst, request.ID, request.Actor, a.backendID, a.closeInstanceGateways)
	if err != nil {
		// Only preflight failures may finish here. Executing intent remains
		// fenced even after cancellation, interruption or a partial effect.
		if current, readErr := a.store.GetLifecycleRequest(ctx, request.ID, request.Actor); readErr == nil && current.Phase == "admitted" {
			if _, finishErr := a.store.FinishLifecycleRequest(ctx, request.ID, request.Actor, 75); finishErr != nil {
				return cleanupError(finishErr)
			}
		}
		return cleanupError(err)
	}
	return jsonResult(proof)
}

// Called only after BeginCleanup has durably fenced the instance. Holding the
// same lock as inventory reads prevents stale reconciliation from reopening it.
func (a *App) closeInstanceGateways(ctx context.Context, inst model.Instance) error {
	a.gatewayMu.Lock()
	defer a.gatewayMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, manager := range []*providerGatewayManager{a.zenGateway, a.deepseekGateway} {
		if manager == nil {
			continue
		}
		manager.mu.Lock()
		err := manager.listeners[inst.Name].close(ctx)
		if err == nil {
			delete(manager.listeners, inst.Name)
		}
		manager.mu.Unlock()
		if err != nil {
			return err
		}
	}
	if manager := a.integrationGateway; manager != nil {
		manager.mu.Lock()
		var err error
		if listener := manager.listeners[inst.Name]; listener != nil {
			err = listener.close(ctx)
		}
		if err == nil {
			delete(manager.listeners, inst.Name)
		}
		manager.mu.Unlock()
		if err != nil {
			return err
		}
	}
	if manager := a.ampConnectGateway; manager != nil {
		manager.mu.Lock()
		err := manager.listeners[inst.Name].close(ctx)
		if err == nil {
			delete(manager.listeners, inst.Name)
		}
		manager.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (a *App) cmdCleanupReceipt(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	if len(args) != 2 {
		return cleanupError(errors.New("usage: cleanup-receipt <request-id>"))
	}
	r, err := a.store.CleanupReceipt(ctx, args[1], actor.UserLogin)
	if err != nil {
		if request, readErr := a.store.GetLifecycleRequest(ctx, args[1], actor.UserLogin); readErr == nil {
			return cleanupError(fmt.Errorf("cleanup request %s; no committed receipt", request.Phase))
		}
		return cleanupError(err)
	}
	return jsonResult(r)
}
