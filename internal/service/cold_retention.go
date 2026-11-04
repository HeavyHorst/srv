package service

import (
	"context"
	"time"

	"srv/internal/model"
)

// Transfers run separately from runtime reconciliation, one disk at a time.
func (a *App) runColdRetention(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		a.offloadExpiredDisks(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) offloadExpiredDisks(ctx context.Context, now time.Time) {
	if a.cfg.ColdStorageRetention <= 0 || a.cfg.ColdStorageHost == "" {
		return
	}
	instances, err := a.store.ListInstances(ctx, false)
	if err != nil {
		a.log.Warn("list cold retention candidates", "err", err)
		return
	}
	for _, inst := range instances {
		if ctx.Err() != nil {
			return
		}
		if inst.State != model.StateStopped {
			continue
		}
		lease, err := a.beginCommand()
		if err != nil {
			return // A snapshot owns the mutation barrier.
		}
		unlock := a.acquireInstance(inst.Name, false)
		if unlock == nil {
			lease.Release()
			continue
		}
		// Recheck the incarnation and stop time under the lifecycle lock; a
		// start/stop or name reuse since the inventory read must not evict early.
		since, err := a.store.StoppedSince(ctx, inst.ID)
		if err == nil && !since.IsZero() && !since.Add(a.cfg.ColdStorageRetention).After(now) {
			status, statusErr := a.provisioner.ColdDisk(ctx, inst.Name, "status")
			err = statusErr
			if err == nil && (status.Phase == "hot" || status.Phase == "purged") {
				_, err = a.provisioner.ColdDisk(ctx, inst.Name, "offload")
				if err == nil {
					a.log.Info("cold retention offloaded disk", "name", inst.Name, "stopped_since", since)
				}
			}
		}
		unlock()
		lease.Release()
		if err != nil && ctx.Err() == nil {
			a.log.Warn("cold retention offload failed", "name", inst.Name, "err", err)
		}
	}
}
