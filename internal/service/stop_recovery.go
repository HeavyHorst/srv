package service

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"srv/internal/model"
	"srv/internal/store"
	"srv/internal/vmrunner"
)

func (a *App) validateStopRecovery(ctx context.Context, actor model.Actor, old store.LifecycleRequest) error {
	r := old.Recovery
	if old.Phase != "recovered" || r == nil || r.Phase != "done" || !r.MatchesRequest(old) {
		return errors.New("missing completed stop recovery proof")
	}
	inst, err := a.lookupVisibleInstance(ctx, actor, r.Name)
	if err != nil {
		return err
	}
	if err := a.checkOrbIdentity(ctx, inst, []string{r.InstanceID, r.CreatedAt, r.BackendID, r.Image}); err != nil {
		return err
	}
	if inst.State != model.StateStopped || inst.StartRequestID != r.StartRequestID || inst.StartBootID != r.HostBootID {
		return errors.New("stop recovery attempt is no longer current")
	}
	generation, err := a.store.BootstrapGeneration(ctx, inst.ID, inst.CreatedAt)
	if err != nil {
		return err
	}
	if generation != r.BootstrapGeneration {
		return errors.New("stop recovery bootstrap generation changed")
	}
	return nil
}

func (a *App) cmdReconcileStop(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	if len(args) != 15 || args[4] != "--save-state" || args[5] != "--expect" || args[10] != "--attempt" || args[13] != "--generation" {
		return cleanupError(errors.New("usage: operation-reconcile-stop RECOVERY_ID REQUEST_ID NAME --save-state --expect UUID CREATED_AT BACKEND_ID IMAGE --attempt START_ID HOST_BOOT_ID --generation N"))
	}
	for _, id := range []string{args[1], args[2], args[6], args[11]} {
		if _, err := uuid.Parse(id); err != nil {
			return cleanupError(err)
		}
	}
	generation, err := strconv.ParseInt(args[14], 10, 64)
	if err != nil || generation < 0 {
		return cleanupError(errors.New("invalid bootstrap generation"))
	}
	unlock := a.lockInstance(args[3])
	defer func() { unlock(); a.syncManagedGatewaysBestEffort() }()
	inst, err := a.lookupVisibleInstance(ctx, actor, args[3])
	if err != nil {
		return cleanupError(err)
	}
	if err := a.checkOrbIdentity(ctx, inst, args[6:10]); err != nil {
		return cleanupError(err)
	}
	if inst.State != model.StateStopping && inst.State != model.StateStopped {
		return cleanupError(errors.New("stop recovery requires a stopping or stopped orb"))
	}
	if inst.StartRequestID != args[11] || inst.StartBootID != args[12] {
		return cleanupError(errors.New("stop recovery attempt differs"))
	}
	currentGeneration, err := a.store.BootstrapGeneration(ctx, inst.ID, inst.CreatedAt)
	if err != nil {
		return cleanupError(err)
	}
	if currentGeneration != generation {
		return cleanupError(errors.New("stop recovery bootstrap generation differs"))
	}
	old, err := a.store.GetLifecycleRequest(ctx, args[2], actor.UserLogin)
	if err != nil {
		return cleanupError(err)
	}
	expected := orbOperationRequest(actor, "stop", inst.Name, args[2], args[6:10])
	if old.Command != expected.Command || old.Name != expected.Name || old.Arguments != expected.Arguments {
		return cleanupError(errors.New("original stop binding differs"))
	}
	if old.Phase == "done" {
		return jsonResult(old)
	}
	recovery := store.StopRecovery{ID: args[1], OriginalRequestID: old.ID, Actor: actor.UserLogin, OriginalArguments: old.Arguments, Name: inst.Name, InstanceID: inst.ID, CreatedAt: args[7], BackendID: args[8], Image: args[9], StartRequestID: args[11], HostBootID: args[12], BootstrapGeneration: generation, SaveState: true}
	if err := a.store.BeginStopRecovery(ctx, recovery); err != nil {
		return cleanupError(err)
	}
	if old.Phase == "recovered" {
		if err := a.validateStopRecovery(ctx, actor, old); err != nil {
			return cleanupError(err)
		}
		return jsonResult(old)
	}
	if _, err := a.provisioner.ReconcileSavedStop(ctx, inst.Name, vmrunner.StartResultRequest{Name: inst.Name, InstanceID: inst.ID, RequestID: inst.StartRequestID, HostBootID: inst.StartBootID}); err != nil {
		return cleanupError(err)
	}
	persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.store.CompleteStopRecovery(persistCtx, recovery.ID, actor.UserLogin); err != nil {
		return cleanupError(err)
	}
	result, err := a.store.GetLifecycleRequest(persistCtx, old.ID, actor.UserLogin)
	if err != nil {
		return cleanupError(err)
	}
	return jsonResult(result)
}
