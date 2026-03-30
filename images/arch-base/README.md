# Arch Base Image

This directory builds the Arch guest image expected by `srv`.

It produces two artifacts:

- `vmlinux`: an x86_64 Firecracker-compatible kernel built from the upstream 6.12 LTS kernel using Firecracker's recommended 6.1 guest config as a seed plus a small fragment for Tailscale, Arch guest usability, and Firecracker's current x86 ACPI boot requirements.
- `rootfs-base.img`: a sparse ext4 image populated with an Arch userspace via `pacstrap`.

The guest rootfs includes a boot-time `srv-bootstrap.service` that:

1. discovers the primary virtio interface from the kernel-provided default route
2. adds a route to Firecracker MMDS at `169.254.169.254/32`
3. reads the MMDS payload from `http://169.254.169.254/` with `Accept: application/json`
4. sets the hostname from `srv.hostname`
5. starts `tailscaled`
6. runs `tailscale up --auth-key=... --hostname=... --ssh` on the first authenticated boot and relies on persisted `tailscaled` state on later boots
7. writes `/var/lib/srv/bootstrap.done` with the latest successful bootstrap timestamp for debugging

That `--ssh` flag is intentional: it makes the control plane's existing `connect: ssh root@<name>` output usable through Tailscale SSH without injecting per-user OpenSSH keys into the guest image.

## Requirements

- Arch Linux host or another system with `pacstrap` and `systemctl`
- root privileges
- network access to Arch package mirrors, `kernel.org`, and GitHub raw content
- build dependencies such as `base-devel`, `arch-install-scripts`, `bc`, `e2fsprogs`, `rsync`, and `curl`

## Build

```bash
sudo ./images/arch-base/build.sh
```

By default this currently builds Linux `6.12.79` while reusing Firecracker's `microvm-kernel-ci-x86_64-6.1.config` as the baseline config.

The kernel build parallelism is conservative by default. Override it if needed:

```bash
sudo KERNEL_JOBS=2 ./images/arch-base/build.sh
```

If you want to pin a different kernel release, override `KERNEL_VERSION`:

```bash
sudo KERNEL_VERSION=6.1.167 ./images/arch-base/build.sh
```

By default the script writes artifacts under `images/arch-base/out/`, which is ignored by git.

To write directly into the service's expected runtime image directory:

```bash
sudo OUTPUT_DIR=/var/lib/srv/images/arch-base ./images/arch-base/build.sh
```

Changes under `overlay/` only reach new guests after you rebuild `rootfs-base.img` and refresh the host's configured base rootfs artifact.

## Outputs

After a successful build, the output directory contains:

- `vmlinux`
- `rootfs-base.img`
- `manifest.txt`

You can then point the service at those paths with `-base-kernel` and `-base-rootfs`.

## Notes

- The kernel build starts from Firecracker's `microvm-kernel-ci-x86_64-6.1.config` and runs `olddefconfig` against the selected source tree, so newer longterm kernels can reuse Firecracker's known-good microVM baseline.
- On current Firecracker x86 builds, the guest kernel needs `CONFIG_PCI=y` for ACPI initialization even when the microVM is still using MMIO virtio devices instead of PCI transport. The fragment also disables `CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES` so the kernel prefers ACPI discovery instead of probing the same virtio devices twice.
- The kernel fragment also enables Landlock and adds it to `CONFIG_LSM`, which keeps pacman's default download sandbox working inside the guest on current Arch releases. Without that, package installs can fail with `landlock is not supported by the kernel` unless you disable pacman's sandbox manually.
- If the kernel build still fails with a generic top-level `Makefile:... Error 2`, retry with `KERNEL_JOBS=1` to surface the first real error line.
- The rootfs intentionally omits the Arch `linux` package. The custom kernel is supplied separately as `vmlinux`, which matches how Firecracker boots guests, and the builder disables `90-mkinitcpio-install.hook` during `pacstrap` because no guest initramfs is needed.
- The rootfs package set installs `iptables-nft` explicitly so `pacstrap` does not stop for the `libxtables.so=12-64` provider prompt on newer Arch hosts.
- The builder uses its own minimal `pacman.conf` with only the standard Arch repositories so host-local repos and pacman hooks do not leak into the guest image build.
- `/etc/resolv.conf` is symlinked to `/proc/net/pnp` so the kernel `ip=` boot parameter inserted by `firecracker-go-sdk` provides working DNS before `tailscale up` runs.
- Journald is configured to forward logs to `ttyS0`, which makes the guest bootstrap flow visible in each instance's `serial.log`.
