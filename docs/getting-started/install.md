# Installation

## System requirements

- Linux host with cgroup v2 and `/dev/kvm`
- IPv4 forwarding enabled: `net.ipv4.ip_forward=1`
- `SRV_DATA_DIR` on a reflink-capable filesystem (`btrfs` or reflink-enabled `xfs`), with `SRV_BASE_ROOTFS` on the same filesystem
- Tailscale installed and working on the host
- Tailscale OAuth client credentials (`TS_CLIENT_ID` and `TS_CLIENT_SECRET`) with permission to mint auth keys for the configured guest tags
- `ip`, `iptables`, `cp`, and `resize2fs` available on the host
- Official static Firecracker and jailer release pair (the installer can download these for you)

!!! note
    srv is intentionally a single-host control plane. Clustering, multi-host scheduling, and high availability are out of scope for the current phase.

## Build

Build the three binaries from source:

```bash
go build ./cmd/srv
go build ./cmd/srv-net-helper
go build ./cmd/srv-vm-runner
```

Or install them to a standard location with the provided installer (see below).

## Build the guest image

The Arch base image builder produces the `vmlinux` kernel and `rootfs-base.img` rootfs that srv provisions from.

On an Arch Linux host:

```bash
sudo OUTPUT_DIR=/var/lib/srv/images/arch-base ./images/arch-base/build.sh
```

On a non-Arch host, use the podman workflow:

```bash
sudo podman run --rm --privileged --network host \
  -v "$PWD":/work \
  -v /var/lib/srv/images/arch-base:/var/lib/srv/images/arch-base \
  -w /work \
  docker.io/library/archlinux:latest \
  bash -lc '
    set -euo pipefail
    pacman -Sy --noconfirm archlinux-keyring
    pacman -Syu --noconfirm arch-install-scripts base-devel bc e2fsprogs rsync curl systemd
    OUTPUT_DIR=/var/lib/srv/images/arch-base ./images/arch-base/build.sh
  '
```

See [Building a custom guest image](../tasks/build-custom-image.md) and the [guest image reference](../reference/guest-image.md) for details on what the image includes and how to customize it.

## Install and configure

The systemd installer handles binary installation, unit setup, and downloading the official static Firecracker/jailer pair:

```bash
sudo ./contrib/systemd/install.sh
```

Enable IPv4 forwarding:

```bash
sudo tee /etc/sysctl.d/90-srv-ip-forward.conf >/dev/null <<'EOF'
net.ipv4.ip_forward = 1
EOF
sudo sysctl --system
```

Write the environment file. At minimum you need Tailscale credentials and guest artifact paths:

```bash
sudoedit /etc/srv/srv.env
```

Key entries to fill in:

```bash
TS_AUTHKEY=tskey-auth-xxxxxxxxxxxxxxxx
TS_CLIENT_ID=your-oauth-client-id
TS_CLIENT_SECRET=your-oauth-client-secret
TS_TAILNET=your-tailnet.ts.net
SRV_BASE_KERNEL=/var/lib/srv/images/arch-base/vmlinux
SRV_BASE_ROOTFS=/var/lib/srv/images/arch-base/rootfs-base.img
SRV_GUEST_AUTH_TAGS=tag:microvm
```

See the full [configuration reference](../reference/configuration.md) for every variable.

Then start the services:

```bash
sudo ./contrib/systemd/install.sh --enable-now
```

## Validate

Run the end-to-end smoke test to confirm everything works:

```bash
sudo ./contrib/smoke/host-smoke.sh
```

The smoke test verifies systemd units, SSH reachability, guest creation, readiness polling, backup/restore, cgroup limits, and cleanup. It is the supported validation gate after install, restore, and upgrade.

See the [smoke test reference](../reference/troubleshooting.md#smoke-test) for details on overrides and failure artifacts.

## Next steps

- [Walkthrough](walkthrough.md) — create your first VM
- [Running as a daemon](running-as-daemon.md) — systemd details