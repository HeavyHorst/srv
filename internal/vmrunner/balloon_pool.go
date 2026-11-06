package vmrunner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	balloonSettleTime = 10 * time.Second
	balloonStallTime  = 30 * time.Second
)

// Only the runner's balloon loop owns this volatile state. The pool mutex
// reserves commands before parallel HTTP requests, not across those requests.
// No amount here is a promise of reclaimed physical host memory.
type balloonPool struct {
	mu                 sync.Mutex
	identity           os.FileInfo
	limit              int64
	members            map[string]*balloonObservation
	inflated, returned int64 // MiB commanded this sweep
	saturated          bool
}

type balloonObservation struct {
	socket, cgroup    os.FileInfo
	target, actual    int64
	changed, progress time.Time
}

func (s *Server) prepareBalloonPools(vms []balloonVM) map[string]*balloonPool {
	pools := make(map[string]*balloonPool)
	for _, vm := range vms {
		if !vm.UseTarget {
			continue
		}
		p, exists := pools[vm.PoolPath]
		if !exists {
			info, err := os.Stat(vm.PoolPath)
			if err != nil {
				continue
			}
			limit, err := readInt64File(filepath.Join(vm.PoolPath, "memory.max"))
			if err != nil || limit <= 0 {
				s.log.Warn("skip balloon pool with invalid memory limit", "pool", vm.PoolPath, "err", err)
				continue
			}
			p = s.balloonPools[vm.PoolPath]
			if p == nil || !os.SameFile(p.identity, info) || p.limit != limit {
				p = &balloonPool{identity: info, limit: limit, members: make(map[string]*balloonObservation)}
			}
			p.inflated, p.returned = 0, 0
			pools[vm.PoolPath] = p
		}
	}
	// Retain only current members. A pool move or name reuse must not inherit a
	// previous guest's pending work; socket/cgroup identity is checked below too.
	for path, p := range pools {
		members := make(map[string]*balloonObservation)
		for _, vm := range vms {
			if vm.PoolPath == path {
				members[vm.Name] = p.members[vm.Name]
			}
		}
		p.members = members
	}
	s.balloonPools = pools
	return pools
}

// Called after the workers join. Report a transition, not one warning per VM
// every five seconds. Pending commands and their settling period are not
// evidence of saturation yet.
func (s *Server) reportBalloonPools(now time.Time) {
	for path, pool := range s.balloonPools {
		used, err := readInt64File(filepath.Join(path, "memory.current"))
		if err != nil || used < 0 {
			continue
		}
		waiting := false
		for _, obs := range pool.members {
			if obs != nil && (obs.target != obs.actual || now.Sub(obs.changed) < balloonSettleTime) {
				waiting = true
			}
		}
		saturated := used > pool.limit-pool.limit/5 && pool.inflated == 0 && !waiting
		if saturated && !pool.saturated {
			s.log.Warn("pool pressure without responsive safe balloon donors", "pool", path, "used_bytes", used, "limit_bytes", pool.limit)
		} else if !saturated && pool.saturated {
			s.log.Info("pool balloon pressure state cleared", "pool", path, "used_bytes", used, "limit_bytes", pool.limit)
		}
		pool.saturated = saturated
	}
}

func (s *Server) reconcilePoolBalloon(ctx context.Context, vm balloonVM, pool *balloonPool, now time.Time) error {
	if pool == nil {
		return nil
	}
	// Start, stop, save/resume and disk operations use this same name lock.
	// Keep it until PATCH completes so a plan cannot reach a replacement VM.
	unlock, err := s.lockNameContext(ctx, vm.Name)
	if err != nil {
		return err
	}
	defer unlock()
	socket, err := os.Stat(vm.SocketPath)
	if err != nil {
		return err
	}
	cgroup, err := os.Stat(vm.CgroupPath)
	if err != nil {
		return err
	}
	pool.mu.Lock()
	previous := pool.members[vm.Name]
	if previous == nil || !os.SameFile(previous.socket, socket) || !os.SameFile(previous.cgroup, cgroup) {
		pool.members[vm.Name] = nil
		s.closeFirecrackerClient(vm.SocketPath)
	}
	pool.mu.Unlock()
	stats, err := s.readBalloonStats(ctx, vm.SocketPath)
	if err != nil {
		return err
	}
	safe, step, ok := desiredBalloonTargetMiB(stats)
	if stats.TargetMiB == nil || stats.ActualMiB == nil || *stats.TargetMiB < 0 || *stats.ActualMiB < 0 {
		return nil
	}

	pool.mu.Lock()
	// Re-read the live limit and usage just before reserving a command. Pool
	// resize may race observation; a changed limit invalidates this round.
	limit, err := readInt64File(filepath.Join(vm.PoolPath, "memory.max"))
	if err != nil || limit != pool.limit {
		pool.mu.Unlock()
		return err
	}
	used, err := readInt64File(filepath.Join(vm.PoolPath, "memory.current"))
	if err != nil || used < 0 {
		pool.mu.Unlock()
		return fmt.Errorf("read balloon pool usage: value=%d error=%v", used, err)
	}
	obs := pool.members[vm.Name]
	actual, target := *stats.ActualMiB, *stats.TargetMiB
	fallen := false
	if obs == nil {
		obs = &balloonObservation{socket: socket, cgroup: cgroup, changed: now, progress: now, actual: actual, target: target}
		pool.members[vm.Name] = obs
	} else {
		// An unexpected decrease may be deflate_on_oom. Do not fight the guest
		// by asking it to reinflate to the old target immediately.
		fallen = actual < obs.actual && target >= obs.target
		if actual != obs.actual {
			obs.progress = now
			// Actual is live device state; available memory is polled separately.
			// Allow another stats interval after progress before granting more.
			obs.changed = now
		}
		if target != obs.target {
			obs.changed = now
			obs.progress = now
		}
		obs.actual, obs.target = actual, target
	}
	// Cancelling unfinished inflation needs only device state, not guest
	// headroom. Missing headroom still forbids new inflation or memory grants.
	cancelInflation := target > actual && (fallen || now.Sub(obs.progress) >= balloonStallTime)
	if !ok && !cancelInflation {
		pool.mu.Unlock()
		return nil
	}
	var pendingInflate, pendingReturn int64
	allObserved := true
	for _, member := range pool.members {
		if member == nil {
			allObserved = false
			continue
		}
		pendingInflate += max(member.target-member.actual, 0)
		pendingReturn += max(member.actual-member.target, 0)
	}
	limitMiB := bytesToMiBFloor(limit)
	roundBudget := min(balloonMaxTargetChangeMiB, limitMiB/20)
	// Small pools still make progress without exceeding their 5% command cap.
	step = min(step, max(int64(1), roundBudget))
	desired, reason := target, "hold"
	settled := actual == target && now.Sub(obs.changed) >= balloonSettleTime
	if cancelInflation {
		desired, reason = actual, "cancel stalled or guest-deflated inflation"
	} else if safe < target {
		// Cancelling outstanding inflation costs no grant. Actual deflation
		// does: reserve against measured space below 90%, not expected reclaim.
		grant := max(int64(0), bytesToMiBFloor(limit-limit/10-used)-pendingReturn)
		grant = min(grant, max(int64(0), roundBudget-pool.returned))
		if !allObserved || now.Sub(obs.changed) < balloonSettleTime || actual != target {
			grant = 0
		}
		grant = min(grant, max(actual-safe, 0))
		// A final sub-step return can satisfy a small deficit without exceeding
		// either the measured grant budget or the guest's requested headroom.
		if grant != actual-safe {
			grant = roundDownToMultiple(grant, step)
		}
		desired = min(target, actual-grant)
		pool.returned += grant
		reason = "guest headroom"
	} else if allObserved && settled && used > limit-limit/5 && safe > target {
		budget := max(int64(0), bytesToMiBFloor(used-(limit-limit/4))-pendingInflate)
		budget = min(budget, max(int64(0), roundBudget-pool.inflated))
		delta := roundDownToMultiple(min(budget, safe-target), step)
		desired += delta
		pool.inflated += delta
		reason = "pool pressure"
	}
	if desired == target {
		pool.mu.Unlock()
		return nil
	}
	// Reserve even on timeout: the API may have applied a PATCH whose response
	// was lost. The next observation resolves its actual outcome.
	obs.target, obs.changed, obs.progress = desired, now, now
	pool.mu.Unlock()
	s.log.Debug("pool balloon target", "pool", vm.PoolPath, "name", vm.Name, "reason", reason,
		"pool_used_bytes", used, "pool_limit_bytes", limit, "previous_target_mib", target,
		"actual_mib", actual, "target_mib", desired, "available_memory_bytes", stats.AvailableMemory)
	return s.patchBalloonTarget(ctx, vm.SocketPath, desired)
}
