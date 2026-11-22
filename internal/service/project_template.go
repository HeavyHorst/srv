package service

import (
	"context"
	"errors"
	"strings"

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
	} else if len(args) == 4 && args[1] == "identify" {
		err = a.provisioner.CheckTemplateSource(args[2], args[3], actor)
		if err == nil {
			return jsonResult(map[string]string{"action": "identify", "template": args[2], "owner": actor.UserLogin, "backend_id": a.backendID})
		}
	} else if (len(args) == 3 || len(args) == 9 && args[3] == "--expect-owner" && args[5] == "--expect-source" && args[7] == "--expect-backend") && args[1] == "delete" {
		a.mu.Lock()
		defer a.mu.Unlock()
		if len(args) == 3 {
			err = a.provisioner.DeleteTemplate(ctx, args[2], actor)
		} else if strings.TrimSpace(args[4]) == "" || !strings.EqualFold(args[4], actor.UserLogin) {
			err = errors.New("authenticated template owner does not match --expect-owner")
		} else if strings.TrimSpace(args[6]) == "" {
			err = errors.New("--expect-source requires the source instance ID")
		} else if args[8] == "" || args[8] != a.backendID {
			err = errors.New("backend ID does not match --expect-backend")
		} else if err = a.provisioner.DeleteTemplateFromSource(ctx, args[2], args[6], actor); err == nil {
			return jsonResult(map[string]string{"action": "delete", "template": args[2], "owner": actor.UserLogin, "backend_id": a.backendID})
		}
	} else {
		err = errors.New("usage: project-template create|verify NAME ID | project-template identify ID SOURCE_ID | project-template delete ID [--expect-owner OWNER --expect-source SOURCE_ID --expect-backend BACKEND_ID]")
	}
	if err != nil {
		return commandResult{stderr: err.Error() + "\n", exitCode: 1}, err
	}
	return jsonResult(map[string]string{"action": args[1], "template": args[len(args)-1]})
}
