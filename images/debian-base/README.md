# Debian 13 guest image

Builds a native **amd64 Debian 13 (trixie)** ext4 development guest with
`debootstrap --variant=minbase`. Outputs are `vmlinux`, `rootfs-base.img`, and
`manifest.txt` (kernel release, artifact SHA-256 hashes and installed package
versions). No partition table, bootloader or initramfs is needed.

This is intentionally smaller in scope than Arch: systemd, systemd-resolved,
Tailscale, Docker (Debian's docker.io/docker-cli), git, curl, jq, tmux,
openssh-client, iproute2, iptables, kmod and basic shell utilities. It does **not**
include Arch's language toolchains, editors, Chromium, agent-browser, Pi or
OpenCode. Install project-specific tools with apt or `.agents/setup`; the Amp
runner installs Amp separately. SSH access uses Tailscale SSH, not an sshd.

## Host requirements

Use a trusted, native x86_64 Linux host with root, chroot, device-node creation
and mount privileges (CAP_SYS_ADMIN). An unprivileged container is insufficient;
a container must be privileged and have working mount propagation. Build only
trusted inputs: package maintainer scripts execute as root in a chroot, which
is not a security boundary. Allow several GB of staging space in addition to
the sparse image, and network access to Debian and pkgs.tailscale.com.

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
