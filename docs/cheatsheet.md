# srv Cheatsheet

Quick reference for the srv control plane.

## Commands (via SSH)

```bash
ssh srv -- [--json] <command> [args]
```

Use `--json` with the non-streaming instance and backup commands when you need machine-readable output.

| Command | Description |
|---------|-------------|
| `new <name>` | Create new VM with optional `--cpus`, `--ram`, `--rootfs-size` |
| `list` | Show visible VMs (all for admins, own for regular users) |
| `top [--interval DURATION]` | Live per-VM CPU, memory, disk, and network view; press `q` to exit |
| `status` | Admin-only host capacity and allocation summary |
| `inspect <name>` | Show VM details and status |
| `logs <name>` | View serial or firecracker logs |
| `start <name>` | Start a stopped VM |
| `stop <name>` | Stop VM (graceful shutdown) |
| `restart <name>` | Restart VM |
| `delete <name>` | Remove VM |
| `resize <name>` | Resize stopped VM (CPU/RAM up or down, rootfs grow-only) |
| `backup create <name>` | Create an in-place backup for a stopped VM |
| `backup list <name>` | List stored backups for a VM |
| `restore <name> <backup-id>` | Restore a stopped VM from one of its backups |

## Quick Examples

```bash
# Create VM
ssh srv new demo
ssh srv status
ssh srv -- --json inspect demo

# With sizing
ssh srv new demo --cpus 4 --ram 8G --rootfs-size 20G

# Resize (must be stopped)
ssh srv stop demo
ssh srv resize demo --cpus 4 --ram 8G
ssh srv start demo

# Backup and restore (VM must be stopped)
ssh srv stop demo
ssh srv backup create demo
ssh srv backup list demo
ssh srv restore demo <backup-id>

# View logs
ssh srv logs demo
ssh srv logs demo serial
ssh srv logs demo firecracker
ssh srv logs -f demo serial
ssh srv logs -f demo firecracker

# Live VM usage
ssh -t srv top
ssh -t srv top --interval 2s
```

## Systemd Management

```bash
# Status
sudo systemctl status srv srv-net-helper srv-vm-runner

# Restart
sudo systemctl stop srv srv-net-helper srv-vm-runner
sleep 5
sudo systemctl start srv-vm-runner srv-net-helper srv

# View logs
sudo journalctl -u srv -f
sudo journalctl -u srv-vm-runner -f
```

## Smoke Test

```bash
sudo ./contrib/smoke/host-smoke.sh
```

## Build Artifacts

```bash
# Binaries
go build ./cmd/srv
go build ./cmd/srv-net-helper
go build ./cmd/srv-vm-runner

# Guest image
sudo OUTPUT_DIR=/var/lib/srv/images/arch-base ./images/arch-base/build.sh

# Install
sudo ./contrib/systemd/install.sh
sudoedit /etc/srv/srv.env
sudo ./contrib/systemd/install.sh --enable-now
```

## Key Env Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SRV_DATA_DIR` | `/var/lib/srv` | State directory on the same reflink-capable filesystem as `SRV_BASE_ROOTFS`, for example `btrfs` or reflink-enabled `xfs` |
| `SRV_BASE_KERNEL` | - | Firecracker kernel path |
| `SRV_BASE_ROOTFS` | - | Base rootfs image on the same reflink-capable filesystem as `SRV_DATA_DIR`, such as `btrfs` or reflink-enabled `xfs` |
| `SRV_BASE_INITRD` | - | Optional initrd |
| `SRV_ALLOWED_USERS` | - | Comma-separated Tailscale allowlist |
| `SRV_ADMIN_USERS` | - | Cross-instance admin access |
| `SRV_GUEST_AUTH_TAGS` | - | Tags for guest auth keys |
| `TS_TAILNET` | - | Tailnet name |
| `SRV_FIRECRACKER_BIN` | `/usr/bin/jailer` | Firecracker binary |
| `SRV_JAILER_BIN` | `/usr/bin/jailer` | Jailer binary |

## Backup & Restore

```bash
# Backup
sudo systemctl stop srv srv-net-helper srv-vm-runner
sudo tar -czf backup.tar.gz /etc/srv /var/lib/srv

# Restore
sudo ./contrib/systemd/install.sh
# restore /etc/srv/srv.env and /var/lib/srv
sudo systemctl daemon-reload
sudo systemctl enable --now srv-vm-runner srv-net-helper srv
sudo ./contrib/smoke/host-smoke.sh
```

## Debug Commands

```bash
# VM inspection
ssh srv inspect <name>

# Recent logs
ssh srv logs <name> serial
ssh srv logs <name> firecracker

# System logs
journalctl -u srv-vm-runner | tail

# Check cgroups
cat /sys/fs/cgroup/firecracker-vms/<name>/cpu.max
cat /sys/fs/cgroup/firecracker-vms/<name>/memory.max
```

## Notes

- VM disks at: `SRV_DATA_DIR/instances/<name>/rootfs.img`
- VM backups live at: `SRV_DATA_DIR/backups/<name>/<backup-id>/`
- Resize requires a stopped VM; CPU and RAM can go up or down within limits, but rootfs shrink is rejected
- Resize only works on stopped VMs
- Backup and restore only work on stopped VMs and only restore onto the original VM record, not a newly recreated VM with the same name
- Creators manage their own VMs; admins manage all VMs
- Warm start/restart reuses tailscaled state
- Host reboot auto-restarts active VMs
