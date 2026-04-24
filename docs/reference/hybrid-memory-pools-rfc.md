# RFC: Hybrid Memory Pools

Status: proposed

This document describes a proposed hybrid memory model for `srv`. It is analysis only and does not describe shipped behavior.

The goal is to add an explicit pooled-memory mode without changing the existing fixed-memory contract that `srv` already provides.

## Summary

`srv` should keep fixed memory as the default and add an opt-in pooled mode backed by explicit reserved memory pools.

- Fixed mode stays unchanged. A VM's requested RAM is guest-visible RAM, the host-side reservation, and the hard per-VM cap.
- Pooled mode is explicit. A VM's requested RAM stays guest-visible, but host backing comes from a separately reserved pool.
- Creating a pool reserves memory immediately from host capacity.
- Pooled member VMs do not consume host memory budget a second time.
- CPU behavior stays as-is. This RFC is about memory, not CPU scheduling.

The intended split is:

- Fixed mode for predictable workloads such as databases, JVMs, and services that need a strong memory contract.
- Pooled mode for agents, cron jobs, preview apps, helpers, and other mostly idle or bursty workloads.

## Motivation

Today `srv` already overcommits CPU in practice, but memory is treated as a hard reservation and hard limit.

That is a good default for predictable isolation, but it leaves density on the table for workloads that are mostly idle and only occasionally need their full guest-visible RAM.

The proposed pool model is a hybrid approach:

- keep the existing fixed model intact
- add a second, explicit memory-sharing model
- make the softer contract obvious in the CLI, status views, and docs

This avoids weakening the current product contract for existing workloads while still enabling higher density for the right class of VM.

## Current State

Today one value, `memory_mib`, effectively drives three different behaviors:

- guest-visible Firecracker RAM
- per-VM cgroup memory limit
- host admission and capacity accounting

That current coupling creates a strong and easy-to-understand contract, but it means `srv` has no way to distinguish:

- how much RAM the guest can see
- how much memory the host has reserved for that VM
- how much memory a shared pool has reserved for a set of VMs

The design work in this RFC is mostly about splitting those meanings cleanly while preserving fixed mode.

## Goals

- Keep fixed mode as the default and preserve its current semantics.
- Add an explicit pool abstraction for reserved host memory.
- Allow pooled VMs to have guest-visible RAM larger than their individually dedicated host reservation.
- Make pool reservations visible in admission control and `status` output.
- Avoid double-counting pooled member VMs against host memory budget.
- Use Firecracker ballooning to make pooled memory technically meaningful rather than just optimistic accounting.

## Non-Goals

- No pool resize in v1.
- No moving VMs between pools in v1.
- No automatic conversion of existing fixed VMs into pooled VMs in v1.
- No CPU pool abstraction.
- No attempt to replace fixed mode as the default.

## Proposed Product Shape

### Fixed Mode

Fixed mode remains the default.

In fixed mode:

- `--ram` means guest-visible RAM
- `--ram` also means host-reserved RAM
- `--ram` also remains the hard per-VM cgroup cap

This is the current `srv` contract and should remain unchanged.

### Pooled Mode

Pooled mode is opt-in.

In pooled mode:

- `--ram` means guest-visible RAM
- host-reserved RAM comes from an explicit pool
- the hard host reservation is attached to the pool, not summed again for each pooled VM
- the runtime contract is elastic rather than strictly dedicated

This mode should be documented as a softer contract than fixed mode.

## CLI Shape

The intended command shape is:

```bash
ssh srv pool create pool1 --size 8GiB
ssh srv pool list
ssh srv pool inspect pool1
ssh srv pool delete pool1

ssh srv new agent-01 --pool pool1 --ram 2GiB
```

Design rules:

- `new` without `--pool` keeps using fixed mode.
- `new --pool <name>` still requires `--ram` because guest-visible RAM must stay explicit.
- `pool delete` should fail while the pool still has member VMs.
- `resize` may change guest-visible RAM for a pooled VM only while the VM is stopped.
- `resize` should not support changing memory mode or pool assignment in v1.

## Schema And Model Changes

### Instance Model

Keep `memory_mib` as the guest-visible memory field to minimize churn.

Add fields to the instance model for:

- memory mode, such as `fixed` or `pool`
- pool association, ideally by pool ID

Recommended semantics:

- `memory_mib` always means guest-visible RAM
- fixed instances derive host reservation from `memory_mib`
- pooled instances derive host reservation from their pool, not from `memory_mib`

This preserves backward compatibility for existing instance rows while giving the runtime and admission paths enough information to split behavior.

### Pool Model

Add a new pool object with only the fields needed for v1:

- ID
- name
- reserved size in bytes
- created and updated timestamps
- creator identity metadata

Avoid policy-heavy fields in v1. The first version should be explicit and simple.

### SQLite Changes

Add a `memory_pools` table.

Add new instance columns:

- `memory_mode`
- `memory_pool_id`

Migration strategy:

- existing instances default to `fixed`
- existing `memory_mib` values remain valid as-is
- changes stay additive

## Admission And Accounting

### Host Memory Admission

Host memory accounting should change from:

- sum of currently reserved per-VM memory

to:

- sum of fixed-instance reserved memory
- plus sum of reserved pool memory

Creating a pool should reserve host memory immediately, even if it has no members yet.

That matches the intended product contract: a pool is guaranteed capacity, not just a label.

### Pooled VM Admission

Creating or starting a pooled VM should:

- validate that the referenced pool exists
- validate that the instance belongs to pooled mode
- avoid charging host memory budget again for that VM

For v1, add one simple guardrail:

- reject a pooled VM whose guest-visible RAM exceeds the total reserved size of the pool

That does not solve every unsafe shape, but it prevents obviously unreasonable configurations.

The same guardrail should apply to stopped-only guest-visible RAM resize for pooled VMs. A pooled VM may change its guest-visible RAM within the existing pool, but the resize must not change host reservation accounting and must still fit within the pool's reserved size.

### Status Reporting

`status` should keep a top-level memory line, but the details should distinguish:

- fixed reserved memory
- pool reserved memory
- host reserve

Example shape:

```text
memory       40/63.5 GiB [63%] - 512 MiB host reserve
             fixed: 8 GiB
             pools: 32 GiB across 1 pool
```

The important point is that pooled members should not inflate that number again.

## Runtime And Cgroup Layout

### Fixed VMs

Keep the current layout for fixed VMs:

- each VM gets its own cgroup leaf
- `memory.max` stays equal to the VM's guest-visible RAM

This preserves the current fixed guarantee.

### Pooled VMs

Pooled VMs need a hierarchical cgroup layout.

Recommended shape:

- `firecracker-pools/<pool>` for the pool parent
- `firecracker-pools/<pool>/<vm>` for each pooled member

Recommended policy:

- the pool parent holds the hard memory reservation with `memory.max = pool size`
- child VM cgroups remain useful for process placement, metrics, CPU limits, and pids limits
- pooled member VMs should not each carry a separate hard memory reservation equal to their guest-visible RAM

That gives the system one hard host-side budget per pool while still retaining per-VM accounting and control.

## Firecracker Balloon Integration

Pooled mode is only credible if it uses ballooning.

Without ballooning and free-page reporting, pooled mode would mostly be accounting plus risky overcommit.

### Boot-Time Configuration

For pooled VMs:

- keep Firecracker machine memory set to the guest-visible RAM
- attach a balloon device before boot
- enable free-page reporting
- enable balloon statistics collection

For fixed VMs:

- keep the current path unchanged in v1

### Runtime Behavior

The system then needs a conservative control loop that can:

- observe resident memory usage
- observe balloon statistics where available
- reclaim slack from idle pooled guests
- avoid aggressive oscillation

The v1 goal is not a sophisticated scheduler. It is a stable, understandable reclaim loop that makes a reserved pool actually shareable.

## Observability And UX

The main UX risk is misleading users into thinking pooled guest-visible RAM is the same as dedicated reserved RAM.

### Inspect

`inspect` should make the distinction explicit.

For pooled VMs, show fields like:

- memory mode
- guest-visible RAM
- pool name
- host reservation model: shared via pool

When a pooled VM's guest-visible RAM has been changed with `resize`, `inspect` should still make clear that the host reservation remains shared via the same pool rather than becoming a dedicated reservation.

For fixed VMs, show:

- memory mode
- guest-visible RAM
- host reservation model: dedicated

### Top

`top` should stop implying that every VM's configured RAM is a dedicated host guarantee.

For pooled VMs, it should show at least:

- current resident memory
- guest-visible RAM
- pool identity or pool pressure context

### JSON Output

JSON responses should also carry the distinction explicitly so automation can reason about it.

## Risks

- Balloon reclaim is best-effort and reactive.
- If many pooled VMs become memory-hot at once, the pool can hit its hard cap and trigger OOM pressure.
- Applications that auto-size based on visible RAM are poor candidates for pooled mode.
- Poor observability would make the feature dangerous because users could assume pooled VMs are fixed VMs.
- Recovery code must recreate pool parents before restarting pooled VMs after host reboot.

## Rollout Strategy

### Phase 1: Schema And Accounting

- add pool schema and instance metadata
- add pool CRUD commands
- reserve pool memory in host admission and status accounting
- leave pooled runtime launch guarded until the accounting model is correct

### Phase 2: Runtime Support

- add pooled cgroup hierarchy
- extend the VM runner start contract with pooled-mode inputs
- start pooled VMs with balloon devices enabled
- keep fixed VMs on the existing path

### Phase 3: Observability

- update `status`
- update `inspect`
- update JSON output
- update `top` to present pooled memory honestly

### Phase 4: Hardening

- tune reclaim heuristics
- validate reboot recovery
- document recommended and discouraged workload types

## Testing Strategy

### Unit Tests

- migration tests for the new pool schema and additive columns
- accounting tests proving pool reservation is charged immediately
- accounting tests proving pooled members are not double-counted
- command parsing tests for `pool` subcommands and `new --pool`

### Provisioner Tests

- create pool succeeds when capacity is available
- create pool fails when host memory budget would be exceeded
- pooled VM creation validates the pool and does not reserve host memory again
- stopped pooled VM guest-RAM resize succeeds when the new value fits within the pool reservation
- stopped pooled VM guest-RAM resize fails when the new value exceeds the pool reservation
- deleting a non-empty pool fails
- fixed-mode behavior remains unchanged

### VM Runner Tests

- fixed VMs keep the existing per-VM cgroup behavior
- pooled VMs land under the pool hierarchy
- pooled VMs enable balloon configuration before boot

### End-To-End Tests

- create a pool, create pooled VMs, start them, inspect them, and stop them
- verify `status` reflects pool reservation instead of double-counting members
- verify reboot recovery rebuilds pool parents before pooled VM restart
- verify `top` and `inspect` distinguish guest-visible memory from reserved host memory

## Recommended v1 Decisions

- fixed mode remains default and unchanged
- pooled mode is explicit and opt-in
- pool creation is the reservation event against host memory budget
- pooled VMs use guest-visible RAM for Firecracker machine size
- pooled VMs may change guest-visible RAM only while stopped and only within the existing pool
- pooled VMs rely on a reserved pool plus ballooning for host-side sharing
- v1 does not include pool resize, pool migration, or mode conversion

## Open Questions

- Should pooled mode expose any per-pool safety policy in v1, or stay limited to reserved size only?
- How much balloon and resident-memory detail should be exposed in `status` versus only `inspect` and `top`?

## Recommendation

Proceed with a memory-only hybrid design.

Keep fixed mode as the strong and predictable default. Add explicit reserved memory pools for dense, mostly idle workloads. Treat pooled mode as an elastic contract that must be clearly labeled as different from fixed reservations in every user-facing view.
