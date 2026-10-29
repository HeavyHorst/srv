package provision

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"srv/internal/model"
	"srv/internal/orbguest"
)

func awaitingState(inst model.Instance) string {
	if inst.KindName() == model.KindOrb {
		return model.StateAwaitingGuest
	}
	return model.StateAwaitingTailnet
}

// Readiness is the host-only bootstrap channel, never optional project networking.
func (p *Provisioner) waitForOrb(ctx context.Context, inst model.Instance) (model.Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.GuestReadyTimeout)
	defer cancel()
	runtime, err := p.instanceRuntimeIdentity(ctx, inst)
	if err != nil {
		return inst, err
	}
	dir, err := p.instanceDir(inst.Name)
	if err != nil {
		return inst, err
	}
	for {
		probe, cancelProbe := context.WithTimeout(ctx, 2*time.Second)
		err = orbguest.Ping(probe, filepath.Join(dir, "orb.vsock"))
		cancelProbe()
		if err == nil {
			current, runtimeErr := p.instanceRuntimeIdentity(ctx, inst)
			if runtimeErr != nil || current != runtime {
				return inst, errors.Join(errors.New("orb runtime changed during readiness check"), runtimeErr)
			}
			updated := inst
			updated.State, updated.LastError = model.StateReady, ""
			updated.UpdatedAt = time.Now().UTC()
			if err := p.store.UpdateInstanceRuntime(ctx, inst, updated); err != nil {
				return inst, err
			}
			p.recordEvent(inst.ID, "orb", "orb bootstrap channel ready", nil)
			return updated, nil
		}
		select {
		case <-ctx.Done():
			return inst, errors.Join(errors.New("orb bootstrap channel unavailable"), ctx.Err(), err)
		case <-time.After(time.Second):
		}
	}
}
