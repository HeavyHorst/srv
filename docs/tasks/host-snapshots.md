# Host snapshots

The `snapshot create` command is a host-level disaster-recovery primitive that creates a consistent point-in-time copy of `SRV_DATA_DIR`.

## How it works

```bash
ssh srv snapshot create
```

This is an admin-only command that:

1. Briefly rejects all other SSH commands
2. Waits for already admitted commands to finish
3. Checkpoints SQLite
4. Flushes the filesystem
5. Creates a readonly btrfs snapshot of `SRV_DATA_DIR` under `SRV_DATA_DIR/.snapshots/<timestamp>`

## Snapshot semantics

The snapshot is intentionally simple:

- **Control-plane consistent**: SQLite state is checkpointed
- **Stopped guests fully safe**: rootfs data is on disk and consistent
- **Running guests crash-consistent only**: like pulling the power on a running VM — the filesystem may need journal replay on restore

!!! warning
    This is not a substitute for per-VM backups if you need application-consistent snapshots of running guests. Stop guests first or use `ssh srv backup create` for VM-level consistency.

## Prerequisites

- `SRV_DATA_DIR` must be a **btrfs subvolume root** — a plain directory on btrfs is not enough
- The snapshot covers `SRV_DATA_DIR` only. `/etc/srv`, environment files, and unit overrides still need your existing operator-managed backup flow

## Use with btrfs send/receive

You can combine snapshots with `btrfs send/receive` for off-host replication:

```bash
# After creating a snapshot
sudo btrfs send SRV_DATA_DIR/.snapshots/<timestamp> | \
  ssh backup-host btrfs receive /backup/srv/
```

Run `btrfs send/receive` **after** the local snapshot already exists — the snapshot barrier is not involved in the replication step.

## Not included

- `/etc/srv/srv.env` — back this up separately
- Systemd unit overrides in `/etc/systemd/system/srv*.service.d/`
- Firecracker and jailer binaries

See the [operations reference](../reference/operations.md) for the full host backup and restore workflow.