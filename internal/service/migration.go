package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	gssh "github.com/gliderlabs/ssh"

	"srv/internal/model"
	"srv/internal/vmrunner"
)

// handleMigrationDisk is reachable only after trusted-node forwarding checks.
// The frontend refuses this command from users; only migrate constructs it.
func (a *App) handleMigrationDisk(sess gssh.Session, actor model.Actor, args []string) (int, error) {
	if len(args) >= 2 && args[1] == "import" {
		return a.handleImportSessionMode(sess, actor, append([]string{"import"}, args[2:]...), true)
	}
	if len(args) != 2 || len(args[1]) > 16384 {
		return 2, fmt.Errorf("invalid internal migration command")
	}
	data, err := base64.RawURLEncoding.DecodeString(args[1])
	var q vmrunner.MigrationDiskRequest
	if err != nil || json.Unmarshal(data, &q) != nil {
		return 2, fmt.Errorf("invalid migration request")
	}
	if err := rejectPTY(sess, "migration-disk"); err != nil {
		return 1, err
	}
	unlock := a.lockInstance(q.Name)
	defer unlock()
	if q.Action == "prepare" || q.Action == "send-base" || q.Action == "send-disk" || q.Action == "metadata" || q.Action == "cleanup" {
		if _, err := a.lookupVisibleInstance(sess.Context(), actor, q.Name); err != nil {
			return 1, err
		}
	} else if err := a.authorizeReplacement(sess.Context(), actor, q.Name, actor.UserLogin); err != nil {
		return 1, err
	}
	if q.Action == "metadata" {
		m, err := a.provisioner.MigrationStatus(sess.Context(), q.Name)
		if err != nil {
			return 1, err
		}
		if m.Phase != "prepared" || m.Token != q.Token {
			return 1, fmt.Errorf("migration token mismatch")
		}
		_, err = a.provisioner.ExportNativeMetadata(sess.Context(), q.Name, sess, q.Transfer)
		if err != nil {
			return 1, err
		}
		return 0, nil
	}
	var in io.Reader
	if q.Action == "receive-base" || q.Action == "receive-disk" {
		in = sess
	}
	var out io.Writer
	if q.Action == "send-base" || q.Action == "send-disk" {
		out = sess
	}
	result, err := a.provisioner.TransferMigrationDisk(sess.Context(), q, in, out)
	if err != nil {
		return 1, err
	}
	if out == nil {
		err = json.NewEncoder(sess).Encode(result)
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

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
