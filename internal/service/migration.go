package service

import (
	"context"
	"fmt"

	"srv/internal/model"
)

// cmdMigration takes the full command argv, including "migration". Responses
// are JSON regardless of output mode, for reconnect-safe frontend orchestration.
func (a *App) cmdMigration(ctx context.Context, actor model.Actor, args []string, _ outputFormat) (commandResult, error) {
	fail := func(err error) (commandResult, error) {
		return commandResult{stderr: err.Error() + "\n", exitCode: 1}, err
	}
	if len(args) < 3 {
		return fail(fmt.Errorf("usage: migration prepare NAME TARGET TOKEN | complete NAME TOKEN | status NAME | receipt NAME | abort NAME TOKEN --confirm-destination-destroyed"))
	}
	unlock := a.lockInstance(args[2])
	defer func() { unlock(); a.syncManagedGatewaysBestEffort() }()
	if _, err := a.lookupVisibleInstance(ctx, actor, args[2]); err != nil {
		return fail(err)
	}
	var m model.Migration
	var err error
	switch {
	case args[1] == "prepare" && len(args) >= 5:
		opts, parseErr := parseImportOptions(append([]string{"import"}, args[5:]...))
		if parseErr != nil {
			return fail(parseErr)
		}
		m, err = a.provisioner.PrepareMigration(ctx, args[2], args[3], args[4], opts)
	case args[1] == "complete" && len(args) == 4:
		m, err = a.provisioner.FinishMigration(ctx, args[2], args[3], false)
	case args[1] == "status" && len(args) == 3:
		m, err = a.provisioner.MigrationStatus(ctx, args[2])
	case args[1] == "receipt" && len(args) == 3:
		inst, lookupErr := a.lookupVisibleInstance(ctx, actor, args[2])
		if lookupErr != nil {
			return fail(lookupErr)
		}
		receipt, receiptErr := a.store.GetImportReceipt(ctx, inst.ID)
		if receiptErr != nil {
			return fail(receiptErr)
		}
		return jsonResult(receipt)
	case args[1] == "abort" && len(args) == 5 && args[4] == "--confirm-destination-destroyed":
		if !a.isAdmin(actor) {
			return fail(fmt.Errorf("migration recovery requires admin access"))
		}
		m, err = a.provisioner.FinishMigration(ctx, args[2], args[3], true)
	default:
		return fail(fmt.Errorf("invalid migration command; abort requires admin and --confirm-destination-destroyed (including all imported identity copies)"))
	}
	if err != nil {
		return fail(err)
	}
	return jsonResult(m)
}
