# Configuration reference

srv is configured through environment variables, read from `/etc/srv/srv.env` by the systemd units.

## Tailscale credentials

| Variable | Required | Description |
|----------|----------|-------------|
| `TS_AUTHKEY` | Yes* | Tailscale auth key for the control-plane node. After first start, tsnet state usually persists, but keeping this configured is the simplest setup. |
| `TS_CLIENT_ID` | Yes | Tailscale OAuth client ID for minting guest auth keys |
| `TS_CLIENT_SECRET` | Yes | Tailscale OAuth client secret for minting guest auth keys |
| `TS_TAILNET` | Yes | Tailnet name used for API operations |

Use either `TS_AUTHKEY` or `TS_CLIENT_ID`/`TS_CLIENT_SECRET` for the control-plane node. The OAuth flow is preferred for guest auth key minting.

## Core settings

| Variable | Default | Description |
|----------|---------|-------------|
| `SRV_HOSTNAME` | `srv` | Tailscale hostname for the control plane |
| `SRV_LISTEN_ADDR` | `:22` | Tailnet TCP listen address for the SSH API |
| `SRV_DATA_DIR` | `/var/lib/srv` | State directory. Must be on the same reflink-capable filesystem as `SRV_BASE_ROOTFS`. |
| `SRV_NET_HELPER_SOCKET` | `/run/srv/net-helper.sock` | Unix socket for the privileged network helper |
| `SRV_VM_RUNNER_SOCKET` | `/run/srv-vm-runner/vm-runner.sock` | Unix socket for the Firecracker VM runner |
| `SRV_FIRECRACKER_BIN` | `/usr/bin/firecracker` | Path to the Firecracker binary |
| `SRV_JAILER_BIN` | `/usr/bin/jailer` | Path to the jailer binary |

## Guest artifacts

| Variable | Default | Description |
|----------|---------|-------------|
| `SRV_BASE_KERNEL` | (required) | Path to the Firecracker guest kernel image |
| `SRV_BASE_ROOTFS` | (required) | Path to the base rootfs image. Must be on the same reflink-capable filesystem as `SRV_DATA_DIR`. |
| `SRV_BASE_INITRD` | (empty) | Optional initrd image path |

## Guest defaults

| Variable | Default | Description |
|----------|---------|-------------|
| `SRV_VM_VCPUS` | `1` | Default vCPU count for new VMs |
| `SRV_VM_MEMORY_MIB` | `1024` | Default memory in MiB for new VMs |
| `SRV_VM_PIDS_MAX` | `512` | Maximum tasks in each VM cgroup |
| `SRV_GUEST_AUTH_TAGS` | (required) | Comma-separated tags applied to guest auth keys |
| `SRV_GUEST_AUTH_EXPIRY` | `15m` | TTL for one-off guest auth keys |
| `SRV_GUEST_READY_TIMEOUT` | `2m` | Time to wait for a guest to join the tailnet |

## Networking

| Variable | Default | Description |
|----------|---------|-------------|
| `SRV_VM_NETWORK_CIDR` | `172.28.0.0/16` | IPv4 network reserved for VM `/30` allocations |
| `SRV_VM_DNS` | `1.1.1.1,1.0.0.1` | Comma-separated guest nameservers |
| `SRV_OUTBOUND_IFACE` | auto-detected | Optional override for the host interface used for NAT |

## Authorization

| Variable | Default | Description |
|----------|---------|-------------|
| `SRV_ALLOWED_USERS` | (empty) | Comma-separated Tailscale login allowlist. Empty means allow all tailnet users. |
| `SRV_ADMIN_USERS` | (empty) | Comma-separated Tailscale logins with cross-instance visibility and management rights |

## Zen gateway

| Variable | Default | Description |
|----------|---------|-------------|
| `SRV_ZEN_API_KEY` | (empty) | OpenCode Zen API key. When set, enables per-VM Zen gateways. |
| `SRV_ZEN_BASE_URL` | `https://opencode.ai/zen` | Upstream Zen API base URL |
| `SRV_ZEN_GATEWAY_PORT` | `11434` | TCP port for each VM's gateway proxy |

## Alternate Tailscale endpoints

| Variable | Default | Description |
|----------|---------|-------------|
| `TS_CONTROL_URL` | (Tailscale default) | Alternate Tailscale coordination server (e.g. Headscale) |
| `TS_API_BASE_URL` | `https://api.tailscale.com` | Alternate Tailscale API base URL |
| `SRV_GUEST_TAILSCALE_CONTROL_URL` | (same as `TS_CONTROL_URL`) | Alternate control URL injected into guest bootstrap |

## Misc

| Variable | Default | Description |
|----------|---------|-------------|
| `SRV_LOG_LEVEL` | `info` | Log level |
| `SRV_EXTRA_KERNEL_ARGS` | (empty) | Additional kernel arguments appended to the guest boot line |
| `SRV_JAILER_BASE_DIR` | `SRV_DATA_DIR/jailer` | Base directory for jailer workspaces. Must be on the same filesystem as `SRV_DATA_DIR`. |

## Path constraints

Several paths must share the same reflink-capable filesystem (typically btrfs or reflink-enabled xfs):

- `SRV_DATA_DIR` — host state directory
- `SRV_BASE_ROOTFS` — base guest image
- `SRV_JAILER_BASE_DIR` — jailer workspaces (hard-links log files into the jail)

Cross-filesystem hard-links fail, so keeping these on the same filesystem is required.