package service

import (
	"context"
	"errors"
	"fmt"

	"srv/internal/model"
)

func (a *App) cmdUpgrade(ctx context.Context, actor model.Actor, args []string, outFormat outputFormat) (commandResult, error) {
	if len(args) != 2 && !(len(args) == 3 && (args[2] == "--rollback" || args[2] == "--finalize")) {
		err := errors.New("usage: upgrade <name> [--rollback | --finalize] (upgrade/rollback require a stopped VM; finalize permanently discards rollback)")
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}
	name := args[1]
	unlock := a.lockInstance(name)
	defer unlock()
	if _, err := a.lookupVisibleInstance(ctx, actor, name); err != nil {
		return missingInstanceResult("upgrade", name, err)
	}
	rollback := len(args) == 3 && args[2] == "--rollback"
	finalize := len(args) == 3 && args[2] == "--finalize"
	var inst model.Instance
	var err error
	if finalize {
		inst, err = a.provisioner.FinalizeKernelUpgrade(ctx, name)
	} else {
		inst, err = a.provisioner.UpgradeKernel(ctx, name, rollback)
	}
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("upgrade %s: %v\n", name, err), exitCode: 1}, err
	}
	action := "upgraded"
	if rollback {
		action = "rolled-back"
	}
	if finalize {
		action = "finalized"
	}
	if outFormat == outputFormatJSON {
		return jsonResult(commandActionJSON{Action: action, Instance: instanceSummaryPayload(a.cfg, inst, false)})
	}
	if finalize {
		return commandResult{stdout: fmt.Sprintf("finalized: %s\nstate: %s\nrollback disk discarded; boot files retained\n", name, inst.State)}, nil
	}
	return commandResult{stdout: fmt.Sprintf("%s: %s\nstate: stopped\nkernel: %s\nrollback restores the entire pre-upgrade disk, including guest data\n", action, name, inst.KernelPath)}, nil
}
