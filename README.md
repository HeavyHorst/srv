# srv

Self-hosted control-plane service for creating Firecracker microVMs over SSH on a Tailscale tailnet.

Target UX:

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

## Current Supported Shape

- `tsnet` joins the tailnet as `srv` and exposes the control API on tailnet TCP port `22`.
- `gliderlabs/ssh` handles `exec` requests and rejects shell sessions.
- SQLite stores instances, events, command audits, and authz decisions.
- Btrfs reflinks clone the base rootfs for fast per-instance writable disks.
- Firecracker launches each VM with a TAP device and host-side NAT.
- A small root-only network helper owns TAP and iptables mutations, while a separate root-owned VM runner service invokes Firecracker through the official jailer, drops the microVM process to `srv-vm:srv`, enforces per-VM cgroup v2 CPU, memory, swap, and pid limits, and derives host runtime paths from `SRV_DATA_DIR/instances`.
- The control plane mints a one-off Tailscale auth key for each guest and injects it through Firecracker MMDS metadata.
- The prepared-host path is the supported path today: systemd-managed `srv`, `srv-net-helper`, and `srv-vm-runner` on a Linux host with root privileges, Tailscale, cgroup v2, and `/dev/kvm`.
- `new`, `resize`, `list`, `inspect`, `logs`, `start`, `stop`, `restart`, and `delete` work end to end on that real host path.
- The host smoke harness is the current validation bar for that behavior: it checks control-plane SSH reachability, guest creation, `inspect` readiness, guest SSH after each ready transition, `list`, a full stop/start cycle, and delete against a real guest.
- Graceful stop now follows Firecracker's `SendCtrlAltDel` path against the Arch guest image instead of relying on forced-kill fallback, and warm start/restart reuses persisted guest `tailscaled` state.
- When `srv` starts under systemd after a host reboot, previously active instances are restarted automatically.
- Instance creators can see and manage their own instances by default, while optional admin users can see and manage every instance.

## Non-Goals For Now

- `srv` is intentionally a single-host control plane for this phase of the project.
- Clustering or multi-host scheduling is out of scope for now.
- High availability or control-plane replication is out of scope for now.
- A web UI is out of scope for now; SSH remains the control interface.
- Live migration is out of scope for now.

Operational maturity for the prepared-host path now has a concrete runbook instead of scattered caveats.

## Operations Runbook

- [docs/operations.md](file:///home/rene/Code/srv/docs/operations.md) consolidates backup, restore, rebuild, upgrade, rollback, smoke-gated validation, and host hardening expectations for the supported prepared-host path.

## Operational Bar

- A host can be rebuilt from the docs and a backup, with guests coming back cleanly.
- Guest image and schema upgrades have a repeatable rollout and rollback story.
- Per-VM CPU, memory, and relevant disk or process limits are enforced, not just configured at boot time.
- The prepared-host smoke pass is part of the normal upgrade and recovery workflow, not just ad hoc validation.
- The current caveats are explicitly owned in the ops docs: cgroup v2 only, the static Firecracker and jailer release pairing, preserved `/etc/srv/srv.env` paths across reinstall, and guest image rebuild requirements.

## Instance Lifecycle Notes

- `new` accepts `--cpus`, `--ram`, and `--rootfs-size` to set per-instance sizing at creation time.
- `resize` accepts the same sizing flags, but only for instances in the `stopped` state.
- CPU and memory changes are persisted on the instance record and take effect on the next `start` or `restart`.
- Existing stopped guests pick up the currently configured `SRV_BASE_KERNEL` and optional `SRV_BASE_INITRD` on the next `start` or `restart`, so you can roll boot artifacts forward without recreating the writable guest disk.
- Rootfs resizing is grow-only. Requests smaller than the current disk image are rejected.
- Rootfs growth happens on the host by expanding the disk image and filesystem before the next boot; live resize is not supported.
- Each instance's writable disk lives at `SRV_DATA_DIR/instances/<name>/rootfs.img`; the configured base kernel and base rootfs remain separate shared inputs.
- `logs <name>` shows recent serial and Firecracker log output remotely, or a single surface with `logs <name> serial` / `logs <name> firecracker`.

## Authz Model

- `SRV_ALLOWED_USERS` controls who may invoke the service at all. If it is empty, any reachable tailnet user may issue commands.
- Instance creators can list, inspect, resize, start, stop, restart, delete, and read logs for their own instances.
- `SRV_ADMIN_USERS` grants cross-instance visibility and management for operators who need to act across the whole host.

## Host Requirements

- Linux host with cgroup v2 and `/dev/kvm`
- Firecracker and jailer installed, or let `contrib/systemd/install.sh` download the official static release pair into `/usr/local/bin`; the prepared-host path is currently validated against the official `v1.15.0` release pair
- The jailer path expects a statically linked Firecracker binary compatible with the official musl release builds; dynamically linked distro builds can fail after chroot before the API socket appears
- If `/etc/srv/srv.env` already exists, confirm `SRV_FIRECRACKER_BIN` and `SRV_JAILER_BIN` point at the intended static binaries after running the installer; the installer keeps existing env files unless you opt into overwriting them
- `ip`, `iptables`, `cp`, `resize2fs`, and `stat` available on the host
- `SRV_DATA_DIR` on Btrfs
- Tailscale tailnet access for the control plane
- Tailscale OAuth client credentials with permission to mint auth keys for the configured guest tags

## Key Environment Variables

- `TS_AUTHKEY` or `TS_CLIENT_SECRET` / `TS_CLIENT_ID`: control-plane Tailscale credentials
- `TS_TAILNET`: tailnet name used for guest auth-key minting
- `SRV_ALLOWED_USERS`: optional comma-separated Tailscale login allowlist
- `SRV_ADMIN_USERS`: optional comma-separated Tailscale login list with cross-instance visibility and management rights
- `SRV_BASE_KERNEL`: Firecracker guest kernel image
- `SRV_BASE_INITRD`: optional initrd image
- `SRV_BASE_ROOTFS`: base guest rootfs image stored on Btrfs
- `SRV_GUEST_AUTH_TAGS`: comma-separated tags applied to guest auth keys
- `SRV_DATA_DIR`: host state directory, default `/var/lib/srv`
- `SRV_NET_HELPER_SOCKET`: unix socket used to reach the privileged host-network helper, default `/run/srv/net-helper.sock`
- `SRV_VM_RUNNER_SOCKET`: unix socket used to reach the separate root-owned Firecracker runner helper, default `/run/srv-vm-runner/vm-runner.sock`
- `SRV_JAILER_BIN`: Firecracker jailer binary used by the VM runner, default `/usr/bin/jailer`
- `SRV_JAILER_BASE_DIR`: base directory for jailer workspaces, default `SRV_DATA_DIR/jailer`
- `SRV_VM_NETWORK_CIDR`: host-side private network pool for guest TAP allocations, default `172.28.0.0/16`
- `SRV_VM_PIDS_MAX`: maximum tasks allowed in each VM cgroup, default `512`
- `SRV_OUTBOUND_IFACE`: optional override for the host interface used for NAT

## Run

```bash
go build ./cmd/srv
sudo ./srv \
  -data-dir /var/lib/srv \
  -hostname srv \
  -tailnet your-tailnet.ts.net \
  -base-kernel /var/lib/srv/images/arch-base/vmlinux \
  -base-rootfs /var/lib/srv/images/arch-base/rootfs-base.img \
  -guest-auth-tags tag:microvm
```

When running outside systemd, start both helpers separately so `srv` can reach `SRV_NET_HELPER_SOCKET` and `SRV_VM_RUNNER_SOCKET`. The systemd path below is the recommended way to keep the non-root control plane, the root-only network helper, and the separate root-owned Firecracker runner wired together. Build those helpers with `go build ./cmd/srv-net-helper ./cmd/srv-vm-runner` before launching them manually.

## Run Under Systemd

Example systemd assets live in [contrib/systemd/srv.service](file:///home/rene/Code/srv/contrib/systemd/srv.service), [contrib/systemd/srv-net-helper.service](file:///home/rene/Code/srv/contrib/systemd/srv-net-helper.service), [contrib/systemd/srv-vm-runner.service](file:///home/rene/Code/srv/contrib/systemd/srv-vm-runner.service), and [contrib/systemd/srv.env.example](file:///home/rene/Code/srv/contrib/systemd/srv.env.example).

For a one-shot install, use [contrib/systemd/install.sh](file:///home/rene/Code/srv/contrib/systemd/install.sh):

```bash
sudo ./contrib/systemd/install.sh
sudoedit /etc/srv/srv.env
sudo ./contrib/systemd/install.sh --enable-now
```

That installer now downloads the matching official Firecracker and jailer release tarball, verifies its published SHA256 sidecar, and installs the static binaries under `/usr/local/bin` by default.

If `/etc/srv/srv.env` already exists, the installer keeps it by default. On upgraded hosts, verify that `SRV_FIRECRACKER_BIN` and `SRV_JAILER_BIN` were not left pointing at older distro binaries under `/usr/bin`.

Install the binaries, units, and environment file like this:

```bash
go build -o /usr/local/bin/srv ./cmd/srv
go build -o /usr/local/bin/srv-net-helper ./cmd/srv-net-helper
go build -o /usr/local/bin/srv-vm-runner ./cmd/srv-vm-runner
curl -L https://github.com/firecracker-microvm/firecracker/releases/download/v1.15.0/firecracker-v1.15.0-$(uname -m).tgz | tar -xz
sudo install -m 0755 release-v1.15.0-$(uname -m)/firecracker-v1.15.0-$(uname -m) /usr/local/bin/firecracker
sudo install -m 0755 release-v1.15.0-$(uname -m)/jailer-v1.15.0-$(uname -m) /usr/local/bin/jailer
sudo install -d -m 0755 /etc/srv
sudo install -m 0644 contrib/systemd/srv.service /etc/systemd/system/srv.service
sudo install -m 0644 contrib/systemd/srv-net-helper.service /etc/systemd/system/srv-net-helper.service
sudo install -m 0644 contrib/systemd/srv-vm-runner.service /etc/systemd/system/srv-vm-runner.service
sudo install -m 0640 contrib/systemd/srv.env.example /etc/srv/srv.env
sudoedit /etc/srv/srv.env
sudo systemctl daemon-reload
sudo systemctl enable --now srv
```

Under systemd, the main `srv` unit runs as the dedicated `srv` service user, the root-owned network helper owns host-side TAP and firewall mutation, and the separate root-owned VM runner invokes Firecracker through the official jailer before dropping the microVM process to `srv-vm:srv`. The runner keeps itself in a delegated `supervisor/` cgroup, launches each VM directly into a constrained `firecracker-vms/<name>/` leaf cgroup, and still derives each VM's `rootfs.img`, logs, and Firecracker socket from `SRV_DATA_DIR/instances/<name>/` instead of accepting caller-supplied host paths. The jailer builds its chroot workspaces under `SRV_JAILER_BASE_DIR`. Keep `SRV_DATA_DIR` on Btrfs and point `SRV_BASE_KERNEL` and `SRV_BASE_ROOTFS` at the artifacts built under [images/arch-base/](file:///home/rene/Code/srv/images/arch-base/README.md).

The `srv-vm-runner` unit is expected to run as `User=root`, `Group=srv`, and `Delegate=cpu memory pids` so the control plane can reach `/run/srv-vm-runner/vm-runner.sock` while the jailer still has the privileges it needs to set up the microVM sandbox and program per-VM cgroup limits. In particular, do not add `NoNewPrivileges=yes` to that unit: the jailer must drop to the configured `srv-vm:srv` identity and `exec` Firecracker on real hosts.

## Host-Level Smoke Test

For a repeatable end-to-end validation pass on a prepared host, run [contrib/smoke/host-smoke.sh](contrib/smoke/host-smoke.sh):

```bash
sudo ./contrib/smoke/host-smoke.sh
```

For install, restore, or upgrade validation, use the stricter gate from [docs/operations.md](file:///home/rene/Code/srv/docs/operations.md):

```bash
STRICT_HOST_ASSERTIONS=1 sudo ./contrib/smoke/host-smoke.sh
```

That harness is the current validation bar for prepared hosts. It validates the systemd-managed `srv`, `srv-net-helper`, and `srv-vm-runner` units, confirms the SSH control surface is reachable, creates a real guest, waits for `inspect` readiness, polls for a real guest SSH session after each ready transition, verifies `list`, exercises a stop/start cycle, and finally deletes the guest. With `STRICT_HOST_ASSERTIONS=1`, it also checks the live per-VM cgroup limit files and the cleanup of TAP, jailer, and cgroup artifacts after stop/delete. On failure it collects `inspect`, `logs`, `systemctl status`, and `journalctl` artifacts automatically. See [contrib/smoke/README.md](contrib/smoke/README.md) for prerequisites, environment overrides, and artifact locations.

When debugging a failed host run, start with `ssh root@srv inspect <name>`, then compare the newest lines from `ssh root@srv logs <name> serial`, `ssh root@srv logs <name> firecracker`, and `journalctl -u srv-vm-runner`. The serial and Firecracker log files are append-only, so the latest lines are the trustworthy ones for the current failure.

## Build The Arch Base Image

The repo now includes an Arch guest image builder under `images/arch-base/`.

```bash
sudo OUTPUT_DIR=/var/lib/srv/images/arch-base ./images/arch-base/build.sh
```

That builder compiles a Firecracker-compatible `vmlinux`, pacstraps an Arch rootfs image, and installs the guest boot-time bootstrap service described below. The bootstrap service runs on every boot, starts `tailscaled`, and only performs `tailscale up --auth-key=... --ssh` on the first authenticated boot. The kernel fragment is also tuned for Firecracker's current x86 ACPI boot path and includes the minimal x86 keyboard-controller/input stack needed for graceful `SendCtrlAltDel` shutdowns. Rebuilt kernels can be picked up by existing stopped guests on their next `start` or `restart` once the host's configured `SRV_BASE_KERNEL` is refreshed. Rootfs changes under `images/arch-base/overlay/` still only reach newly created guests after you rebuild the artifacts and refresh `SRV_BASE_ROOTFS`. See `images/arch-base/README.md` for details.

## Guest Bootstrap Contract

The service injects Firecracker MMDS JSON like this:

```json
{
  "srv": {
    "version": 1,
    "hostname": "demo",
    "tailscale_auth_key": "tskey-auth-...",
    "tailscale_control_url": "",
    "tailscale_tags": ["tag:microvm"]
  }
}
```

The Arch guest image is expected to include a boot-time service that runs on every boot and:

1. Discovers the primary virtio NIC from the kernel-provided default route
2. Adds a `169.254.169.254/32` route over that NIC
3. Reads the MMDS payload from `http://169.254.169.254/` while requesting JSON output
4. Sets the hostname from `srv.hostname`
5. Starts `tailscaled`
6. Runs `tailscale up --auth-key=... --ssh` on the first authenticated boot and relies on persisted `tailscaled` state on later boots
7. Deletes any transient copy of the key it created locally

The control plane does not mutate the guest disk directly in this version; it relies on the image consuming the MMDS contract.
