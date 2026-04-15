# SSH command reference

All srv commands are invoked through the SSH surface. The service treats SSH as command transport only — there is no shell session.

## Syntax

```bash
ssh srv <command> [args]
```

For machine-readable output, use `--json` with non-streaming instance and backup commands. With OpenSSH, terminate local option parsing first:

```bash
ssh srv -- --json list
ssh srv -- --json inspect demo
ssh srv -- --json status
```

## Instance commands

| Command | Description |
|---------|-------------|
| `new <name>` | Create a new VM |
| `new <name> --cpus <n>` | Create with custom vCPU count |
| `new <name> --ram <size>` | Create with custom memory (`2G`, `512M`, or MiB integer) |
| `new <name> --rootfs-size <size>` | Create with custom rootfs size |
| `list` | Show visible VMs (all for admins, own for regular users) |
| `inspect <name>` | Show VM details and event history |
| `logs <name>` | View serial log (default) |
| `logs <name> serial` | View serial log |
| `logs <name> firecracker` | View Firecracker VMM log |
| `logs -f <name> [serial\|firecracker]` | Follow log output |
| `top [--interval <duration>]` | Watch live per-VM CPU, memory, disk, and network usage; run with `ssh -t` and press `q` to exit |

## Lifecycle commands

| Command | Description |
|---------|-------------|
| `start <name>` | Start a stopped VM |
| `stop <name>` | Stop a running VM (graceful shutdown) |
| `restart <name>` | Stop and start a VM |
| `delete <name>` | Delete a VM and all its resources |

## Resize command

| Command | Description |
|---------|-------------|
| `resize <name> --cpus <n>` | Resize vCPUs on a stopped VM |
| `resize <name> --ram <size>` | Resize memory on a stopped VM |
| `resize <name> --rootfs-size <size>` | Resize rootfs (stopped VM, grow-only) |

All flags can be combined. Omitted flags keep the current value.

## Backup commands

| Command | Description |
|---------|-------------|
| `backup create <name>` | Create a backup from a stopped VM |
| `backup list <name>` | List backups for a VM |
| `restore <name> <backup-id>` | Restore a stopped VM from a backup |

## Transfer commands

| Command | Description |
|---------|-------------|
| `export <name>` | Stream a stopped VM as a tar artifact to stdout |
| `import` | Read a tar artifact from stdin and recreate the VM |

Usage: `ssh srv-a export demo | ssh srv-b import`

## Host commands

| Command | Description |
|---------|-------------|
| `status` | Admin-only host capacity and allocation summary |
| `snapshot create` | Admin-only host-local btrfs snapshot of SRV_DATA_DIR |

## Notes

- `new` accepts `--cpus`, `--ram`, and `--rootfs-size` in any combination
- `resize` requires the VM to be stopped; CPU and RAM may increase or decrease within limits, while rootfs is grow-only
- `resize`, `backup`, and `restore` all require the VM to be stopped
- Backups are tied to the original VM record — they cannot be restored onto a different VM
- Export requires the source VM to be stopped
- Import recreates the VM under the same name and leaves it stopped
- `top` refreshes continuously by default; use `ssh -t srv top --interval 2s` or similar to slow the redraw rate
