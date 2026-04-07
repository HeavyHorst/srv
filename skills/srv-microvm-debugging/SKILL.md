---
name: srv-microvm-debugging
description: Guides debugging and testing work inside srv-managed Firecracker microVMs over SSH with Tailscale networking. Use when running isolated code, inspecting VM state, viewing logs, resizing instances, or restoring backups.
---

# srv-microvm-debugging

Use `srv` to spawn isolated Firecracker microVMs for running code, debugging, and testing with full isolation and automatic Tailscale networking.

## Overview

The `srv` service provides SSH-accessible VM management. Connect to the control plane via Tailscale SSH and create disposable VMs with configurable resources.

## Prerequisites

- Connected to the Tailscale tailnet where `srv` is running
- SSH access to `srv` (or configured hostname)

## Common Operations

### Create a VM for debugging

```bash
ssh srv new <name> [--cpus N] [--ram NG] [--rootfs-size NG]
```

Example:
```bash
ssh srv new debug-session --cpus 2 --ram 4G
```

### Check VM status and get Tailscale IP

```bash
ssh srv inspect <name>
```

Returns a text summary with fields like `state`, `tailscale-name`, `tailscale-ip`, `vcpu-count`, and `memory-mib`.

### View VM logs

```bash
# Serial console output (stdout/stderr from guest)
ssh srv logs <name> serial

# Firecracker logs (VM-level events)
ssh srv logs <name> firecracker
```

### Access the VM

Once `inspect` shows state as `ready` with a `tailscale-ip`:

```bash
ssh root@<tailnet_ip>
```

Or use the instance name if configured in SSH config.

### Stop and restart

```bash
ssh srv stop <name>
ssh srv start <name>
```

### Resize (when stopped)

```bash
ssh srv stop <name>
ssh srv resize <name> --cpus 4 --ram 8G
ssh srv start <name>
```

### Backup and restore (when stopped)

```bash
ssh srv stop <name>
ssh srv backup create <name>
ssh srv backup list <name>
ssh srv restore <name> <backup-id>
ssh srv start <name>
```

Backups are in-place stopped-VM snapshots of the writable rootfs and logs. Restore only works back onto the same original VM record, not a newly recreated VM with the same name.

### List all VMs

```bash
ssh srv list
```

### Clean up

```bash
ssh srv delete <name>
```

## Debugging Workflow

1. **Create**: `ssh srv new debug-vm --cpus 2 --ram 4G`
2. **Wait for ready**: Poll `ssh srv inspect debug-vm` until state is `ready`
3. **Connect**: `ssh root@<tailnet_ip>` from inspect output
4. **Run code**: Execute commands inside the VM
5. **Checkpoint if needed**: `ssh srv stop debug-vm && ssh srv backup create debug-vm && ssh srv start debug-vm`
6. **Check logs**: If issues occur, `ssh srv logs debug-vm serial`
7. **Restore if needed**: stop the VM, `ssh srv backup list debug-vm`, `ssh srv restore debug-vm <backup-id>`, then start it again
8. **Clean up**: `ssh srv delete debug-vm`

## Tips for Agents

- VMs boot quickly (seconds) but still need polling - check `inspect` until state transitions to `ready`
- Rootfs is writable and persists across stop/start cycles
- Use stopped backups before risky debugging sessions that may leave the VM in a bad state
- Use `resize` to give more resources to long-running analysis tasks
- Serial logs capture stdout/stderr - useful for capturing script output
- VMs auto-join the Tailscale network - no manual network configuration needed
- The guest image is Arch Linux with standard tools available

## Resource Limits

- CPU: Configurable per VM (default varies by installation)
- RAM: Specified with `--ram` flag (e.g., `2G`, `512M`)
- Disk: `--rootfs-size` for writable disk (grow-only, can resize larger later)

## Error Handling

If `inspect` shows errors or the VM doesn't reach `ready` state:
1. Check Firecracker logs: `ssh srv logs <name> firecracker`
2. Check serial output for boot failures: `ssh srv logs <name> serial`
3. Verify host resources aren't exhausted: `ssh srv list` to see all VMs
4. Try recreating with more resources if OOM issues suspected
