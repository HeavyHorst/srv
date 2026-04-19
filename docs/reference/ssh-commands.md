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
| `new <name> --integration <name>` | Create and enable one or more existing integrations (admin only) |
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

- `new` accepts `--cpus`, `--ram`, and `--rootfs-size` in any combination
- `new` also accepts repeated `--integration <name>` flags; the create request fails if any requested integration cannot be enabled
- `resize` requires the VM to be stopped; CPU and RAM may increase or decrease within limits, while rootfs is grow-only
- `resize`, `backup`, and `restore` all require the VM to be stopped
- Backups are tied to the original VM record — they cannot be restored onto a different VM
- Export requires the source VM to be stopped
- Import recreates the VM under the same name and leaves it stopped
- `top` refreshes continuously by default; use `ssh -t srv top --interval 2s` or similar to slow the redraw rate
- Integration targets are intentionally narrow in v1: HTTP only, operator-managed, no guest-supplied raw secrets, and no automatic outbound interception
