package service

import (
	"context"
	"errors"

	"srv/internal/model"
)

// Templates have their own owner namespace, independent of instance names.
func (a *App) cmdProjectTemplate(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	var err error
	if len(args) == 4 && (args[1] == "create" || args[1] == "verify") {
		a.mu.Lock()
		defer a.mu.Unlock()
		unlock := a.lockInstance(args[2])
		defer unlock()
		if _, err = a.lookupVisibleInstance(ctx, actor, args[2]); err == nil {
			if args[1] == "verify" {
				err = a.provisioner.VerifyTemplate(ctx, args[2], args[3], actor)
			} else {
				err = a.provisioner.CreateTemplate(ctx, args[2], args[3], actor)
			}
		}
	} else if len(args) == 3 && args[1] == "delete" {
		a.mu.Lock()
		defer a.mu.Unlock()
		err = a.provisioner.DeleteTemplate(ctx, args[2], actor)
	} else {
		err = errors.New("usage: project-template create|verify NAME ID | project-template delete ID")
	}
	if err != nil {
		return commandResult{stderr: err.Error() + "\n", exitCode: 1}, err
	}
	return jsonResult(map[string]string{"action": args[1], "template": args[len(args)-1]})
}
