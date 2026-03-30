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

## Current MVP Shape

- `tsnet` joins the tailnet as `srv` and exposes the control API on tailnet TCP port `22`.
- `gliderlabs/ssh` handles `exec` requests and rejects shell sessions.
- SQLite stores instances, events, command audits, and authz decisions.
- Btrfs reflinks clone the base rootfs for fast per-instance writable disks.
- Firecracker launches each VM with a TAP device and host-side NAT.
- A small root-only network helper owns TAP and iptables mutations, while a separate root-owned VM runner service invokes Firecracker through the official jailer, drops the microVM process to `srv-vm:srv`, manages per-VM cgroups, and derives host runtime paths from `SRV_DATA_DIR/instances`.
- The control plane mints a one-off Tailscale auth key for each guest and injects it through Firecracker MMDS metadata.
- `new`, `resize`, `list`, `inspect`, `start`, `stop`, `restart`, and `delete` are implemented.
- When `srv` starts under systemd after a host reboot, previously active instances are restarted automatically.

## Non-Goals For Now

- `srv` is intentionally a single-host control plane for this phase of the project.
- Clustering or multi-host scheduling is out of scope for now.
- High availability or control-plane replication is out of scope for now.
- A web UI is out of scope for now; SSH remains the control interface.
- Live migration is out of scope for now.

Moving past MVP here means making the single-host service safer to share and easier to operate, not turning it into a general-purpose cloud platform.

## Post-MVP Checklist

Ordered by current impact for this single-host design, with effort called out so the next steps stay pragmatic:

1. Ownership-aware authz and visibility. High impact, medium effort. Instance creators should not automatically be able to manage every instance just because they can reach the service; owner-by-default behavior with an explicit admin path is the clearest first step past MVP.
2. Better operator debugging. High impact, low effort. Add remote access to serial and Firecracker logs, and make `inspect` point directly at the relevant failure surface when a guest is stuck in `awaiting-tailnet` or fails during boot.
3. Backup, restore, and upgrade runbook. High impact, medium effort. Define and test how to back up `SRV_DATA_DIR`, recover a host, and roll forward guest image or schema changes without improvisation.
4. Host-level smoke test. Medium-high impact, medium effort. Add one repeatable end-to-end validation path that exercises the systemd units, both helpers, the jailer, guest tailnet join, and teardown on a real host.
5. Service hardening and resource controls. Medium impact, medium effort. Tighten the privileged helpers further, document the expected host security posture, and make per-VM resource limits part of normal operations.

## Instance Lifecycle Notes

- `new` accepts `--cpus`, `--ram`, and `--rootfs-size` to set per-instance sizing at creation time.
- `resize` accepts the same sizing flags, but only for instances in the `stopped` state.
- CPU and memory changes are persisted on the instance record and take effect on the next `start` or `restart`.
- Rootfs resizing is grow-only. Requests smaller than the current disk image are rejected.
- Rootfs growth happens on the host by expanding the disk image and filesystem before the next boot; live resize is not supported.

## Host Requirements

- Linux host with `/dev/kvm`
- Firecracker and jailer installed, default paths `/usr/bin/firecracker` and `/usr/bin/jailer`
- `ip`, `iptables`, `cp`, `resize2fs`, and `stat` available on the host
- `SRV_DATA_DIR` on Btrfs
- Tailscale tailnet access for the control plane
- Tailscale OAuth client credentials with permission to mint auth keys for the configured guest tags

## Key Environment Variables

- `TS_AUTHKEY` or `TS_CLIENT_SECRET` / `TS_CLIENT_ID`: control-plane Tailscale credentials
- `TS_TAILNET`: tailnet name used for guest auth-key minting
- `SRV_ALLOWED_USERS`: optional comma-separated Tailscale login allowlist
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

Install the binaries, units, and environment file like this:

```bash
go build -o /usr/local/bin/srv ./cmd/srv
go build -o /usr/local/bin/srv-net-helper ./cmd/srv-net-helper
go build -o /usr/local/bin/srv-vm-runner ./cmd/srv-vm-runner
sudo install -d -m 0755 /etc/srv
sudo install -m 0644 contrib/systemd/srv.service /etc/systemd/system/srv.service
sudo install -m 0644 contrib/systemd/srv-net-helper.service /etc/systemd/system/srv-net-helper.service
sudo install -m 0644 contrib/systemd/srv-vm-runner.service /etc/systemd/system/srv-vm-runner.service
sudo install -m 0640 contrib/systemd/srv.env.example /etc/srv/srv.env
sudoedit /etc/srv/srv.env
sudo systemctl daemon-reload
sudo systemctl enable --now srv
```

Under systemd, the main `srv` unit runs as the dedicated `srv` service user, the root-owned network helper owns host-side TAP and firewall mutation, and the separate root-owned VM runner invokes Firecracker through the official jailer before dropping the microVM process to `srv-vm:srv`. The runner still derives each VM's `rootfs.img`, logs, and Firecracker socket from `SRV_DATA_DIR/instances/<name>/` instead of accepting caller-supplied host paths, while the jailer builds its chroot workspaces under `SRV_JAILER_BASE_DIR`. Keep `SRV_DATA_DIR` on Btrfs and point `SRV_BASE_KERNEL` and `SRV_BASE_ROOTFS` at the artifacts built under [images/arch-base/](file:///home/rene/Code/srv/images/arch-base/README.md).

## Build The Arch Base Image

The repo now includes an Arch guest image builder under `images/arch-base/`.

```bash
sudo OUTPUT_DIR=/var/lib/srv/images/arch-base ./images/arch-base/build.sh
```

That builder compiles a Firecracker-compatible `vmlinux`, pacstraps an Arch rootfs image, and installs the guest first-boot bootstrap service described below. The kernel fragment is tuned for Firecracker's current x86 ACPI boot path, so the built guest can boot its root block device without falling back to deprecated legacy discovery. See `images/arch-base/README.md` for details.

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

The Arch guest image is expected to include a first-boot service that:

1. Discovers the primary virtio NIC from the kernel-provided default route
2. Adds a `169.254.169.254/32` route over that NIC
3. Reads the MMDS payload from `http://169.254.169.254/` while requesting JSON output
4. Sets the hostname from `srv.hostname`
5. Starts `tailscaled`
6. Runs `tailscale up --auth-key=... --ssh`
7. Deletes any transient copy of the key it created locally

The control plane does not mutate the guest disk directly in this version; it relies on the image consuming the MMDS contract.
