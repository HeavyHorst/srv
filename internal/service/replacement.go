package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"srv/internal/model"
)

// authorizeReplacement must be called with the per-instance lock held, before
// provisioning can remove the previous incarnation's files or change its record.
func (a *App) authorizeReplacement(ctx context.Context, actor model.Actor, name, artifactOwner string) error {
	if !a.isAdmin(actor) && artifactOwner != "" && !strings.EqualFold(artifactOwner, actor.UserLogin) {
		return fmt.Errorf("artifact owner %q does not match importing user", artifactOwner)
	}
	inst, err := a.store.GetInstance(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if inst.State != model.StateDeleted && !a.canAccessInstance(actor, inst) {
		return fmt.Errorf("instance %q may only be replaced by its owner or an admin", name)
	}
	return nil
}
