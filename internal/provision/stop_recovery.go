package provision

import (
	"context"
	"errors"
	"time"

	"srv/internal/model"
	"srv/internal/vmrunner"
)

// ReconcileSavedStop is called under the service instance lock. The runner
// closes only this exact attempt and verifies snapshot identity and files.
func (p *Provisioner) ReconcileSavedStop(ctx context.Context, name string, expected vmrunner.StartResultRequest) (model.Instance, error) {
	inst, err := p.store.GetInstance(ctx, name)
	if err != nil {
		return inst, err
	}
	if expected.Validate() != nil || expected.Name != inst.Name || expected.InstanceID != inst.ID || expected.RequestID != inst.StartRequestID || expected.HostBootID != inst.StartBootID {
		return inst, errors.New("stop recovery attempt differs")
	}
	if inst.KindName() != model.KindOrb || (inst.State != model.StateStopping && inst.State != model.StateStopped) {
		return inst, errors.New("stop recovery requires a stopping or stopped orb")
	}
	if p.vmRunner == nil {
		return inst, errors.New("VM runner is unavailable")
	}
	if inst.MemoryMiB <= 0 || inst.MemoryMiB > (1<<63-1)/(1<<20) {
		return inst, errors.New("invalid guest memory size")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := p.vmRunner.StopInstanceVM(stopCtx, vmrunner.StopRequest{Name: inst.Name, InstanceID: inst.ID, RequestID: expected.RequestID, HostBootID: expected.HostBootID, SaveState: true, RequireSavedState: true, ExpectedMemoryBytes: inst.MemoryMiB << 20, PID: inst.FirecrackerPID}); err != nil {
		return inst, err
	}
	return p.finishStop(inst)
}
