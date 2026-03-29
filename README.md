# srv

Self-hosted control-plane service for creating Firecracker microVMs over SSH on a Tailscale tailnet.

Target UX:

```bash
ssh root@srv new demo
```

The service treats SSH as command transport only. Caller identity comes from Tailscale `WhoIs` data resolved from the incoming tailnet connection.

## Current MVP Shape

- `tsnet` joins the tailnet as `srv` and exposes the control API on tailnet TCP port `22`.
- `gliderlabs/ssh` handles `exec` requests and rejects shell sessions.
- SQLite stores instances, events, command audits, and authz decisions.
- Btrfs reflinks clone the base rootfs for fast per-instance writable disks.
- Firecracker launches each VM with a TAP device and host-side NAT.
- The control plane mints a one-off Tailscale auth key for each guest and injects it through Firecracker MMDS metadata.
- `new`, `list`, `inspect`, `start`, `stop`, `restart`, and `delete` are implemented.
- When `srv` starts under systemd after a host reboot, previously active instances are restarted automatically.

## Host Requirements

- Linux host with `/dev/kvm`
- Firecracker installed, default path `/usr/bin/firecracker`
- `ip`, `iptables`, `cp`, `stat`, and `sysctl` available on the host
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

## Run Under Systemd

Example systemd assets live in [contrib/systemd/srv.service](file:///home/rene/Work/Code/srv/contrib/systemd/srv.service) and [contrib/systemd/srv.env.example](file:///home/rene/Work/Code/srv/contrib/systemd/srv.env.example).

For a one-shot install, use [contrib/systemd/install.sh](file:///home/rene/Work/Code/srv/contrib/systemd/install.sh):

```bash
sudo ./contrib/systemd/install.sh
sudoedit /etc/srv/srv.env
sudo ./contrib/systemd/install.sh --enable-now
```

Install the binary, unit, and environment file like this:

```bash
go build -o /usr/local/bin/srv ./cmd/srv
sudo install -d -m 0755 /etc/srv
sudo install -m 0644 contrib/systemd/srv.service /etc/systemd/system/srv.service
sudo install -m 0640 contrib/systemd/srv.env.example /etc/srv/srv.env
sudoedit /etc/srv/srv.env
sudo systemctl daemon-reload
sudo systemctl enable --now srv
```

The unit runs `srv` as root because the service needs KVM, TAP device creation, and host NAT management. Keep `SRV_DATA_DIR` on Btrfs and point `SRV_BASE_KERNEL` and `SRV_BASE_ROOTFS` at the artifacts built under [images/arch-base/](file:///home/rene/Work/Code/srv/images/arch-base/README.md).

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
