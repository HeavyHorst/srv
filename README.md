# srv

> **Note:** This project is under active development. APIs, configuration, and behavior may change without notice.

Self-hosted control-plane service for creating Firecracker microVMs over SSH on a Tailscale tailnet.

`srv` exposes an SSH command surface on one Linux host and manages Firecracker microVMs behind it.

```bash
ssh root@srv new demo
```

Per-instance sizing can be overridden at create time:

```bash
ssh root@srv new demo --cpus 4 --ram 8G --rootfs-size 20G
```

Existing stopped instances can be resized later:

```bash
ssh root@srv stop demo
ssh root@srv resize demo --cpus 4 --ram 8G --rootfs-size 20G
ssh root@srv start demo
```

The service treats SSH as command transport only. Caller identity comes from Tailscale `WhoIs` data resolved from the incoming tailnet connection.

## Quickstart

1. Build the Arch base image artifacts:

```bash
sudo OUTPUT_DIR=/var/lib/srv/images/arch-base ./images/arch-base/build.sh
```

2. Install the systemd assets:

```bash
sudo ./contrib/systemd/install.sh
sudoedit /etc/srv/srv.env
sudo ./contrib/systemd/install.sh --enable-now
```

3. Validate the host end to end:

```bash
sudo ./contrib/smoke/host-smoke.sh
```

The installer downloads the matching official static Firecracker and jailer release pair into `/usr/local/bin` by default. If `/etc/srv/srv.env` already exists, it is kept by default, so upgraded hosts should verify that `SRV_FIRECRACKER_BIN` and `SRV_JAILER_BIN` still point at the intended static binaries.

The prepared-host systemd path is the one the repo currently validates. Manual foreground runs are still useful for development, but they are not the documented production path.

## Common Commands

```bash
# create
ssh root@srv new demo
ssh root@srv new demo --cpus 4 --ram 8G --rootfs-size 20G

# inspect and logs
ssh root@srv list
ssh root@srv inspect demo
ssh root@srv logs demo
ssh root@srv logs demo serial
ssh root@srv logs demo firecracker

# lifecycle
ssh root@srv stop demo
ssh root@srv start demo
ssh root@srv restart demo
ssh root@srv delete demo

# stopped-VM backups
ssh root@srv backup create demo
ssh root@srv backup list demo
ssh root@srv restore demo <backup-id>

# resize while stopped
ssh root@srv stop demo
ssh root@srv resize demo --cpus 4 --ram 8G --rootfs-size 20G
ssh root@srv start demo
```

Per-VM backup and restore is currently an in-place stopped-instance workflow: create a backup from a stopped VM, then restore that backup back onto the same VM later. Backups are tied to the original VM record and are not restored onto a newly recreated VM that happens to reuse the same name.

## Host Requirements

- Linux host with cgroup v2 and `/dev/kvm`
- `SRV_DATA_DIR` on a reflink-capable filesystem, with `SRV_BASE_ROOTFS` on the same filesystem; for example `btrfs`, or `xfs` created with reflink support enabled
- Tailscale installed and working on the host
- Tailscale OAuth client credentials with permission to mint auth keys for the configured guest tags
- `ip`, `iptables`, `cp`, and `resize2fs` available on the host
- Official static Firecracker and jailer release pair, or let `contrib/systemd/install.sh` install them

## Key Configuration

Core environment variables live in [`contrib/systemd/srv.env.example`](contrib/systemd/srv.env.example).

- `TS_AUTHKEY` or `TS_CLIENT_SECRET` / `TS_CLIENT_ID`: control-plane Tailscale credentials
- `TS_TAILNET`: tailnet name used for guest auth-key minting
- `SRV_ALLOWED_USERS`: optional comma-separated Tailscale login allowlist
- `SRV_ADMIN_USERS`: optional comma-separated Tailscale login list with cross-instance visibility and management rights
- `SRV_BASE_KERNEL`: Firecracker guest kernel image
- `SRV_BASE_INITRD`: optional initrd image
- `SRV_BASE_ROOTFS`: base guest rootfs image stored on the same reflink-capable filesystem as `SRV_DATA_DIR` such as `btrfs` or reflink-enabled `xfs`
- `SRV_GUEST_AUTH_TAGS`: comma-separated tags applied to guest auth keys
- `SRV_DATA_DIR`: host state directory, default `/var/lib/srv`
- `SRV_JAILER_BASE_DIR`: base directory for jailer workspaces, default `SRV_DATA_DIR/jailer`
- `SRV_VM_PIDS_MAX`: maximum tasks allowed in each VM cgroup, default `512`
- `SRV_OUTBOUND_IFACE`: optional override for the host interface used for NAT

## Validation

For a repeatable end-to-end validation pass on a prepared host, run:

```bash
sudo ./contrib/smoke/host-smoke.sh
```

That harness validates the systemd-managed `srv`, `srv-net-helper`, and `srv-vm-runner` units, confirms the SSH control surface is reachable, creates a real guest, waits for `inspect` readiness, polls for a real guest SSH session after each ready transition, verifies `list`, exercises a full stop/backup/start/restore cycle, proves restore actually rolls the guest rootfs back, checks the live per-VM cgroup limit files, and finally deletes the guest while confirming TAP, jailer, and cgroup cleanup.

When debugging a failed host run, start with `ssh root@srv inspect <name>`, then compare the newest lines from `ssh root@srv logs <name> serial`, `ssh root@srv logs <name> firecracker`, and `journalctl -u srv-vm-runner`.

## Architecture Notes

- `tsnet` joins the tailnet as `srv` and exposes the control API on tailnet TCP port `22`.
- `gliderlabs/ssh` handles `exec` requests and rejects shell sessions.
- SQLite stores instances, events, command audits, and authz decisions.
- Reflinks clone the base rootfs for fast per-instance writable disks.
- A root-only network helper owns TAP and iptables mutations, while a separate root-owned VM runner invokes Firecracker through the official jailer, drops the microVM process to `srv-vm:srv`, and places each VM into its own cgroup v2 leaf.
- The control plane mints a one-off Tailscale auth key for each guest and injects it through Firecracker MMDS metadata.
- Existing stopped guests pick up the currently configured `SRV_BASE_KERNEL` and optional `SRV_BASE_INITRD` on their next `start` or `restart`.
- Rootfs changes only affect newly created guests after you rebuild the base image artifacts and refresh `SRV_BASE_ROOTFS`.

The current Arch guest image expects a boot-time service that reads MMDS, sets the hostname, starts `tailscaled`, and runs `tailscale up --auth-key=... --ssh` on the first authenticated boot only.

## Docs

- [`docs/operations.md`](docs/operations.md): backup, restore, rebuild, upgrade, rollback, and host hardening
- [`contrib/smoke/README.md`](contrib/smoke/README.md): smoke-test prerequisites, behavior, overrides, and artifacts
- [`docs/cheatsheet.md`](docs/cheatsheet.md): operator command reference
- [`images/arch-base/README.md`](images/arch-base/README.md): guest image builder and overlay details
- [`contrib/systemd/install.sh`](contrib/systemd/install.sh): one-shot installer for the supported host path

## Non-Goals For Now

- `srv` is intentionally a single-host control plane for this phase of the project.
- Clustering or multi-host scheduling is out of scope for now.
- High availability or control-plane replication is out of scope for now.
- A web UI is out of scope for now; SSH remains the control interface.
- Live migration is out of scope for now.
