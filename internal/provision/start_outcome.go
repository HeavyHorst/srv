package provision

import (
	"context"
	"errors"
	"fmt"
	"time"

	"srv/internal/model"
	"srv/internal/vmrunner"
)

type startResolver interface {
	ResolveInstanceStart(context.Context, vmrunner.StartResultRequest) (vmrunner.StartResponse, error)
}

func (p *Provisioner) persistInstance(inst model.Instance) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.store.UpdateInstance(ctx, inst)
}

func (p *Provisioner) startResult(ctx context.Context, inst model.Instance) (vmrunner.StartResponse, error) {
	resolver, ok := p.vmRunner.(startResolver)
	if !ok {
		return vmrunner.StartResponse{}, errors.New("VM runner does not support start outcome recovery")
	}
	return resolver.ResolveInstanceStart(ctx, vmrunner.StartResultRequest{
		Name: inst.Name, InstanceID: inst.ID, RequestID: inst.StartRequestID, HostBootID: inst.StartBootID,
	})
}

// Called under the service's instance lock, before any PID-based lifecycle
// decision. Unknown outcomes are never converted into permission to clean up.
func (p *Provisioner) resolveRuntime(ctx context.Context, inst *model.Instance) error {
	if inst.StartRequestID == "" || inst.State == model.StateDeleted {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := p.startResult(ctx, *inst)
	if err != nil {
		return errors.Join(errStartUncertain, err)
	}
	observed := *inst
	switch result.Outcome {
	case vmrunner.StartRunning:
		if result.PID <= 0 {
			return errStartUncertain
		}
		if inst.State == model.StateStopped || inst.State == model.StateFailed {
			return fmt.Errorf("%w: running attempt conflicts with stored state %s", errStartUncertain, inst.State)
		}
		inst.FirecrackerPID = result.PID
		if inst.State == model.StateProvisioning {
			inst.State = awaitingState(*inst)
		}
	case vmrunner.StartAbsent:
		if inst.FirecrackerPID != 0 || inst.State == model.StateProvisioning || inst.State == model.StateAwaitingTailnet || inst.State == model.StateAwaitingGuest || inst.State == model.StateReady {
			if err := p.cleanupNetworking(*inst); err != nil {
				return err
			}
			inst.FirecrackerPID = 0
			if inst.State != model.StateStopping && inst.State != model.StateDeleting {
				inst.State = model.StateStopped
				if inst.KindName() != model.KindOrb && !hasTailnetIdentity(*inst) {
					inst.State = model.StateFailed
				}
			}
		}
	default:
		return fmt.Errorf("%w: %s", errStartUncertain, result.Reason)
	}
	if observed.State == inst.State && observed.FirecrackerPID == inst.FirecrackerPID {
		return nil
	}
	inst.UpdatedAt = time.Now().UTC()
	return p.store.UpdateInstanceRuntime(ctx, observed, *inst)
}
