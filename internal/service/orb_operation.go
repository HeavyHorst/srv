package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"srv/internal/model"
	"srv/internal/store"
)

// Bound operations are optional for human CLI use, mandatory for the provider.
// Check under the instance lock, not in the front's point-in-time inventory.
func (a *App) checkOrbIdentity(ctx context.Context, inst model.Instance, identity []string) error {
	if err := a.checkOrbBinding(ctx, inst, identity); err != nil {
		return err
	}
	return a.store.CheckCleanupFence(ctx, inst.ID)
}

func (a *App) checkInstanceBinding(inst model.Instance, identity []string) error {
	if len(identity) != 4 {
		return errors.New("expected UUID CREATED_AT BACKEND_ID IMAGE")
	}
	created, err := time.Parse(time.RFC3339Nano, identity[1])
	if err != nil {
		return err
	}
	if inst.ID != identity[0] || !inst.CreatedAt.Equal(created) || a.backendID == "" || a.backendID != identity[2] || inst.ImageName() != identity[3] {
		return errors.New("instance incarnation or backend identity mismatch")
	}
	return nil
}

func (a *App) checkOrbBinding(ctx context.Context, inst model.Instance, identity []string) error {
	if err := a.checkInstanceBinding(inst, identity); err != nil {
		return err
	}
	if inst.KindName() != model.KindOrb {
		return errors.New("orb incarnation required")
	}
	if m, err := a.store.GetMigration(ctx, inst.ID); err == nil && m.Phase != "aborted" {
		return errors.New("migration excludes bound orb operations")
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := a.store.GetImportReceipt(ctx, inst.ID); !errors.Is(err, sql.ErrNoRows) {
		return errors.New("import history excludes bound orb operations")
	}
	return nil
}

func orbOperationRequest(actor model.Actor, operation, name, id string, identity []string) store.LifecycleRequest {
	args, _ := json.Marshal(append([]string{operation, name}, identity...))
	return store.LifecycleRequest{ID: id, Actor: actor.UserLogin, Command: operation, Name: name, Arguments: string(args)}
}

func (a *App) beginOrbOperation(ctx context.Context, actor model.Actor, inst model.Instance, operation string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if len(args) != 6 || args[0] != "--expect" {
		return errors.New("expected --expect REQUEST_ID UUID CREATED_AT BACKEND_ID IMAGE")
	}
	if _, err := uuid.Parse(args[1]); err != nil {
		return err
	}
	if err := a.checkOrbIdentity(ctx, inst, args[2:]); err != nil {
		return err
	}
	r := orbOperationRequest(actor, operation, inst.Name, args[1], args[2:])
	admitted, err := a.store.AdmitLifecycleRequest(ctx, r)
	if err != nil {
		return err
	}
	if !admitted {
		return errors.New("operation already admitted; query operation-result, never replay")
	}
	return a.store.BeginLifecycleRequest(ctx, r.ID, r.Actor)
}

func (a *App) finishOrbOperation(ctx context.Context, actor model.Actor, args []string) error {
	if len(args) == 0 {
		return nil
	}
	ok, err := a.store.FinishLifecycleRequest(ctx, args[1], actor.UserLogin, 0)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("operation no longer executing")
	}
	return nil
}

// Cancellation closes admission, not running effects. Holding the same lock as
// the worker prevents a late SSH session from executing a cancelled request.
// An executing/interrupted request cannot be cancelled based on VM state.
func (a *App) cmdOperationResult(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	if len(args) == 8 && args[0] == "operation-cancel" {
		if args[3] != "start" && args[3] != "stop" {
			return cleanupError(errors.New("unsupported cancellable operation"))
		}
		if _, err := uuid.Parse(args[1]); err != nil {
			return cleanupError(err)
		}
		unlock := a.lockInstance(args[2])
		defer unlock()
		inst, err := a.lookupVisibleInstance(ctx, actor, args[2])
		if err != nil {
			return cleanupError(err)
		}
		if err := a.checkOrbIdentity(ctx, inst, args[4:]); err != nil {
			return cleanupError(err)
		}
		r := orbOperationRequest(actor, args[3], args[2], args[1], args[4:])
		if err := a.store.CancelLifecycleRequest(ctx, r); err != nil {
			return cleanupError(err)
		}
	} else if len(args) != 2 || args[0] != "operation-result" {
		return cleanupError(errors.New("usage: operation-result REQUEST_ID | operation-cancel REQUEST_ID NAME OPERATION UUID CREATED_AT BACKEND_ID IMAGE"))
	}
	r, err := a.store.GetLifecycleRequest(ctx, args[1], actor.UserLogin)
	if err != nil {
		return cleanupError(err)
	}
	if r.Command != "start" && r.Command != "stop" && r.Command != "bootstrap" {
		return cleanupError(fmt.Errorf("not an orb operation"))
	}
	return jsonResult(r)
}
