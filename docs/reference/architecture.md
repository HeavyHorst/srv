# Architecture

srv is a single-host control plane that manages Firecracker microVMs through an SSH command surface on a Tailscale tailnet.

## Control plane

```
┌──────────────────────────────────────────────────────┐
│                    srv process                        │
│                                                      │
│  ┌──────────┐  ┌──────────┐  ┌────────────────────┐ │
│  │  tsnet    │  │ glider   │  │  authorization     │ │
│  │  :22/tcp │──│ labs/ssh │──│  (Tailscale WhoIs)  │ │
│  └──────────┘  └──────────┘  └────────────────────┘ │
│                                                      │
│  ┌──────────┐  ┌──────────┐  ┌────────────────────┐ │
│  │  SQLite   │  │ Reflink  │  │  Tailscale API     │ │
│  │  store    │  │ cloner   │  │  (auth key minting) │ │
│  └──────────┘  └──────────┘  └────────────────────┘ │
│                                                      │
│  ┌──────────────┐  ┌───────────────────────────────┐ │
│  │  Zen gateway  │  │  Per-VM HTTP proxy            │ │
│  │  manager      │  │  (injects SRV_ZEN_API_KEY)   │ │
│  └──────────────┘  └───────────────────────────────┘ │
└──────────────────────────────────────────────────────┘
         │                    │                │
    ┌────┴────┐          ┌───┴───┐       ┌────┴────┐
    │ net-    │          │ vm-   │       │ Tailscale│
    │ helper  │          │ runner│       │ coord.   │
    │ (root)  │          │ (root)│       │ server   │
    └────┬────┘          └───┬───┘       └─────────┘
         │                   │
    ┌────┴────┐          ┌───┴───────────┐
    │ TAP +   │          │ Firecracker    │
    │ iptables│          │ + jailer       │
    │ + NAT   │          │ → srv-vm:srv   │
    └─────────┘          │ → cgroup v2    │
                         └────────────────┘
```

## Key components

### tsnet + gliderlabs/ssh

`srv` joins the tailnet as the hostname configured in `SRV_HOSTNAME` (default `srv`) and listens on the tailnet TCP port configured in `SRV_LISTEN_ADDR` (default `:22`). The `gliderlabs/ssh` library handles SSH exec requests and rejects shell sessions.

Caller identity comes from Tailscale `WhoIs` data resolved from the incoming tailnet connection — not from the SSH username.

### SQLite store

All instance state, event history, command audits, and authorization decisions are stored in SQLite under `SRV_DATA_DIR/state/app.db`. Migrations run during startup and are additive.

### Reflink cloning

When a new VM is created, the base rootfs is cloned using filesystem reflinks (on btrfs or reflink-enabled xfs). This gives each VM its own writable copy without actually copying the data — the copy is instantaneous on a reflink-capable filesystem.

### Network helper

A root-only helper process owns all TAP device creation, iptables MASQUERADE rules, and FORWARD rules. The main `srv` process communicates with it over a unix socket.

### VM runner

A root-owned process invokes Firecracker through the official jailer binary. After the jailer sets up the chroot and drops privileges, the microVM process runs as `srv-vm:srv`. Each VM is placed into its own cgroup v2 leaf under `firecracker-vms/<name>` with enforced limits.

### Tailscale integration

For each new VM, the control plane:

1. Mints a one-off Tailscale auth key using the configured OAuth credentials and tags
2. Injects the key into the VM's MMDS payload
3. The guest bootstrap service reads the key and runs `tailscale up --auth-key=... --ssh`

On warm reboots (after a `stop` + `start` or `restart`), the guest reuses its persisted `tailscaled` state instead of minting a new key.

### Zen gateway

When `SRV_ZEN_API_KEY` is set, `srv` binds a per-instance HTTP proxy on each VM's gateway IP and `SRV_ZEN_GATEWAY_PORT`. The proxy forwards guest requests to the upstream Zen API while injecting the host key. See [Zen gateway](../networking/zen-gateway.md) for details.

## Data paths

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
│       ├── rootfs.img       # Writable reflink clone
│       ├── serial.log       # Serial console output
│       └── firecracker.log  # VMM log
├── backups/
│   └── <name>/
│       └── <backup-id>/
│           └── rootfs.img   # Backup copy
├── jailer/                 # Jailer workspaces
└── .snapshots/              # btrfs host snapshots
    └── <timestamp>/
```

## Service architecture

| Service | User | Role |
|---------|------|------|
| `srv.service` | `srv` | Main control plane (tsnet, SSH, SQLite, API) |
| `srv-net-helper.service` | `root` | TAP, iptables, NAT management |
| `srv-vm-runner.service` | `root` → `srv-vm:srv` | Firecracker invocation, cgroup management |

The `srv-vm-runner` process starts as root, and the jailer drops the Firecracker process to `srv-vm:srv` within each VM's cgroup v2 leaf.