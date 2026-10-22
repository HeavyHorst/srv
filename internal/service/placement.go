package service

import (
	"context"
	"errors"

	"srv/internal/model"
	"srv/internal/provision"
)

// Placement exposes suitability and remaining admission budgets, but no other
// users' instances or full admin-only host status. Arguments follow new.
func (a *App) cmdPlacement(ctx context.Context, actor model.Actor, args []string) (commandResult, error) {
	name, opts, integrations, err := parseNewArgs(args)
	if err != nil {
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}
	if args[0] == "import-placement" {
		if len(integrations) > 0 {
			return jsonResult(provision.Placement{Reason: "import placement does not support integrations"})
		}
		return jsonResult(a.provisioner.ImportPlacement(ctx, name, opts))
	}
	if len(integrations) > 0 && !a.isAdmin(actor) {
		err := errors.New("new with --integration requires admin access")
		return commandResult{stderr: err.Error() + "\n", exitCode: 1}, err
	}
	if _, err := a.lookupIntegrationsByName(ctx, integrations); err != nil {
		return jsonResult(provision.Placement{Reason: err.Error()})
	}
	return jsonResult(a.provisioner.Placement(ctx, name, opts))
}
