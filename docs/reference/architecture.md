# Architecture

srv is a single-host control plane that manages Firecracker microVMs through an SSH command surface on a Tailscale tailnet.

## Control plane

<!-- srv-manual:block=diagram -->
```
┌────────────────────────────────────────────────────────┐
│                      srv process                       │
│                                                        │
│  ┌──────────┐  ┌──────────────┐  ┌──────────────────┐  │
│  │ tsnet    │  │ gliderlabs/  │  │ authorization    │  │
│  │ :22/tcp  │──│ ssh          │──│ (Tailscale       │  │
│  └──────────┘  └──────────────┘  │ WhoIs)           │  │
│                                  └──────────────────┘  │
│                                                        │
│  ┌──────────┐  ┌──────────────┐  ┌──────────────────┐  │
│  │ SQLite   │  │ reflink      │  │ Tailscale API    │  │
│  │ store    │  │ cloner       │  │ (auth key        │  │
│  └──────────┘  └──────────────┘  │ minting)         │  │
│                                  └──────────────────┘  │
│                                                        │
│  ┌─────────────┐  ┌─────────────────────────────────┐  │
│  │ Provider    │  │ per-VM HTTP proxies             │  │
│  │ gateways    │  │ (inject host API keys)          │  │
│  └─────────────┘  └─────────────────────────────────┘  │
└────────────────────────────────────────────────────────┘
         │                    │                  │
    ┌────┴────┐          ┌────┴────┐        ┌────┴─────┐
    │ net-    │          │ vm-     │        │ Tailscale│
    │ helper  │          │ runner  │        │ coord.   │
    │ (root)  │          │ (root)  │        │ server   │
    └────┬────┘          └────┬────┘        └──────────┘
         │                    │
    ┌────┴────┐          ┌────┴─────────────┐
    │ TAP +   │          │ Firecracker      │
    │ iptables│          │ + jailer         │
    │ + NAT   │          │ → srv-vm:srv     │
    └─────────┘          │ → cgroup v2      │
                         └──────────────────┘
```

## Key components

### tsnet + gliderlabs/ssh

`srv` joins the tailnet as the hostname configured in `SRV_HOSTNAME` (default `srv`) and listens on the tailnet TCP port configured in `SRV_LISTEN_ADDR` (default `:22`). The `gliderlabs/ssh` library handles SSH exec requests and rejects shell sessions.

Caller identity comes from Tailscale `WhoIs` data resolved from the incoming tailnet connection — not from the SSH username.

### SQLite store

All instance state, event history, command audits, and authorization decisions are stored in SQLite under `SRV_DATA_DIR/state/app.db`. Migrations run during startup and are additive.

### Reflink cloning

When a new VM or Orb is created, the base rootfs is reflink-cloned on btrfs into a dedicated `instances/<name>/disk` subvolume. This gives each instance its own writable `rootfs.img` while initially sharing data extents. All base rootfs images must be on the same btrfs filesystem as the data-root subvolume. Legacy stopped instances migrate automatically on start using a durable journal; see [storage migration](operations.md#storage-layout-migration).

### Network helper

A root-only helper process owns all TAP device creation, iptables MASQUERADE rules, and FORWARD rules. The main `srv` process communicates with it over a unix socket.

### VM runner

A root-owned process invokes Firecracker through the official jailer binary. After the jailer sets up the chroot and drops privileges, the microVM process runs as `srv-vm:srv`. Each VM is placed into its own cgroup v2 leaf under `firecracker-vms/<name>` with enforced limits.

The runner bind-mounts the disk, kernel, initrd, and log files into the jail.
Only the Orb socket is hard-linked, so Orb runtime and jail directories must
remain in the same subvolume. The main `srv` process gains no new capabilities.

### Tailscale integration

For each new VM, the control plane:

1. Mints a one-off Tailscale auth key using the configured OAuth credentials and tags
2. Injects the key into the VM's MMDS payload
3. The guest bootstrap service reads the key and runs `tailscale up --auth-key=... --ssh`

On warm reboots (after a `stop` + `start` or `restart`), the guest reuses its persisted `tailscaled` state instead of minting a new key.

### Provider gateways

When `SRV_ZEN_API_KEY` or `SRV_DEEPSEEK_API_KEY` is set, `srv` binds a per-instance HTTP proxy on each VM's gateway IP and the provider's configured port (`SRV_ZEN_GATEWAY_PORT` for Zen, `SRV_DEEPSEEK_GATEWAY_PORT` for DeepSeek). The proxies forward guest requests to the upstream APIs while injecting the host keys. See [Provider gateways](../networking/provider-gateways.md) for details.

## Data paths

<!-- srv-manual:block=diagram -->
```
SRV_DATA_DIR/
├── state/
│   ├── app.db              # SQLite store
│   ├── tsnet/               # Tailscale persistent state
│   └── host_key             # SSH host key
├── images/
│   └── arch-base/
│       ├── vmlinux          # Guest kernel
│       └── rootfs-base.img  # Base rootfs (reflink source)
├── instances/
│   └── <name>/
│       ├── disk/            # btrfs disk subvolume
│       │   └── rootfs.img   # Writable reflink clone
│       ├── serial.log       # Serial console output
│       └── firecracker.log  # VMM log
├── backups/
│   └── <name>/
│       └── <backup-id>/
│           └── rootfs.img   # Backup copy
├── jailer/                 # Jailer workspaces
└── .snapshots/              # btrfs archive subvolume
    └── <timestamp>/
```

Host snapshots are [root-and-disk bundles with a manifest](../tasks/host-snapshots.md),
not recursive snapshots of the data root. They do not provide global disk atomicity.
Optional [cold storage over SSH](operations.md#optional-cold-disks-over-ssh) moves
stopped disks to a trusted btrfs server and restores them fully before boot.
The root runner owns transfer journals and shares its name lock with VM starts;
the unprivileged control plane gains no filesystem capabilities. Complete host
snapshots require all cold generations hydrated and purged first.

## Service architecture

| Service | User | Role |
|---------|------|------|
| `srv.service` | `srv` | Main control plane (tsnet, SSH, SQLite, API) |
| `srv-net-helper.service` | `root` | TAP, iptables, NAT management |
| `srv-vm-runner.service` | `root` → `srv-vm:srv` | Firecracker invocation, cgroup management |

The `srv-vm-runner` process starts as root, and the jailer drops the Firecracker process to `srv-vm:srv` within each VM's cgroup v2 leaf.
