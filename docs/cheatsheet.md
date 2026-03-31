# srv Cheatsheet

Quick reference for the srv control plane.

## Commands (via SSH)

```bash
ssh root@srv <command> [args]
```

| Command | Description |
|---------|-------------|
| `new <name>` | Create new VM with optional `--cpus`, `--ram`, `--rootfs-size` |
| `list` | Show all VMs |
| `inspect <name>` | Show VM details and status |
| `logs <name>` | View serial or firecracker logs |
| `start <name>` | Start a stopped VM |
| `stop <name>` | Stop VM (graceful shutdown) |
| `restart <name>` | Restart VM |
| `delete <name>` | Remove VM |
| `resize <name>` | Resize stopped VM (grow-only) |

## Quick Examples

```bash
# Create VM
ssh root@srv new demo

# With sizing
ssh root@srv new demo --cpus 4 --ram 8G --rootfs-size 20G

# Resize (must be stopped)
ssh root@srv stop demo
ssh root@srv resize demo --cpus 4 --ram 8G
ssh root@srv start demo

# View logs
ssh root@srv logs demo
ssh root@srv logs demo serial
ssh root@srv logs demo firecracker
```

## Systemd Management

```bash
# Status
sudo systemctl status srv srv-net-helper srv-vm-runner

# Restart
sudo systemctl restart srv srv-net-helper srv-vm-runner

# View logs
sudo journalctl -u srv -f
sudo journalctl -u srv-vm-runner -f
```

## Smoke Test

```bash
# Basic
sudo ./contrib/smoke/host-smoke.sh

# Strict (for upgrades)
STRICT_HOST_ASSERTIONS=1 sudo ./contrib/smoke/host-smoke.sh
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
| `SRV_DATA_DIR` | `/var/lib/srv` | State directory (must be Btrfs) |
| `SRV_BASE_KERNEL` | - | Firecracker kernel path |
| `SRV_BASE_ROOTFS` | - | Base rootfs image |
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
STRICT_HOST_ASSERTIONS=1 sudo ./contrib/smoke/host-smoke.sh
```

## Debug Commands

```bash
# VM inspection
ssh root@srv inspect <name>

# Recent logs
ssh root@srv logs <name> serial
ssh root@srv logs <name> firecracker

# System logs
journalctl -u srv-vm-runner | tail

# Check cgroups
cat /sys/fs/cgroup/firecracker-vms/<name>/cpu.max
cat /sys/fs/cgroup/firecracker-vms/<name>/memory.max
```

## Notes

- VM disks at: `SRV_DATA_DIR/instances/<name>/rootfs.img`
- Resize is grow-only (shrink rejected)
- Resize only works on stopped VMs
- Creators manage their own VMs; admins manage all VMs
- Warm start/restart reuses tailscaled state
- Host reboot auto-restarts active VMs
