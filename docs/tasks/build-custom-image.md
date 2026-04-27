# Build a custom guest image

The default guest image is an Arch Linux rootfs with Docker, Go, Neovim, OpenCode, Pi, and common development tools. You can customize it by modifying the overlay or building a completely different rootfs.

## Default image builder

The `images/arch-base/` directory contains the official builder:

```bash
sudo OUTPUT_DIR=/var/lib/srv/images/arch-base ./images/arch-base/build.sh
```

This produces two artifacts:

- `vmlinux` — an x86_64 Firecracker-compatible kernel built from the 6.12 LTS source
- `rootfs-base.img` — a sparse ext4 image populated via `pacstrap`

### What the image includes

- Docker, docker-compose
- Go, gopls, Odin, odinfmt, OLS
- Neovim with a prewarmed LazyVim config (BMW heritage amber theme)
- OpenCode and Pi CLIs with per-VM provider gateway bootstrap
- Git, fd, ripgrep, tree-sitter-cli, gcc, perf, valgrind
- iptables-nft with IPv4/IPv6 nftables support
- `srv-bootstrap.service` for Tailscale and MMDS setup

### Build overrides

```bash
# Change kernel version
sudo KERNEL_VERSION=6.1.167 OUTPUT_DIR=/var/lib/srv/images/arch-base ./images/arch-base/build.sh

# Change rootfs size (default 10G)
ROOTFS_SIZE=20G sudo OUTPUT_DIR=/var/lib/srv/images/arch-base ./images/arch-base/build.sh

# Reduce kernel build parallelism
sudo KERNEL_JOBS=2 OUTPUT_DIR=/var/lib/srv/images/arch-base ./images/arch-base/build.sh
```

## Podman build on non-Arch hosts

If your host is not Arch Linux and does not provide `pacstrap`:

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

- `--privileged` is required because the builder uses `losetup`, `mkfs.ext4`, and `mount`
- `--network host` keeps mirror and kernel downloads simple

## Customizing the overlay

The `images/arch-base/overlay/` directory contains files that are copied into the guest rootfs during the build. Changes here only affect new guests after you rebuild `rootfs-base.img` and refresh `SRV_BASE_ROOTFS`.

After modifying the overlay:

```bash
sudo OUTPUT_DIR=/var/lib/srv/images/arch-base ./images/arch-base/build.sh
# Then update SRV_BASE_ROOTFS in /etc/srv/srv.env to point at the new image
```

## Rolling out a new image

Rootfs changes only affect newly created guests. Existing guests keep their own writable `rootfs.img`. There is no host-driven in-place rootfs update.

To migrate an existing guest to a new base image:

1. Rebuild and update `SRV_BASE_ROOTFS`
2. Create a new guest with `ssh srv new <name>`
3. Migrate workload data to the new guest
4. Delete the old guest

Or manage the existing guest locally with `pacman -Syu`, accepting guest-local drift.

## Kernel rollout

Kernel roll-forward is simpler. Existing stopped guests pick up the currently configured `SRV_BASE_KERNEL` and optional `SRV_BASE_INITRD` on their next `start` or `restart`:

1. Rebuild the kernel artifact
2. Update `SRV_BASE_KERNEL` (and `SRV_BASE_INITRD` if applicable) in `/etc/srv/srv.env`
3. Restart the units if needed
4. Stop and start guests, or let stopped guests pick up the new kernel on their next start

Rollback: point the path back to the previous artifact and restart guests again.
