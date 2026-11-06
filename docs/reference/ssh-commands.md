# SSH command reference

All srv commands are invoked through the SSH surface. The service treats SSH as command transport only — there is no shell session.

## Syntax

```bash
ssh srv <command> [args]
```

For machine-readable output, use `--json` with supported non-streaming commands such as `list`, `inspect`, `status`, `integration ...`, and `pool ...`. With OpenSSH, terminate local option parsing first:

```bash
ssh srv -- --json list
ssh srv -- --json inspect demo
ssh srv -- --json status
```

## Instance commands

| Command | Description |
|---------|-------------|
| `new <name>` | Create a new VM |
| `new <name> --image <name>` | Select a configured guest image, e.g. `debian-13` (default: `arch`) |
| `new <name> --image <name> --template <id>` | Clone an owner-scoped prepared project template; requires explicit `--host` through srv-front |
| `new <name> --integration <name>` | Create and enable one or more existing integrations (admin only) |
| `new <name> --cpus <n>` | Create with custom vCPU count |
| `new <name> --ram <size>` | Create with custom memory (`2G`, `512M`, or MiB integer) |
| `new <name> --pool <name> --ram <size>` | Create a pooled-memory VM in an existing memory pool |
| `new <name> --rootfs-size <size>` | Create with custom rootfs size |
| `list` | Show visible VMs (all for admins, own for regular users) |
| `orb list` | Show visible Orbs only |
| `list --all` | Show both workload kinds, still subject to ownership rules |
| `orb new <name> --image <name>` | Create an Orb with a prepared Orb image; uses host vsock instead of guest Tailscale enrollment |
| `inspect <name>` | Show VM details and event history |
| `logs <name>` | View serial log (default) |
| `logs <name> serial` | View serial log |
| `logs <name> firecracker` | View Firecracker VMM log |
| `logs -f <name> [serial\|firecracker]` | Follow log output |
| `top [--interval <duration>]` | Watch live per-VM CPU, memory, disk, and network usage; run with `ssh -t` and press `q` to exit |

## Lifecycle commands

Lifecycle commands work for both VMs and Orbs. Names share one namespace per
backend; the image name does not determine the workload kind. Existing instances
remain VMs. See [Amp orbs](../examples/amp-orbs.md) for bootstrap and optional
project networking. `orb bootstrap <name>` is a streaming provider command:
credentials are JSON on stdin, never command arguments; it does not accept `--json`.

| Command | Description |
|---------|-------------|
| `start <name>` | Start a stopped instance, or recheck readiness in `awaiting-tailnet` / `awaiting-guest` |
| `stop <name>` | Stop a running VM (graceful shutdown) |
| `restart <name>` | Stop and start a VM |
| `fsck <name>` | Force-check and repair the rootfs of a stopped VM |
| `delete <name>` | Delete an instance's runtime, network, and local/cold disk allocation |
| `delete <name> --expect REQUEST_ID UUID CREATED_AT BACKEND_ID IMAGE` | Delete the exact expected incarnation; return its durable JSON cleanup receipt |
| `cleanup-receipt REQUEST_ID` | Read the stored cleanup receipt without executing deletion |
| `cleanup-receipt --instance UUID` | Read the deleted incarnation's receipt as its original owner or an administrator, without knowing the deletion request ID |

Deletion uses the same durable workflow for VMs and Orbs. It drains gateways,
stops the runtime, removes any remote cold disk without hydration, and only marks
the instance deleted after cleanup succeeds. Once cleanup has begun, a failure
leaves it `deleting`; background reconciliation or another `delete` resumes the
same request. A preflight rejection need not change the instance's state. All backups
(including legacy Orb backups), project templates and shared host snapshots are
retained. A fresh `delete NAME` selects the current incarnation: automation replaying
an old request after name reuse must use `delete NAME --expect …` instead. Bound
deletion and receipt queries require an explicit `--host` through srv-front.
The former `delete-proof` command is no longer accepted; there is no alias.
The updated runner and network helper are required; drain old helper work during
the coordinated upgrade described in [Orb deletion](../examples/amp-orbs.md#authoritative-native-orb-deletion).

### Manual deletion

Here `srv-a` is a direct backend SSH endpoint. These commands actually delete the
active environment; substitute the intended instance name. No preceding `stop`
or `disk hydrate` is required. Cold storage holds the offloaded active disk, not
an independent backup, so deletion removes it too.

```bash
# Normal VM
ssh srv-a delete demo

# Orb: the same command, not "orb delete"
ssh srv-a delete dev-orb
```

Successful text output for `demo`:

```text
deleted: demo
state: deleted
```

Repeat `delete demo` to resume an incomplete deletion. It reports an error if the
instance is already deleted. If the name has been reused, however, it selects the
new instance. Use bound deletion for delayed jobs and automated retries.

For an Amp-managed Orb, manual SSH deletion discards compute, not the Amp thread.
The next Amp `spawn` retrieves the exact instance's cleanup receipt and creates
fresh compute with a new name and UUID. No provider-side `retire` command is
needed or accepted. Pause without deletion still retains the workspace. See
[discard compute and continue the same thread](../examples/amp-orbs.md#discard-compute-and-continue-the-same-thread).

### Bound deletion and retries

`delete NAME --expect REQUEST_ID UUID CREATED_AT BACKEND_ID IMAGE` deletes only
the expected incarnation and returns a JSON cleanup receipt without needing
`--json`.

| Argument | Source and meaning |
|----------|--------------------|
| `NAME` | `.instance.name` from JSON `inspect` |
| `REQUEST_ID` | A new UUID for this deletion request; keep it unchanged on retry |
| `UUID` | `.instance.id`, the instance UUID, not the request UUID |
| `CREATED_AT` | `.instance.created_at`, preserving the returned timestamp |
| `BACKEND_ID` | `.instance.backend_id`, the backend SSH host-key fingerprint, not a frontend host name |
| `IMAGE` | `.instance.image` |

Prepare the request once, using local `jq` and `uuidgen`. Keep these files until
the operation is resolved. Do not overwrite them or repeat this preparation when
retrying: a fresh inspection could select a replacement instance.

```bash
ssh srv-a -- --json inspect demo > demo-before-delete.json &&
  uuidgen > demo-delete-request.txt
```

Execute the following Bash script to submit or retry the saved request. It stops
if the saved inputs cannot be read:

```bash
set -e
request=$(cat demo-delete-request.txt)
name=$(jq -er '.instance.name' demo-before-delete.json)
id=$(jq -er '.instance.id' demo-before-delete.json)
created=$(jq -er '.instance.created_at' demo-before-delete.json)
backend=$(jq -er '.instance.backend_id' demo-before-delete.json)
image=$(jq -er '.instance.image' demo-before-delete.json)

ssh srv-a delete "$name" --expect \
  "$request" "$id" "$created" "$backend" "$image"
```

Use the same authenticated user and exactly the same arguments on every retry.
A mismatched identity is rejected. An incomplete request resumes; a completed
request returns its stored receipt without deleting a later same-name instance.
This replay contract applies to deletion, not to all start/stop operations.

### Query completion after a lost reply

```bash
ssh srv-a cleanup-receipt "$(cat demo-delete-request.txt)"
```

This only reads the receipt; it does not execute deletion. A receipt establishes
completion. An error means no receipt could be retrieved, not that deletion had
no effects. If needed, resubmit the original bound request above.

For manual deletion, or when another administrator performed deletion, query by
the **original instance UUID saved before deletion** instead:

```bash
ssh srv-a cleanup-receipt --instance "$(jq -er '.instance.id' demo-before-delete.json)"
```

This lookup is available to the deleted instance's original owner and current
administrators, even after name reuse. It does not grant the new same-name
instance's owner access to the old receipt. Request-ID lookup still requires the
user who owns that deletion request. Neither lookup starts or retries deletion.

The backend's bound-deletion and receipt handlers return exit code 75 for handled
errors, including an uncommitted receipt or mismatched request. Do not interpret
that code as proof that nothing changed. Transport and frontend errors may use
other exit codes.

### Deletion through srv-front

Here `srv` is the frontend and `a` is its configured backend name. For preparation
through the frontend, replace the direct `inspect` command above with:

```bash
ssh srv -- --json inspect demo --host a > demo-before-delete.json &&
  uuidgen > demo-delete-request.txt
```

For manual deletion, submission using the saved variables above, or receipt lookup:

```bash
ssh srv delete demo --host a

ssh srv delete "$name" --host a --expect \
  "$request" "$id" "$created" "$backend" "$image"

ssh srv cleanup-receipt "$(cat demo-delete-request.txt)" --host a
ssh srv cleanup-receipt --instance "$id" --host a
```

These are alternative operations, not a sequence to run together. Bound deletion
and receipt lookup require explicit `--host`; `a` does not replace `BACKEND_ID`.
When using the bound frontend example, load the saved variables but replace the
direct `ssh srv-a delete` line with the frontend submission line.

## Cold disk commands

| Command | Description |
|---------|-------------|
| `disk status <name>` | Inspect local/remote disk residency without contacting remote storage |
| `disk offload <name>` | Upload and evict a stopped instance's disk after verified remote publication |
| `disk hydrate <name>` | Restore/recover a stopped disk without booting |
| `disk purge <name>` | Delete a retained remote generation; requires a valid local disk |

Supports `--json` and backend-qualified names through srv-front. Mutations on
native Orbs require admin access. `start`, migration preparation, export,
backup/restore, template creation, resize and fsck hydrate automatically when
needed. Offline commands still require a stopped instance; migration can stop
the source itself. Host snapshots still require explicit hydration and purge. See
[configuration, outage behavior and recovery](operations.md#optional-cold-disks-over-ssh).

## Resize command

| Command | Description |
|---------|-------------|
| `resize <name> --cpus <n>` | Resize vCPUs on a stopped VM |
| `resize <name> --ram <size>` | Resize memory on a stopped VM |
| `resize <name> --pool <pool>` | Assign a stopped VM or Orb to an existing memory pool, or switch pools |
| `resize <name> --fixed-memory` | Remove a stopped VM or Orb from its memory pool |
| `resize <name> --rootfs-size <size>` | Resize rootfs (stopped VM, grow-only) |

All flags can be combined except `--pool` and `--fixed-memory`, which are mutually exclusive. Omitted flags keep the current value.

For pooled VMs, `resize --ram` keeps the VM in the same pool unless `--pool` or `--fixed-memory` is also supplied. The VM or Orb must be stopped, and the final guest-visible RAM must fit within the target pool's reservation when using a pool. Both `resize --pool` and `resize --fixed-memory` without `--ram` preserve the current RAM size. Fixed-memory host capacity is checked on the next start; leaving a pool does not delete it or release its reservation.

## Backup commands

| Command | Description |
|---------|-------------|
| `backup create <name>` | Create a backup from a stopped VM |
| `backup list <name>` | List backups for a VM |
| `backup delete <name> <backup-id>` | Permanently delete a backup |
| `restore <name> <backup-id>` | Restore a stopped VM from a backup |

Backup and restore commands support normal VMs only. Orbs reject all four
commands, including for admins. Old Orb backups remain untouched even when the Orb
is deleted; clean them up manually if desired. Project templates remain supported.

## Project templates

| Command | Description |
|---------|-------------|
| `project-template create <name> <id>` | Save an immutable, identity-cleaned copy of your stopped VM |
| `project-template verify <name> <id>` | Verify an existing template belongs to the stopped source; never recreate a missing template |
| `project-template delete <id>` | Delete a template in your owner namespace; does not delete VMs created from it |

These commands support `--json` and require explicit `--host` through srv-front.
Unlike backups, templates create new identities rather than restore an existing
VM. Templates outlive their source VM. See [Amp orbs](../examples/amp-orbs.md#project-snapshots)
for storage, security and retention requirements.

## Transfer commands

| Command | Description |
|---------|-------------|
| `export <name>` | Stream a stopped VM as a tar artifact to stdout |
| `import` | Read a tar artifact from stdin and recreate the VM |
| `import --pool <name>` | Import into an existing destination memory pool |
| `import --fixed-memory` | Explicitly import with fixed memory, including pooled exports |

Usage: `ssh srv-a export demo | ssh srv-b import`

Pooled exports require an explicit destination memory policy. For coordinated
cold migration, host-qualified names, and `node list` across servers, see the
[multi-host frontend](../getting-started/multi-host.md).

## Admin and host commands

| Command | Description |
|---------|-------------|
| `pool create <name> --size <size>` | Admin-only create a reserved memory pool |
| `pool resize <name> --size <size>` | Admin-only shrink or grow a memory pool live, with usage checks before shrinking |
| `pool list` | Admin-only list reserved memory pools |
| `pool inspect <name>` | Admin-only show a memory pool and its member VMs |
| `pool delete <name>` | Admin-only delete an empty memory pool |
| `status` | Admin-only host capacity and allocation summary |
| `snapshot create` | Admin-only host-local [readonly root-and-disk snapshot bundle](../tasks/host-snapshots.md) with a manifest |

`pool resize` sets the total reservation, not a size delta. The size must be
positive and at least as large as each member's configured guest RAM, not the
sum of their RAM. Growing also requires enough host reservation budget.
Shrinking releases reservation budget without changing VM RAM or membership.

Members may be running (`ready`) or stopped. Retry after any start, stop,
failure cleanup, or migration has finished. Before lowering a live cgroup limit,
srv requires the target to exceed its current usage by at least 256 MiB or 10%
of the target, whichever is larger. An unused pool with zero usage needs no
headroom. Missing or invalid usage readings reject the shrink. There is no
force option and no forced reclaim or balloon inflation.

The check is a snapshot: concurrent allocations and later workload growth can
still reach the new hard limit and trigger the OOM killer. A successful resize
does not guarantee that future workloads fit.

Growth reserves host budget before raising the live limit; shrinking releases
budget only after the runner durably confirms the lower limit. If a request's
outcome is unknown, srv keeps the reservation and a persistent pending operation.
Other resize targets, pool deletion, and new VM starts or assignments into that
pool are blocked until the outcome is resolved. Retry the same resize command
and target size, including after a control-plane restart.

Even a confirmed rejection of growth keeps the increased reservation: a runner
error does not always prove that the limit was unchanged. After fixing the
runner error, resize to the desired size again to apply it or recover the budget.

The runner stores operation receipts in `instances/.pool-resizes/`, including
rejections. Retries return the recorded result instead of changing the limit
again. Receipts survive pool deletion and runner restarts; do not remove them
while the host is in use. A confirmed rejection clears the pending operation,
so a later command can try again after usage falls. Repeating the current size
with no pending operation synchronizes the live limit. Both the control plane
and VM runner need the updated binary and the `/pool/resize` protocol.

## Integration commands

All integration commands are admin-only.

| Command | Description |
|---------|-------------|
| `integration list` | List configured integrations |
| `integration inspect <name>` | Show integration target, auth mode, header references, and timestamps |
| `integration add http <name> --target <url>` | Create an HTTP integration |
| `integration add http <name> --target <url> --header NAME:VALUE` | Add a static upstream header |
| `integration add http <name> --target <url> --header-env NAME:SRV_SECRET_FOO` | Add an env-backed upstream header |
| `integration add http <name> --target <url> --bearer-env SRV_SECRET_FOO` | Inject bearer auth from a host env var |
| `integration add http <name> --target <url> --basic-user USER --basic-password-env SRV_SECRET_BAR` | Inject basic auth from host-managed credentials |
| `integration delete <name>` | Delete an integration that is no longer enabled on any VM |
| `integration enable <vm> <name>` | Enable an integration for a VM |
| `integration disable <vm> <name>` | Disable an integration for a VM |
| `integration list-enabled <vm>` | List integrations currently enabled for a VM |

## Notes

- `new` accepts `--cpus`, `--ram`, `--pool`, and `--rootfs-size` in any combination that matches the memory mode contract
- `new --pool <name>` requires `--ram <size>` because guest-visible RAM stays explicit in pooled mode
- `new` also accepts repeated `--integration <name>` flags; the create request fails if any requested integration cannot be enabled
- Fixed mode remains the default. Without `--pool`, `--ram` means guest-visible RAM; the hard per-VM host cgroup cap is that RAM plus `max(256 MiB, RAM/16)` Firecracker overhead reserve
- In pooled mode, `--ram` means guest-visible RAM while host reservation comes from the pool created with `pool create`
- `resize` requires the VM to be stopped; CPU and RAM may increase or decrease within limits, while rootfs is grow-only
- `resize --pool <name>` moves a fixed-memory or pooled VM or Orb into an existing pool; `resize --fixed-memory` switches back to fixed memory
- `resize`, `fsck`, `backup`, and `restore` all require the VM to be stopped
- Backups are tied to the original VM record — they cannot be restored onto a different VM
- Export requires the source VM to be stopped
- Import recreates the VM under the same name and leaves it stopped
- `top` refreshes continuously by default; use `ssh -t srv top --interval 2s` or similar to slow the redraw rate
- `inspect` shows `memory-mode` and whether host reservation is dedicated or shared via a pool
- `status` memory accounting now splits fixed reservations from pool reservations so pooled member VMs are not double-counted
- `top` shows live/configured RAM for every VM and appends the pool name for pooled rows
- Integration targets are intentionally narrow in v1: HTTP only, operator-managed, no guest-supplied raw secrets, and no automatic outbound interception
