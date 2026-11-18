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
	if !inst.GuestTailnet() {
		return model.StateAwaitingGuest
	}
	return model.StateAwaitingTailnet
}

// waitForHostOnlyReadiness completes readiness of guests outside the tailnet.
func (p *Provisioner) waitForHostOnlyReadiness(ctx context.Context, inst model.Instance) (model.Instance, error) {
	if inst.KindName() == model.KindOrb {
		return p.waitForOrb(ctx, inst)
	}
	return p.waitForBootReport(ctx, inst)
}

// A non-tailnet VM is ready once its bootstrap reports this attempt's nonce on
// the serial console. This attests boot progress of this incarnation, not SSH
// or application health; the reported SSH host key is recorded for operators.
func (p *Provisioner) waitForBootReport(ctx context.Context, inst model.Instance) (model.Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.GuestReadyTimeout)
	defer cancel()
	runtime, err := p.instanceRuntimeIdentity(ctx, inst)
	if err != nil {
		return inst, err
	}
	for {
		report, err := bootReport(inst.SerialLogPath, inst.StartRequestID)
		if err == nil {
			current, runtimeErr := p.instanceRuntimeIdentity(ctx, inst)
			if runtimeErr != nil || current != runtime {
				return inst, errors.Join(errGuestExited, runtimeErr)
			}
			updated := inst
			updated.State, updated.LastError = model.StateReady, ""
			updated.UpdatedAt = time.Now().UTC()
			if err := p.store.UpdateInstanceRuntime(ctx, inst, updated); err != nil {
				return inst, err
			}
			p.recordEvent(inst.ID, "boot", "guest bootstrap reported ready; ssh host key "+report.SSHHostKey, map[string]any{"guest_ip": inst.GuestAddr, "ssh_host_key": report.SSHHostKey})
			return updated, nil
		}
		if errors.Is(err, errBootFailed) {
			return p.recordBootReportFailure(inst, errors.Join(errGuestNotReady, err))
		}
		select {
		case <-ctx.Done():
			return p.recordBootReportFailure(inst, errors.Join(errGuestNotReady, ctx.Err(), err))
		case <-time.After(time.Second):
		}
	}
}

// recordBootReportFailure keeps the guest running, like the tailnet readiness
// path, but makes the failure visible in inspect.
func (p *Provisioner) recordBootReportFailure(inst model.Instance, err error) (model.Instance, error) {
	updated := inst
	updated.LastError = err.Error()
	updated.UpdatedAt = time.Now().UTC()
	persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if persistErr := p.store.UpdateInstanceRuntime(persistCtx, inst, updated); persistErr != nil {
		return inst, errors.Join(err, persistErr)
	}
	p.recordEvent(inst.ID, "boot", "guest boot readiness failed", map[string]any{"error": err.Error()})
	return updated, err
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
