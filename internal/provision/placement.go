package provision

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"

	"srv/internal/format"
	"srv/internal/model"
	"srv/internal/storage"
	"srv/internal/vmrunner"
)

// Placement is a read-only admission estimate, not a reservation. Create still
// checks capacity under admissionMu before allocating any resources.
type Placement struct {
	Eligible      bool   `json:"eligible"`
	Reason        string `json:"reason,omitempty"`
	MemoryLeftMiB int64  `json:"memory_left_mib"`
	DiskLeftBytes int64  `json:"disk_left_bytes"`
}

func (p *Provisioner) Placement(ctx context.Context, name string, opts CreateOptions) (result Placement) {
	return p.placement(ctx, name, opts, false)
}

// ImportPlacement checks the incoming disk, not the destination rootfs template.
// The local kernel/initrd must still exist; compatibility is operator-managed.
// This preflight also fronts native transfers, but its inputs do not identify
// incremental stream bytes. Estimate conservatively as a portable rootfs plus
// both bounded logs; the receiver must still measure its actual write budget.
func (p *Provisioner) ImportPlacement(ctx context.Context, name string, opts CreateOptions) Placement {
	return p.placement(ctx, name, opts, true)
}

func (p *Provisioner) placement(ctx context.Context, name string, opts CreateOptions, importing bool) (result Placement) {
	err := func() error {
		if !validName.MatchString(name) {
			return fmt.Errorf("invalid instance name %q", name)
		}
		if inst, exists, err := p.store.FindInstance(ctx, name); err != nil {
			return err
		} else if exists {
			if inst.State != model.StateDeleted {
				return fmt.Errorf("instance name %q is already in use", name)
			}
			migration, err := p.store.GetMigration(ctx, inst.ID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil && migration.Phase != "completed" && migration.Phase != "aborted" {
				return fmt.Errorf("instance %q is fenced by migration (%s)", name, migration.Phase)
			}
		}
		image, err := p.cfg.ResolveImage(opts.Image)
		if err != nil {
			return err
		}
		paths := []string{image.Kernel}
		if !importing {
			paths = append(paths, image.RootFS)
		}
		for _, path := range paths {
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("image unavailable: %w", err)
			}
		}
		if image.Initrd != "" {
			if _, err := os.Stat(image.Initrd); err != nil {
				return fmt.Errorf("image unavailable: %w", err)
			}
		}
		var baseSize int64
		if importing {
			if opts.RootFSSizeBytes <= 0 || opts.VCPUCount <= 0 || opts.MemoryMiB <= 0 {
				return fmt.Errorf("import placement requires an explicit positive disk, CPU and RAM shape")
			}
		} else {
			baseSize, err = p.rootFSSize(image.RootFS)
			if err != nil {
				return err
			}
		}
		resolved, _, err := p.resolveCreateOptions(opts, baseSize)
		if err != nil {
			return err
		}
		if resolved.MemoryPoolName != "" {
			pool, err := p.requireMemoryPoolByName(ctx, resolved.MemoryPoolName)
			if err != nil {
				return err
			}
			if err := ensureMemoryPoolSupportsGuestMemory(pool, resolved.MemoryMiB); err != nil {
				return err
			}
		} else if err := p.ensureHostMemoryCapacity(ctx, "", resolved.MemoryMiB); err != nil {
			return err
		}
		if err := p.ensureHostDiskCapacity(ctx, "", resolved.RootFSSizeBytes); err != nil {
			return err
		}
		summary, err := p.CapacitySummary(ctx)
		if err != nil {
			return err
		}
		for _, resource := range summary.Capacity {
			switch resource.Resource {
			case "memory":
				left := resource.Left
				if resolved.MemoryPoolName == "" {
					left -= vmrunner.FixedMemoryLimitBytes(resolved.MemoryMiB)
				}
				result.MemoryLeftMiB = left / format.MiB
			case "disk":
				if resource.Filesystem == nil {
					return errors.New("disk placement requires measured filesystem availability")
				}
				var payload int64
				if importing {
					if resolved.RootFSSizeBytes > math.MaxInt64-2*portableLogMaxSize {
						return errors.New("import disk payload estimate overflows")
					}
					payload = resolved.RootFSSizeBytes + 2*portableLogMaxSize
				}
				if err := storage.CheckAvailableSpace(resource.Filesystem.Available, payload); err != nil {
					return err
				}
				result.DiskLeftBytes = resource.Left - payload
			}
		}
		return nil
	}()
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	result.Eligible = true
	return result
}
