#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
OVERLAY="${SCRIPT_DIR}/../arch-base/overlay"
OUTPUT_DIR="${OUTPUT_DIR:-${SCRIPT_DIR}/out}"
ROOTFS_SIZE="${ROOTFS_SIZE:-4G}"
DEBIAN_MIRROR="${DEBIAN_MIRROR:-https://deb.debian.org/debian}"
KERNEL_IMAGE="${KERNEL_IMAGE:-}"
KERNEL_MODULES_DIR="${KERNEL_MODULES_DIR:-}"
WORK=""
PACKAGES=(systemd-sysv systemd-resolved ca-certificates curl git jq tmux
  openssh-client iproute2 iptables kmod docker.io docker-cli containerd
  bash-completion less procps iputils-ping)
# Also works with Debian 12's Docker 20.10 when adapting this builder to bookworm.
# Keep the version and upstream linux/amd64 checksum together when updating.
COMPOSE_VERSION=2.24.7
COMPOSE_SHA256=19c9deb6f4d3915f5c93441b8d2da751a09af82df62d55eab097c2cbfebd519f

die() { echo "$*" >&2; exit 1; }
cleanup() {
  local status=$?
  trap - EXIT
  if [[ -n "$WORK" ]]; then
    # Never delete through a live bind mount if unmounting fails.
    if mountpoint -q "$WORK/rootfs/dev"; then
      umount -R "$WORK/rootfs/dev" || exit 1
    fi
    if mountpoint -q "$WORK/rootfs/proc"; then
      umount "$WORK/rootfs/proc" || exit 1
    fi
    rm -rf -- "$WORK"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

[[ $(id -u) == 0 ]] || die 'run as root (debootstrap, chroot, device nodes and mounts require privileges)'
[[ $(uname -m) == x86_64 ]] || die 'only native amd64 builds are supported'
for cmd in debootstrap mkfs.ext4 chroot mount mountpoint umount systemctl \
  curl modinfo grep find truncate sha256sum; do
  command -v "$cmd" >/dev/null || die "missing required command: $cmd"
done
[[ -f "$KERNEL_IMAGE" ]] || die 'set KERNEL_IMAGE to the shared Arch builder vmlinux'
[[ -d "$KERNEL_MODULES_DIR" ]] || die 'set KERNEL_MODULES_DIR to its matching lib/modules/<release> directory'
KERNEL_MODULES_DIR="$(realpath "$KERNEL_MODULES_DIR")"
KERNEL_IMAGE="$(realpath "$KERNEL_IMAGE")"
KERNEL_RELEASE="$(basename "$KERNEL_MODULES_DIR")"
[[ "$KERNEL_RELEASE" =~ ^[0-9][A-Za-z0-9._+-]*$ ]] || die 'invalid kernel release directory name'
grep -aqF "Linux version $KERNEL_RELEASE " "$KERNEL_IMAGE" || die 'vmlinux release does not match the module directory'
MODULE="$(find "$KERNEL_MODULES_DIR" -type f \( -name '*.ko' -o -name '*.ko.xz' -o -name '*.ko.zst' \) -print -quit)"
[[ -n "$MODULE" ]] || die 'module tree contains no loadable modules'
[[ $(modinfo -F vermagic "$MODULE") == "$KERNEL_RELEASE "* ]] || die 'module vermagic does not match the kernel release'

mkdir -p "$OUTPUT_DIR"
OUTPUT_DIR="$(realpath "$OUTPUT_DIR")"
WORK="$(mktemp -d "$OUTPUT_DIR/.build.XXXXXX")"
ROOT="$WORK/rootfs"
debootstrap --arch=amd64 --variant=minbase trixie "$ROOT" "$DEBIAN_MIRROR"
# Suppress package postinst service starts; these must not affect the host.
printf '#!/bin/sh\nexit 101\n' > "$ROOT/usr/sbin/policy-rc.d"
chmod 0755 "$ROOT/usr/sbin/policy-rc.d"
mount -t proc proc "$ROOT/proc"
mount --rbind /dev "$ROOT/dev"
mount --make-rslave "$ROOT/dev"
chroot "$ROOT" /usr/bin/env DEBIAN_FRONTEND=noninteractive apt-get update
chroot "$ROOT" /usr/bin/env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${PACKAGES[@]}"
# Install the CLI plugin system-wide so root and Amp's user discover the same
# version. Debian 12's docker-compose package is the incompatible Python v1 CLI.
curl -fsSL --retry 3 \
  "https://github.com/docker/compose/releases/download/v${COMPOSE_VERSION}/docker-compose-linux-x86_64" \
  -o "$WORK/docker-compose"
printf '%s  %s\n' "$COMPOSE_SHA256" "$WORK/docker-compose" | sha256sum -c -
install -D -m 0755 "$WORK/docker-compose" "$ROOT/usr/local/lib/docker/cli-plugins/docker-compose"
[[ $(chroot "$ROOT" docker compose version --short) == "$COMPOSE_VERSION" ]] || die 'Docker Compose plugin validation failed'
if [[ "${SRV_AMP_ORB:-0}" != 1 ]]; then
install -d "$ROOT/usr/share/keyrings" "$ROOT/etc/apt/sources.list.d"
curl -fsSL https://pkgs.tailscale.com/stable/debian/trixie.noarmor.gpg \
  -o "$ROOT/usr/share/keyrings/tailscale-archive-keyring.gpg"
echo 'deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/debian trixie main' \
  > "$ROOT/etc/apt/sources.list.d/tailscale.list"
chroot "$ROOT" /usr/bin/env DEBIAN_FRONTEND=noninteractive apt-get update
chroot "$ROOT" /usr/bin/env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends tailscale
fi

if [[ "${SRV_AMP_ORB:-0}" == 1 ]]; then
  (
    guest_script="$(mktemp "$ROOT/root/prepare-amp.XXXXXX")"
    trap 'rm -f "$guest_script"' EXIT
    install -o root -g root -m 0700 "$SCRIPT_DIR/../../contrib/amp/orbs/prepare-guest.sh" "$guest_script"
    chroot "$ROOT" /bin/bash "${guest_script#"$ROOT"}"
  )
fi

install -d "$ROOT/lib/modules/$KERNEL_RELEASE"
cp -a "$KERNEL_MODULES_DIR/." "$ROOT/lib/modules/$KERNEL_RELEASE/"
rm -f "$ROOT/lib/modules/$KERNEL_RELEASE/"{build,source}
chroot "$ROOT" depmod "$KERNEL_RELEASE"
# Reuse only the guest contract, not Arch's editor/agent configuration.
cp -a "$OVERLAY/etc/." "$ROOT/etc/"
install -D -m 0755 "$OVERLAY/usr/local/lib/srv/bootstrap.sh" "$ROOT/usr/local/lib/srv/bootstrap.sh"
install -d "$ROOT/var/lib/srv"
systemctl --root="$ROOT" enable systemd-resolved docker.socket
if [[ "${SRV_AMP_ORB:-0}" == 1 ]]; then
  install -D -m 0755 "$SCRIPT_DIR/../../contrib/amp/orbs/guest-agent.py" "$ROOT/usr/local/lib/srv/orb-guest.py"
  install -D -m 0644 "$SCRIPT_DIR/../../contrib/amp/orbs/srv-orb-guest.service" "$ROOT/etc/systemd/system/srv-orb-guest.service"
  rm -f "$ROOT/etc/systemd/system/srv-bootstrap.service"
  systemctl --root="$ROOT" mask srv-bootstrap.service
  systemctl --root="$ROOT" enable srv-orb-guest.service
else
  systemctl --root="$ROOT" enable tailscaled srv-bootstrap
fi
systemctl --root="$ROOT" disable docker.service containerd.service
systemctl --root="$ROOT" mask getty.target getty@.service serial-getty@.service \
  systemd-udevd.service systemd-udevd-control.socket systemd-udevd-kernel.socket \
  systemd-udev-trigger.service systemd-udev-settle.service systemd-hwdb-update.service
systemctl --root="$ROOT" set-default multi-user.target
# debootstrap copies the build host's /etc/hostname; guests must not inherit it.
rm -f "$ROOT/usr/sbin/policy-rc.d" "$ROOT/etc/resolv.conf" "$ROOT/etc/hostname" "$ROOT/var/lib/dbus/machine-id"
ln -s /run/systemd/resolve/stub-resolv.conf "$ROOT/etc/resolv.conf"
truncate -s 0 "$ROOT/etc/machine-id"
chroot "$ROOT" apt-get clean
rm -rf "$ROOT/var/lib/apt/lists/"*
chroot "$ROOT" dpkg-query -W > "$WORK/packages.txt"
umount -R "$ROOT/dev"
umount "$ROOT/proc"

# Populate ext4 directly from the staging tree: no partition table or initramfs.
truncate -s "$ROOTFS_SIZE" "$WORK/rootfs-base.img"
mkfs.ext4 -q -F -L srv-root -d "$ROOT" "$WORK/rootfs-base.img"
cp "$KERNEL_IMAGE" "$WORK/vmlinux"
{
  echo 'distribution=debian'
  echo 'release=13'
  echo 'suite=trixie'
  echo 'arch=amd64'
  echo "kernel_release=$KERNEL_RELEASE"
  echo "debian_mirror=$DEBIAN_MIRROR"
  echo "rootfs_size=$ROOTFS_SIZE"
  echo "docker_compose_version=$COMPOSE_VERSION"
  echo "docker_compose_sha256=$COMPOSE_SHA256"
  if [[ "${SRV_AMP_ORB:-0}" == 1 ]]; then
    echo 'srv_amp_orb=1'
  fi
  (cd "$WORK" && sha256sum vmlinux rootfs-base.img)
  cat "$WORK/packages.txt"
} > "$WORK/manifest.txt"
for artifact in vmlinux rootfs-base.img manifest.txt; do
  mv "$WORK/$artifact" "$OUTPUT_DIR/$artifact"
done
echo "built Debian 13 artifacts in $OUTPUT_DIR"
