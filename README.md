# srv

> **Note:** This project is under active development. APIs, configuration, and behavior may change without notice.

Self-hosted control-plane service for creating Firecracker microVMs over SSH on a Tailscale tailnet.

`srv` exposes an SSH command surface on one Linux host and manages Firecracker microVMs behind it.

```bash
ssh srv new demo
```

Per-instance sizing can be overridden at create time:

```bash
ssh srv new demo --cpus 4 --ram 8G --rootfs-size 20G
```

Existing stopped instances can be resized later:

```bash
ssh srv stop demo
ssh srv resize demo --cpus 4 --ram 8G --rootfs-size 20G
ssh srv start demo
```

The service treats SSH as command transport only. Caller identity comes from Tailscale `WhoIs` data resolved from the incoming tailnet connection.
Control-plane examples omit a username because authorization is based on that Tailscale identity, not the SSH username.

Most non-streaming instance and backup commands accept a global `--json` flag. With OpenSSH, terminate local ssh option parsing first, for example `ssh srv -- --json inspect demo` or `ssh srv -- --json list`.

## Use Cases

- **Throwaway debug VMs** — spin up an isolated environment, break things, and delete it without affecting the host
- **Sandboxed agent VMs** — give AI coding agents their own cgroup-limited VM with per-instance Tailscale identity and a scoped Zen API proxy
- **Dev/test environments** — fast reflink-based clones from a single base image, with backup/restore for instant reset
- **Isolated workloads** — run services in separate microVMs with per-VM networking, auth, and resource limits

## Quickstart

1. Build the Arch base image artifacts:

```bash
sudo OUTPUT_DIR=/var/lib/srv/images/arch-base ./images/arch-base/build.sh
```

If your host is not Arch and does not provide `pacstrap`, use the documented `podman` workflow in [`images/arch-base/README.md`](images/arch-base/README.md) instead of running the builder directly on the host.

2. Install the systemd assets:

```bash
sudo ./contrib/systemd/install.sh
sudo tee /etc/sysctl.d/90-srv-ip-forward.conf >/dev/null <<'EOF'
net.ipv4.ip_forward = 1
EOF
sudo sysctl --system
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
ssh srv new demo
ssh srv new demo --cpus 4 --ram 8G --rootfs-size 20G

# inspect and logs
ssh srv list
ssh srv status
ssh srv inspect demo
ssh srv -- --json list
ssh srv -- --json status
ssh srv -- --json inspect demo
ssh srv logs demo
ssh srv logs demo serial
ssh srv logs demo firecracker
ssh srv logs -f demo serial
ssh srv logs -f demo firecracker

# lifecycle
ssh srv stop demo
ssh srv start demo
ssh srv restart demo
ssh srv delete demo

# host-local snapshot barrier
ssh srv status
ssh srv snapshot create

# stopped-VM backups
ssh srv backup create demo
ssh srv backup list demo
ssh srv restore demo <backup-id>

# stopped-VM transfer between hosts
ssh srv export demo | ssh srv-dr import

# resize while stopped
ssh srv stop demo
ssh srv resize demo --cpus 4 --ram 8G --rootfs-size 20G
ssh srv start demo
```

Per-VM backup and restore is currently an in-place stopped-instance workflow: create a backup from a stopped VM, then restore that backup back onto the same VM later. Backups are tied to the original VM record and are not restored onto a newly recreated VM that happens to reuse the same name.

`export | import` is the portable stopped-VM path. It streams a tar artifact with a versioned manifest, the writable `rootfs.img`, and any present serial or Firecracker logs. When a log is larger than `256 MiB`, export sends only its newest `256 MiB`. Import recreates the VM under the same name on the destination host, creates destination-local runtime paths and network allocation, leaves the VM stopped, and keeps the source guest's last-known Tailscale metadata as cached state.

Because the guest's durable Tailscale identity lives in the copied rootfs, treat this as cutover or move semantics, not cloning semantics: do not boot the source and imported copies at the same time.

`snapshot create` is a separate host-local disaster-recovery primitive. It briefly blocks all SSH commands, checkpoints SQLite, flushes the filesystem, and creates a readonly btrfs snapshot of `SRV_DATA_DIR` under `SRV_DATA_DIR/.snapshots/<timestamp>`. The semantics are intentionally simple: control-plane consistent, stopped guests fully safe, and running guests crash-consistent only.

`status` is an admin-only host summary. It reports instance counts plus host CPU, memory, and disk allocation headroom. CPU is intentionally advisory so hosts can be overcommitted; memory and disk reflect the same reservation budgets that gate `new`, `start`, and grow-style `resize`.

## Host Requirements

- Linux host with cgroup v2 and `/dev/kvm`
- IPv4 forwarding enabled on the host, for example `net.ipv4.ip_forward=1`
- `SRV_DATA_DIR` on a reflink-capable filesystem, with `SRV_BASE_ROOTFS` on the same filesystem; for example `btrfs`, or `xfs` created with reflink support enabled
- `snapshot create` additionally requires `SRV_DATA_DIR` itself to be a btrfs subvolume root, not just a directory on btrfs
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
- `SRV_ZEN_API_KEY`: optional OpenCode Zen API key for the host-side guest gateway
- `SRV_ZEN_BASE_URL`: optional upstream OpenCode Zen base URL, default `https://opencode.ai/zen`
- `SRV_ZEN_GATEWAY_PORT`: TCP port exposed on each guest's host/gateway IP for the Zen proxy, default `11434`

## Validation

For a repeatable end-to-end validation pass on a prepared host, run:

```bash
sudo ./contrib/smoke/host-smoke.sh
```

That harness validates the systemd-managed `srv`, `srv-net-helper`, and `srv-vm-runner` units, confirms the SSH control surface is reachable, creates a real guest, waits for `inspect` readiness, polls for a real guest SSH session after each ready transition, verifies `list`, exercises a full stop/backup/start/restore cycle, proves restore actually rolls the guest rootfs back, checks the live per-VM cgroup limit files, and finally deletes the guest while confirming TAP, jailer, and cgroup cleanup.

When debugging a failed host run, start with `ssh srv inspect <name>`, then compare the newest lines from `ssh srv logs <name> serial`, `ssh srv logs <name> firecracker`, and `journalctl -u srv-vm-runner`.

## Architecture Notes

- `tsnet` joins the tailnet as `srv` and exposes the control API on tailnet TCP port `22`.
- `gliderlabs/ssh` handles `exec` requests and rejects shell sessions.
- SQLite stores instances, events, command audits, and authz decisions.
- Reflinks clone the base rootfs for fast per-instance writable disks.
- A root-only network helper owns TAP and iptables mutations, while a separate root-owned VM runner invokes Firecracker through the official jailer, drops the microVM process to `srv-vm:srv`, and places each VM into its own cgroup v2 leaf.
- The control plane mints a one-off Tailscale auth key for each guest and injects it through Firecracker MMDS metadata.
- When `SRV_ZEN_API_KEY` is configured, `srv` also binds a per-instance HTTP proxy on that instance's host/gateway IP and forwards `/v1/...` requests to the upstream OpenCode Zen API with the host key injected. The proxy only serves requests from that instance's guest IP, and guest bootstrap writes both `/root/.config/opencode/opencode.json` and Pi config under `/root/.pi/agent/` so the preinstalled `opencode` and `pi` CLIs target that per-VM gateway by default without storing the real Zen key inside the guest.
- `snapshot create` is an admin-only global barrier; while it is active, all other SSH commands are rejected until the local readonly btrfs snapshot has been created.
- Existing stopped guests pick up the currently configured `SRV_BASE_KERNEL` and optional `SRV_BASE_INITRD` on their next `start` or `restart`.
- Rootfs changes only affect newly created guests after you rebuild the base image artifacts and refresh `SRV_BASE_ROOTFS`.

The current Arch guest image expects a boot-time service that reads MMDS, sets the hostname, starts `tailscaled`, manages the root account's default OpenCode and Pi configs for the per-VM Zen gateway when enabled, and runs `tailscale up --auth-key=... --ssh` on the first authenticated boot only.

## Docs

- [`docs/reference/operations.md`](docs/reference/operations.md): backup, restore, rebuild, upgrade, rollback, and host hardening
- [`contrib/smoke/README.md`](contrib/smoke/README.md): smoke-test prerequisites, behavior, overrides, and artifacts
- [`docs/cheatsheet.md`](docs/cheatsheet.md): operator command reference
- [`images/arch-base/README.md`](images/arch-base/README.md): guest image builder and overlay details
- [`contrib/systemd/install.sh`](contrib/systemd/install.sh): one-shot installer for the supported host path

When regenerating the single-page manual with `go run ./cmd/srv-manual docs manual.html`, the generator classifies fenced blocks as `Command`, `Output`, `Diagram`, or `Example`. If a block needs an explicit manual label, place an override comment immediately before it:

````md
<!-- srv-manual:block=diagram -->
```text
┌──────────────────────────┐
│        srv process       │
└──────────────────────────┘
```
````

Supported override values are `command`, `output`, `diagram`, and `example`.

## Non-Goals For Now

- `srv` is intentionally a single-host control plane for this phase of the project.
- Clustering or multi-host scheduling is out of scope for now.
- High availability or control-plane replication is out of scope for now.
- A web UI is out of scope for now; SSH remains the control interface.
- Live migration is out of scope for now.
