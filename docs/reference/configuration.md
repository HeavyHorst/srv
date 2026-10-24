# Configuration reference

srv is configured through environment variables, read from `/etc/srv/srv.env` by the systemd units.

## Provider service owners

Provider nodes can have a fixed, non-human owner identity, including tagged
Tailscale nodes. This is opt-in and uses **stable Tailscale node IDs**, never
hostnames, tags, SSH usernames, Dispatch parameters or Amp member IDs.

```sh
SRV_PROVIDER_NODES='{"nPROVIDER1":"amp-dispatch","nPROVIDER2":"amp-dispatch"}'
SRV_ALLOWED_USERS='alice@example.com,service:amp-dispatch'
```

`srv --provider-nodes '{"nPROVIDER1":"amp-dispatch"}'` overrides the environment
value. The JSON object maps each node ID to a service name matching
`[a-z0-9][a-z0-9_-]{0,63}`. The resulting owner is `service:amp-dispatch`;
values cannot be human logins. Configured nodes use this owner even if their
WhoIs profile belongs to a human/admin. Unconfigured tagged nodes remain denied;
unconfigured ordinary user nodes keep normal user authentication.

For `srv-front`, add the same mapping to its static `--config` JSON:

```json
{
  "provider_nodes": {"nPROVIDER1": "amp-dispatch", "nPROVIDER2": "amp-dispatch"},
  "allowed_users": ["alice@example.com", "service:amp-dispatch"]
}
```

These are additional fields, not a complete frontend config. Configure the
mapping on each backend too: forwarded service owners must be explicitly known
there. Existing frontend trust (`SRV_TRUSTED_FRONTENDS`) still applies, and
frontend and backend allowlists still apply. Provider nodes must **not** be
trusted frontends; backend configuration rejects overlap. Trust only the actual
srv-front node, which authenticates the original peer and forwards its owner.
As before, a trusted frontend is a security boundary that can assert identities.

The `service:` namespace is reserved: direct human profiles cannot claim it and
`SRV_ADMIN_USERS` cannot contain it (startup fails). Service owners receive normal
owner-scoped operations, never admin privileges or another user's instances.
Multiple nodes mapped to the same name intentionally share ownership; use separate
names for isolation. Dispatch supplies no Amp member ID, so this provides **no
per-human Amp ownership**. Removing a mapping revokes that node's service identity
after restart, but does not delete or reassign existing instances. Rotate node IDs
explicitly; do not grant a tag-wide wildcard. Protect configuration as admin-owned.

Host-local `project-template create NAME ID`, `project-template delete ID`, and
`new NAME --template ID` require explicit `--host HOST` through srv-front. Template
creation and use must target the same backend; automatic template placement is
rejected.

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
| `SRV_IMAGES_FILE` | (empty) | JSON image catalog; also available as `--images-file`. Read at service startup. |
| `SRV_IMAGES_DIR` | `SRV_DATA_DIR/images` | Runner's trusted boot-artifact directory; also available as runner `--images-dir`. |

For example, `/etc/srv/images.json` can register Debian alongside the existing
Arch defaults:

```json
{
  "debian-13": {
    "kernel": "/var/lib/srv/images/debian-base/vmlinux",
    "rootfs": "/var/lib/srv/images/debian-base/rootfs-base.img"
  }
}
```

Set `SRV_IMAGES_FILE=/etc/srv/images.json` in the service environment. Upgrade
both `srv` and `srv-vm-runner`, then restart both services; an old runner rejects
image-aware starts rather than silently using its global kernel. Paths must be
absolute; `initrd` is optional. Selected kernels/initrds must resolve within
`SRV_IMAGES_DIR` (except the exact legacy configured kernel/initrd pair).
Keep this tree and its ancestors root-owned and not writable by the srv service
or guests; do not store secrets in it. Symlinks escaping the tree are rejected.
Every base rootfs must be reflink-cloneable into `SRV_DATA_DIR`. Names use lowercase letters,
digits, dots, underscores and hyphens, starting with a letter or digit. Unknown
names are rejected before creating a VM.

`ssh srv new demo --image debian-13` selects Debian; omitting `--image` selects
`arch`. The `arch` entry falls back to `SRV_BASE_KERNEL`, `SRV_BASE_INITRD` and
`SRV_BASE_ROOTFS` unless explicitly defined in the catalog. These legacy variables
are not needed when all required paths are supplied by catalog entries.

The selected image and boot paths are stored with the VM. Non-Arch guests keep
their saved kernel/initrd paths across starts, even if the catalog changes or
the entry is removed. Keep those artifacts available and use versioned paths
when replacing kernels. Arch retains its existing behavior: current configured
Arch boot paths are picked up on start/restart. No existing writable rootfs is
replaced by changing the catalog. `inspect` and JSON responses expose `image`;
older VM records are treated as `arch`.

Portable exports preserve the image name. To import a named image on another
host, register that name with a compatible kernel/module build first; imports
use the destination's boot paths. Older exports without an image name use Arch.
Non-Arch exports use format version 2, so older servers reject them instead of
importing them as Arch; Arch exports remain compatible with format version 1.

## Guest defaults

| Variable | Default | Description |
|----------|---------|-------------|
| `SRV_VM_VCPUS` | `1` | Default vCPU count for new VMs |
| `SRV_VM_MEMORY_MIB` | `1024` | Default memory in MiB for new VMs |
| `SRV_VM_PIDS_MAX` | `512` | Maximum tasks in each VM cgroup |
| `SRV_GUEST_AUTH_TAGS` | (required) | Comma-separated tags applied to guest auth keys |
| `SRV_GUEST_USER_AUTH_TAGS` | — | Optional comma-separated `tailscale-login=tag:name` mappings; a matching creator's tag replaces `SRV_GUEST_AUTH_TAGS` |
| `SRV_GUEST_AUTH_EXPIRY` | `15m` | TTL for one-off guest auth keys |
| `SRV_GUEST_READY_TIMEOUT` | `2m` | Time to wait for a guest to join the tailnet |

User tag mappings are matched case-insensitively against the login returned by Tailscale `WhoIs`; callers cannot request arbitrary tags. For example:

```bash
SRV_GUEST_AUTH_TAGS=tag:microvm
SRV_GUEST_USER_AUTH_TAGS=rene.michaelis@antares.net=tag:rene
```

VMs created by `rene.michaelis@antares.net` receive only `tag:rene`; unmapped creators continue to receive `tag:microvm`. Every configured tag must exist in the tailnet policy and be assignable by the OAuth client used by srv.

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

## Provider gateways

| Variable | Default | Description |
|----------|---------|-------------|
| `SRV_ZEN_API_KEY` | (empty) | OpenCode Zen API key. When set, enables per-VM Zen gateways. |
| `SRV_ZEN_BASE_URL` | `https://opencode.ai/zen` | Upstream Zen API base URL |
| `SRV_ZEN_GATEWAY_PORT` | `11434` | TCP port for each VM's Zen gateway proxy |
| `SRV_DEEPSEEK_API_KEY` | (empty) | DeepSeek API key. When set, enables per-VM DeepSeek gateways. |
| `SRV_DEEPSEEK_BASE_URL` | `https://api.deepseek.com` | Upstream DeepSeek API base URL |
| `SRV_DEEPSEEK_GATEWAY_PORT` | `11436` | TCP port for each VM's DeepSeek gateway proxy |

## HTTP integrations

| Variable | Default | Description |
|----------|---------|-------------|
| `SRV_INTEGRATION_GATEWAY_PORT` | `11435` | TCP port for each VM's host-side integration gateway |
| `SRV_AMP_CONNECT_GATEWAY_PORT` | `3128` | Per-VM HTTP CONNECT gateway for public HTTPS destinations on port 443; private, local, and special-use networks are blocked |
| `SRV_SECRET_*` | (empty) | Host-managed secret env vars referenced by integration definitions for bearer auth, basic auth passwords, or env-backed headers |

The `SRV_SECRET_*` entries are a naming convention rather than a fixed list. They are meant to live in `/etc/srv/srv.env` so the SSH control surface only ever receives secret names such as `SRV_SECRET_OPENAI_PROD`, not the raw credential values.

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

Host-side clients dial `SRV_JAILER_BASE_DIR/<firecracker binary name>/<vm name>/root/firecracker.sock`
and `SRV_DATA_DIR/instances/<vm name>/firecracker.sock` (plus `orb.vsock` for Orbs) by
absolute path, and Unix socket paths are limited to 107 bytes. With the binary named
`firecracker`, the default `SRV_DATA_DIR/jailer` is too long for 60-character Orb names;
`SRV_JAILER_BASE_DIR` must then be at most 12 bytes (9 bytes for 63-character names), for
example `/srvj` on the root filesystem when it also holds `SRV_DATA_DIR`. The VM runner
rejects a start whose socket paths are too long before launching anything. Choose the base
before the first start: start receipts and lifecycle locks live under it, so moving it later
abandons the records of existing attempts.
