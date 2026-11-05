package service

import (
	"context"
	"errors"
	"fmt"

	"srv/internal/model"
)

func (a *App) cmdUpgrade(ctx context.Context, actor model.Actor, args []string, outFormat outputFormat) (commandResult, error) {
	if len(args) != 2 && !(len(args) == 3 && args[2] == "--rollback") {
		err := errors.New("usage: upgrade <name> [--rollback] (VM must be stopped; rollback restores all pre-upgrade disk data)")
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}
	name := args[1]
	unlock := a.lockInstance(name)
	defer unlock()
	if _, err := a.lookupVisibleInstance(ctx, actor, name); err != nil {
		return missingInstanceResult("upgrade", name, err)
	}
	rollback := len(args) == 3
	inst, err := a.provisioner.UpgradeKernel(ctx, name, rollback)
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("upgrade %s: %v\n", name, err), exitCode: 1}, err
	}
	action := "upgraded"
	if rollback {
		action = "rolled-back"
	}
	if outFormat == outputFormatJSON {
		return jsonResult(commandActionJSON{Action: action, Instance: instanceSummaryPayload(a.cfg, inst, false)})
	}
	return commandResult{stdout: fmt.Sprintf("%s: %s\nstate: stopped\nkernel: %s\nrollback restores the entire pre-upgrade disk, including guest data\n", action, name, inst.KernelPath)}, nil
}
