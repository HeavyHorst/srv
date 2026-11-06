# Instance lifecycle

This page covers the full lifecycle of a srv VM from creation to deletion.

## Create

```bash
ssh srv new <name>
```

Arch is the default. To select a configured image at creation:

```bash
ssh srv new debian-vm --image debian-13
```

The image stays with the VM; `start` does not change its operating system.
See [image configuration](../reference/configuration.md#guest-artifacts) for the
host-side catalog and [the Debian builder](../../images/debian-base/README.md)
for the basic Debian 13 rootfs without Arch's optional developer/agent tools.

With custom sizing:

```bash
ssh srv new <name> --cpus <n> --ram <size> --rootfs-size <size>
```

`--ram` and `--rootfs-size` accept units like `2G`, `512M`, or plain MiB integers. `--cpus` must be 1 or an even number, up to 32.

The control plane:

1. Clones the base rootfs as a reflink
2. Allocates a `/30` network subnet and TAP device
3. Mints a one-off Tailscale auth key via MMDS
4. Boots the VM through the jailer with cgroup v2 enforcement
5. Polls until the guest reports `state: ready`

## Inspect

```bash
ssh srv inspect <name>
```

Shows instance state, vCPU count, memory, rootfs size, network addresses, Tailscale name and IP, and event history.

Machine-readable:

```bash
ssh srv -- --json inspect <name>
```

## List and status

```bash
ssh srv list
ssh srv status
```

`list` shows the VMs visible to the caller: all VMs for admins, or only the caller's own VMs for regular users. `status` is admin-only and reports host capacity — instance counts plus CPU, memory, and disk headroom.

The storage rows answer two different questions:

- `DISK RSV` is logical rootfs reservation headroom. This is the budget used to admit `new`, `start`, and rootfs-growing `resize` operations.
- `FS SPACE` is the filesystem's actual used and available space. It also reflects backups, reflink divergence, logs, and other files outside rootfs reservations; shared reflink extents count only once until they diverge.

Use `FS SPACE` to detect physical storage pressure even when the logical reservation budget still has room.

## Logs

```bash
ssh srv logs <name>
ssh srv logs <name> serial
ssh srv logs <name> firecracker
ssh srv logs -f <name> serial
ssh srv logs -f <name> firecracker
```

The serial log is append-only across boots. The Firecracker log is reset when the VMM starts so it only contains the current Firecracker process's output.

## Stop

```bash
ssh srv stop <name>
```

Performs a graceful shutdown through Firecracker. The rootfs is preserved on disk.

### Save RAM before stopping

For a VM or Orb that should continue its running processes after host maintenance:

```bash
ssh srv stop <name> --save-state
```

This pauses the guest, saves its RAM and device state, then terminates the VMM
without running guest shutdown. Wait for the command to succeed before restarting
the runner or host. Ordinary stop, restart, and runner shutdown do not create RAM
snapshots. The VM must have been started by a runner with RAM-state support.

There is one host-local saved state per instance, with no snapshot names or
history. The disk stays in place and must not change while RAM is saved. Resize,
fsck, kernel upgrades, restore, migration, and cold-disk offload are blocked until
the state is resumed or discarded. Deleting the instance removes its saved RAM.

Allow disk space for the guest's configured RAM plus at least 256 MiB. An error
after pausing can leave the guest paused; retry `stop <name> --save-state` after
fixing the error. Never interpret a failed save as permission to reboot the host.
Saving RAM requires the jailer and instance directories to share a filesystem
and subvolume that permits atomic rename between them. Unsupported layouts are
rejected before pausing or recording save intent. A repeated save request only
succeeds while that attempt's saved image still exists; an already consumed image
is not reported as a successful new save.

## Start

```bash
ssh srv start <name>
```

Boots a previously stopped VM. Stopped guests pick up the currently configured `SRV_BASE_KERNEL` and optional `SRV_BASE_INITRD` on their next start.

If saved RAM exists, start resumes it automatically. To discard it and boot fresh:

```bash
ssh srv start <name> --discard-state
```

Restore consumes the saved state before guest execution resumes, so a lost response
cannot replay the same execution state. A failed restore does not fall back to a
cold boot. Configuration or disk mismatches detected before restore leave the saved
state intact and require an explicit `--discard-state` to proceed. A cold start
with that flag removes the saved state as well.

Resume requires the same host, unchanged disk and VM configuration, and compatible
host software. Save records `--snapshot-version` from the producing executable
opened through `/proc/PID/exe`, even if the installed Firecracker was replaced
while the VM was running. Start uses the currently installed binary. Its snapshot
format must match exactly; a changed SHA256 is allowed, but no compatibility is
inferred from Firecracker release numbers or semver ranges. Old binaries are not
retained.

A different snapshot format causes start to delete the saved RAM and cold boot
with the existing disk. Older manifests without a format version receive the same
treatment, even if the recorded binary digest still matches. The runner logs a
warning with the reason (and both versions for a format mismatch). Configuration,
source-receipt and disk checks run first; their failures preserve the saved state.
A failed or malformed version query also preserves it and rejects start. Unexpected
restore failures remain quarantined, without an automatic cold fallback.

Do not replace the installed Firecracker during a start operation. The version
probe holds its executable open, but the jailer subsequently opens the installation
path again. An upgrade between those steps can turn a compatible admission into
a failed restore and quarantine; srv does not lock the host's package manager.

Matching formats do not guarantee host-kernel/KVM compatibility. Host upgrades can
still prevent restore. Existing network connections may break, and vsock
connections are reset; applications must reconnect. The guest needs clock
synchronization after a long pause.

The saved files disappear on resume. Firecracker still holds a private memory
mapping of the unlinked RAM file, so its disk blocks remain allocated until that
VMM exits. This is not a reusable backup or an Amp project-template snapshot.

### Runner API

The privileged runner uses HTTP over its local Unix socket (normally
`/run/srv-vm-runner/vm-runner.sock`). Use the SSH commands for normal operation;
they also update instance state, networking, and readiness in the control plane.

| Request | Body | Result |
|---------|------|--------|
| `POST /vm/stop-state-v1` | Existing stop request plus `"save_state": true` | `204` after durable save, VMM termination, and runtime cleanup |
| `POST /vm/start-state-v1` | Existing full start request; omit `discard_state` or set it to `false` | Resume if present, otherwise cold boot; existing `outcome`, `reason`, `pid` response |
| `POST /vm/start-state-v1` | Existing full start request plus `"discard_state": true` | Remove saved RAM and cold boot |

Stop must identify the running attempt with `name`, `instance_id`, `request_id`,
and `host_boot_id`; a PID alone cannot authorize saving RAM. Start supplies a new
request ID and the current host boot ID, along with the usual boot, network, CPU,
and memory configuration. Retry the same start request to recover its outcome,
or use `POST /vm/start-result`; do not invent a new attempt after an unknown result.
The existing `/vm/start` and `/vm/start-v2` routes also resume saved RAM by default.
The versioned state endpoints make an older runner reject unsupported requests
instead of silently ignoring the save/discard flag.

## Restart

```bash
ssh srv restart <name>
```

Stops and starts the VM in one command. Also picks up the current kernel and initrd.

## Check the root filesystem

```bash
ssh srv stop <name>
ssh srv fsck <name> && ssh srv start <name>
```

`fsck` runs `e2fsck -f -p` against the stopped VM's ext4 rootfs. It forces a full check and automatically repairs problems that can be fixed safely. Fatal or uncorrected filesystem errors make the command fail and leave the VM stopped.

## Delete

```bash
ssh srv delete <name>
```

Removes the VM's rootfs, runtime directory, TAP device, cgroup, and jailer workspace. This is irreversible.

## Warm boot behavior

When a VM that already has `tailscaled` state reboots (via `start` or `restart`), it reuses its existing Tailscale identity instead of minting a new auth key. This is called a warm boot.

On cold boot (first `new`), the guest bootstrap service reads the Tailscale auth key from MMDS and runs `tailscale up --auth-key=... --ssh` exactly once.
