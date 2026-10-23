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

// delete-proof REQUEST_ID NAME UUID CREATED_AT BACKEND_ID IMAGE.
// The service owns identity, admission, and receipt publication under its locks.
func (a *App) cmdDeleteProof(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	if len(args) != 7 {
		return cleanupError(errors.New("complete cleanup identity required"))
	}
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
		if err != nil {
			return cleanupError(fmt.Errorf("cleanup request %s; no committed receipt or replay permitted", old.Phase))
		}
		return jsonResult(proof)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return cleanupError(err)
	}
	inst, err := a.lookupVisibleInstance(ctx, actor, args[2])
	if err != nil {
		return cleanupError(err)
	}
	if err := a.checkOrbIdentity(ctx, inst, args[3:]); err != nil {
		return cleanupError(err)
	}
	if inst.State != model.StateReady && inst.State != model.StateStopped {
		return cleanupError(fmt.Errorf("cleanup excludes state %s", inst.State))
	}
	admitted, err := a.store.AdmitLifecycleRequest(ctx, request)
	if err != nil {
		return cleanupError(err)
	}
	if !admitted {
		return cleanupError(errors.New("request already admitted; query only"))
	}
	proof, err := a.provisioner.DeleteWithProof(ctx, inst, request.ID, request.Actor, a.backendID)
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
