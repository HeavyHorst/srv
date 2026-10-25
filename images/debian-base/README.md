# Debian 13 guest image

Builds a native **amd64 Debian 13 (trixie)** ext4 development guest with
`debootstrap --variant=minbase`. Outputs are `vmlinux`, `rootfs-base.img`, and
`manifest.txt` (kernel release, artifact SHA-256 hashes and installed package
versions). No partition table, bootloader or initramfs is needed.

This is intentionally smaller in scope than Arch: systemd, systemd-resolved,
Tailscale, Docker (Debian's docker.io/docker-cli), Compose v2, git, curl, jq, tmux,
openssh-client, iproute2, iptables, kmod and basic shell utilities. It does **not**
include Arch's language toolchains, editors, Chromium, agent-browser, Pi or
OpenCode. Install project-specific tools with apt or `.agents/setup`; the Amp
runner installs Amp separately unless you opt into native Amp orb preparation
below. SSH access uses Tailscale SSH, not an sshd.

Compose is the checksum-pinned upstream linux/amd64 **v2.24.7 CLI plugin** at
`/usr/local/lib/docker/cli-plugins/docker-compose`; use `docker compose`, not the
legacy Python `docker-compose` command. This version was qualified with Debian
12's Docker 20.10 in the deployed bookworm adaptation; pinning it keeps that
adaptation from accidentally installing Debian 12's v1 package. The default
builder still targets Debian 13. Update the version and checksum together after
compatibility testing. The build checks plugin discovery without starting a
daemon and records its version/checksum in `manifest.txt`.

## Host requirements

Use a trusted, native x86_64 Linux host with root, chroot, device-node creation
and mount privileges (CAP_SYS_ADMIN). An unprivileged container is insufficient;
a container must be privileged and have working mount propagation. Build only
trusted inputs: package maintainer scripts execute as root in a chroot, which
is not a security boundary. Allow several GB of staging space in addition to
the sparse image, and network access to Debian, pkgs.tailscale.com, GitHub and
its release-asset redirects.

On Debian 12/13 or Ubuntu:

```bash
sudo apt-get update
sudo apt-get install debootstrap debian-archive-keyring e2fsprogs kmod \
  curl ca-certificates systemd util-linux coreutils findutils grep
```

An older debootstrap must support `trixie`. No loop device is required for the
Debian build itself: `mkfs.ext4 -d` populates the image from a staging tree.
The build temporarily mounts proc and a slave bind of /dev for package setup.
Exit/signal traps unmount and delete staging; if unmount fails, staging is left
in place rather than deleting files through a live mount. Existing output
artifacts are replaced only after the image has been constructed successfully.

## Reuse the shared kernel AND modules

First run the existing [Arch builder](../arch-base/README.md), or use its already
completed output and retained kernel work directory. Do not use Debian's stock
kernel packages: srv needs the custom built-in virtio/ext4 boot path and Docker
modules. A matching version string alone does not prove identical kernel config
or symbol CRCs: always take the kernel and module tree from the **same build**.
The builder checks the vmlinux release banner and a module's vermagic, requires
real .ko files, and regenerates depmod metadata in Debian.

From the repository root, export modules from retained Arch build work (adjust
6.12.79 if that build used another KERNEL_VERSION):

```bash
kernel_work="$PWD/images/arch-base/out/work/linux-6.12.79"
module_stage="$PWD/images/debian-base/out/shared-kernel"
kernel_release=$(cat "$kernel_work/include/config/kernel.release")
sudo mkdir -p "$module_stage"
sudo make -C "$kernel_work" INSTALL_MOD_PATH="$module_stage" modules_install
sudo env \
  KERNEL_IMAGE="$kernel_work/vmlinux" \
  KERNEL_MODULES_DIR="$module_stage/lib/modules/$kernel_release" \
  ./images/debian-base/build.sh
```

`modules_install` does not rebuild the kernel; it requires the original built
modules and make/toolchain utilities used by Arch. If the Arch build ran in a
container, run that command inside the same privileged Arch build environment.
`kernel_work/vmlinux` is the exact kernel copied to the Arch output at build time.

Alternatively extract the modules already installed in the completed Arch image
(this path needs mount/loop privileges but no compiler):

```bash
sudo bash <<'EOF'
set -euo pipefail
stage="$PWD/images/debian-base/out/shared-kernel"
mnt=$(mktemp -d)
trap 'umount "$mnt" && rmdir "$mnt"' EXIT
mount -o loop,ro images/arch-base/out/rootfs-base.img "$mnt"
mkdir -p "$stage/lib/modules"
cp -a "$mnt/lib/modules/." "$stage/lib/modules/"
EOF
# Substitute the release directory copied above, e.g. 6.12.79:
sudo env KERNEL_IMAGE="$PWD/images/arch-base/out/vmlinux" \
  KERNEL_MODULES_DIR="$PWD/images/debian-base/out/shared-kernel/lib/modules/6.12.79" \
  ./images/debian-base/build.sh
```

## Optional native Amp orb preparation

Set `SRV_AMP_ORB=1` alongside the required kernel inputs:

```bash
sudo env SRV_AMP_ORB=1 KERNEL_IMAGE="$kernel_work/vmlinux" \
  KERNEL_MODULES_DIR="$module_stage/lib/modules/$kernel_release" \
  ./images/debian-base/build.sh
```

Defaults are unchanged. The shared
[`prepare-guest.sh`](../../contrib/amp/orbs/prepare-guest.sh) runs as root inside
the chroot before DNS rewriting and cleanup. It installs the
[custom-image prerequisites](https://ampcode.com/docs/orbs/self-hosted):
Bash/coreutils/util-linux, CA certificates, glibc, git, Node.js (checked to be
18 or later), tmux, curl, sudo, ripgrep, gh and openssh-client. User/group `user`
gets writable `/home/user` and passwordless sudo. Amp is installed as that user
under `/home/user/.amp`; `/usr/local/bin/amp` links to the writable CLI so runtime
self-updates can replace it. The manifest gains `srv_amp_orb=1`, and its package
inventory includes the extra Debian packages.

Allow build-time access to ampcode.com/static.ampcode.com for the current,
unpinned installer and CLI. No login, tokens, repository checkout or project
setup is baked in. Orb builds omit Tailscale and disable native srv enrollment.
A host-only vsock agent configures DNS and starts `/usr/local/bin/amp orb bootstrap`
through systemd as user/group `user`, with `HOME=/home/user` and runtime-only
credentials. Preparation does not start bootstrap. Ordinary builds retain native
MMDS/Tailscale behavior. Projects may install optional Tailscale in `.agents/setup`
and authenticate it in `.agents/resume`; see the [Orb guide](../../docs/examples/amp-orbs.md).

Desktop is included alongside Portal and Terminal dependencies. Preparation
fetches `/opt/amp-wayland` from a SHA-256-pinned layer of the official Linux amd64
Amp image and verifies it before extraction. It installs Xwayland, D-Bus,
PulseAudio, FFmpeg, fonts, foot, swaybg and the graphics libraries needed by the
bundle. Allow build-time access to `us-central1-docker.pkg.dev` and its blob
redirects; no Docker daemon or image tooling is needed. This increases the image's
installed size; raise `ROOTFS_SIZE` if adding further tools exceeds the default.

`/etc/amp-orb.env` supplies program paths to bootstrap and login shells. Private
RPATHs resolve bundled libraries without changing other applications' libraries.
The layer digest is saved in `/opt/amp-wayland/srv-layer.sha256`; preparation
checks runtime libraries and executable versions. Register the resulting image
under any srv catalog name (for example `amp-orb-desktop`) and set the provider's
`ORB_IMAGE` and `SRV_ORB_IMAGE` to that name, or use
`orb new NAME --image amp-orb-desktop` directly (not plain `new`).

Real srv/KVM boot, registration, CLI self-updates, Desktop/Tunnel operation and
wake-up still need host validation. Stop/start does not preserve desktop sessions.

## Install and configure

```bash
sudo install -d /var/lib/srv/images/debian-base
sudo cp --sparse=always images/debian-base/out/rootfs-base.img \
  /var/lib/srv/images/debian-base/rootfs-base.img
sudo install -m 0644 images/debian-base/out/vmlinux \
  images/debian-base/out/manifest.txt /var/lib/srv/images/debian-base/
```

Register these paths as `debian-13` in `SRV_IMAGES_FILE` (see
[image configuration](../../docs/reference/configuration.md#guest-artifacts)),
then create guests with `ssh srv new demo --image debian-13`. Keep the runtime
base rootfs on the reflink-capable filesystem used for srv instance storage.

Optional build variables: `OUTPUT_DIR` (default `images/debian-base/out`),
`ROOTFS_SIZE` (default `4G`, must fit installed contents), `DEBIAN_MIRROR`
(default `https://deb.debian.org/debian`), plus required `KERNEL_IMAGE` and
`KERNEL_MODULES_DIR`. Output includes a package/version inventory, but upstream
Debian and Tailscale repositories change: this is a repeatable build recipe,
**not a bit-for-bit reproducible or snapshot-pinned image**. Only Debian main
from the selected mirror and Tailscale stable are configured; security updates
must be managed by the operator (including enabling trixie-security if desired).

## Guest contract and limitations

The builder reads `../arch-base/overlay/etc/` and the bootstrap script directly;
there is no duplicated bootstrap implementation or Arch overlay modification.
It shares MMDS enrollment, transient hostname, resolved stub/MagicDNS, provider
gateway configuration, serial journaling, Docker module/sysctl setup and the
Firecracker poweroff override. Bootstrap may write provider configs even though
the associated optional agent binaries are not installed. Docker is socket
activated; containerd starts when Docker needs it. Getty and udev are masked,
as in Arch; the shared kernel and its devtmpfs supply guest devices. Fresh
machine IDs are generated on boot. No Tailscale identity is baked into the image.

Full runtime validation needs an srv/Firecracker-capable KVM host, MMDS and a
Tailscale enrollment. Check enrollment/serial logs, DNS, `docker run --rm
hello-world`, and runner registration there before production use. Rebuild the
rootfs after shared overlay changes, and distribute kernel/rootfs as a pair.
