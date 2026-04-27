# Arch Base Image

This directory builds the Arch guest image expected by `srv`.

It produces two artifacts:

- `vmlinux`: an x86_64 Firecracker-compatible kernel built from the upstream 6.12 LTS kernel using Firecracker's recommended 6.1 guest config as a seed plus a small fragment for Tailscale, Arch guest usability, Firecracker's current x86 ACPI boot requirements, and virtio balloon/page-reporting reclaim.
- `rootfs-base.img`: a sparse ext4 image populated with an Arch userspace via `pacstrap`, including Docker tooling with socket activation, a small developer toolset (`go`, `gopls`, `lua-language-server`, `neovim`, `opencode`, `pi`, `odin`, `odinfmt`, `ols`, `shfmt`, `stylua`), a root LazyVim starter config preloaded with the BMW heritage amber theme assets mirrored from the `config` repo, default OpenCode and Pi configs that bootstrap rewrites to the per-VM host provider gateways when available, and a matching `/lib/modules/<kernel>` tree for the separately built guest kernel.

The guest rootfs includes a boot-time `srv-bootstrap.service` that:

1. discovers the primary virtio interface from the kernel-provided default route
2. adds a route to Firecracker MMDS at `169.254.169.254/32`
3. reads the MMDS payload from `http://169.254.169.254/` with `Accept: application/json`
4. sets the hostname from `srv.hostname`
5. starts `tailscaled`
6. runs `tailscale up --auth-key=... --hostname=... --ssh` on the first authenticated boot and relies on persisted `tailscaled` state on later boots
7. writes `/root/.config/opencode/opencode.json` plus Pi config under `/root/.pi/agent/` to the per-VM host provider gateways when the host enabled provider API keys, or removes those managed defaults when all provider gateways are disabled
8. writes `/var/lib/srv/bootstrap.done` with the latest successful bootstrap timestamp for debugging

That `--ssh` flag is intentional: it makes the control plane's existing `connect: ssh root@<name>` output usable through Tailscale SSH without injecting per-user OpenSSH keys into the guest image.

## Requirements

- Arch Linux host, or another Linux system with `podman` available for the containerized workflow below
- root privileges
- network access to Arch package mirrors, `kernel.org`, and GitHub for both raw content and LazyVim plugin downloads during the image build
- for direct host builds: dependencies such as `base-devel`, `arch-install-scripts`, `bc`, `e2fsprogs`, `kmod`, `rsync`, and `curl`

## Build

```bash
sudo ./images/arch-base/build.sh
```

By default this currently builds Linux `6.12.79` while reusing Firecracker's `microvm-kernel-ci-x86_64-6.1.config` as the baseline config.

The default guest rootfs size is `10G`. Override it with `ROOTFS_SIZE` if you want a different image size.

The default guest image is now intentionally less minimal: it includes `docker`, `docker-compose`, `go`, `gopls`, `lua-language-server`, `neovim`, `opencode`, `pi`, `odin`, `odinfmt`, `ols`, `perf`, `shfmt`, `stylua`, `valgrind`, `git`, `fd`, `ripgrep`, `tree-sitter-cli`, `gcc`, a root LazyVim starter config with the BMW heritage amber theme preselected, boot-time module loading for `overlay` and `br_netfilter`, Docker-friendly sysctls, nftables IPv4/IPv6 family support for Arch's `iptables-nft` userspace, and a matching kernel module tree with real Docker-related `.ko` files installed from the custom Firecracker kernel build. Docker is installed but not started at boot; `docker.socket` starts `dockerd` and `containerd` on first Docker CLI/API use.

To keep idle memory low, the image masks getty/serial-getty and the udev device manager units. Firecracker's kernel-provided network configuration and devtmpfs device nodes are sufficient for the guest bootstrap path, including Tailscale SSH and Docker socket activation.

The LazyVim config lives under `/root/.config/nvim`. The image overlay now includes the shared BMW LazyVim colorscheme files, a managed `lua/plugins/bootstrap-theme.lua`, and overrides that point `lua_ls`, `gopls`, and `ols` at system binaries already installed in the image while clearing Mason's default `ensure_installed` list. That keeps the build-time `nvim --headless "+Lazy! sync"` prewarm deterministic instead of racing Mason's background installers, so fresh guests inherit the downloaded plugin set and do not need to bootstrap LazyVim on first launch.

OpenCode is installed from Arch's `extra` repository, while Pi is fetched from the upstream `badlogic/pi-mono` release tarball during the image build and installed under `/opt/pi` with `/usr/local/bin/pi` symlinked to the bundled binary. Guest bootstrap manages `/root/.config/opencode/opencode.json` and `/root/.pi/agent/{auth,models,settings}.json` at boot. When host-side provider gateways are enabled, bootstrap points both CLIs at the per-VM gateway URLs and uses a local placeholder `apiKey` only so the clients keep provider model catalogs available; the real upstream credentials still live only on the host and are injected by `srv`'s proxies.

If you want to pin a different Pi release, override `PI_VERSION`:

```bash
sudo PI_VERSION=0.70.2 ./images/arch-base/build.sh
```

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

## Podman Build On Non-Arch Linux Hosts

If your host is not Arch and does not provide `pacstrap`, run the existing builder inside a privileged Arch Linux container instead of trying to recreate the Arch packaging environment on the host.

Install `podman` with your distro's package manager, then from the repo root run:

```bash
sudo podman run --rm --privileged --network host \
  -v "$PWD":/work \
  -w /work \
  docker.io/library/archlinux:latest \
  bash -lc '
    set -euo pipefail
    pacman -Sy --noconfirm archlinux-keyring
    pacman -Syu --noconfirm arch-install-scripts base-devel bc e2fsprogs rsync curl systemd
    ./images/arch-base/build.sh
  '
```

That keeps the build logic in one place: the host only needs `podman`, while the container supplies `pacstrap`, `pacman`, and the expected Arch userspace.

If you want the built artifacts to land directly in the installed `srv` runtime path on the host, pass `OUTPUT_DIR` through to the containerized build:

```bash
sudo install -d -m 0755 /var/lib/srv/images/arch-base
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

Notes for the containerized path:

- `--privileged` is required because the builder uses `losetup`, `mkfs.ext4`, and `mount`.
- `--network host` keeps mirror and kernel downloads simple and avoids container DNS surprises during `pacstrap`.
- The output directory should still be on the same reflink-capable filesystem as `SRV_DATA_DIR` when you intend to use `rootfs-base.img` with an installed host.
- If you build into the default repo-local output directory, move or copy `vmlinux` and `rootfs-base.img` to the paths configured by `SRV_BASE_KERNEL` and `SRV_BASE_ROOTFS` in `/etc/srv/srv.env` before creating guests.

Changes under `overlay/` only reach new guests after you rebuild `rootfs-base.img` and refresh the host's configured base rootfs artifact.

Rebuilt `vmlinux` artifacts can be picked up by existing stopped guests on their next `start` or `restart` once the host's `SRV_BASE_KERNEL` points at the refreshed kernel path.

## Outputs

After a successful build, the output directory contains:

- `vmlinux`
- `rootfs-base.img`
- `manifest.txt`

You can then point the service at those paths with `-base-kernel` and `-base-rootfs`.

## Notes

- The kernel build starts from Firecracker's `microvm-kernel-ci-x86_64-6.1.config` and runs `olddefconfig` against the selected source tree, so newer longterm kernels can reuse Firecracker's known-good microVM baseline.
- On current Firecracker x86 builds, the guest kernel needs `CONFIG_PCI=y` for ACPI initialization even when the microVM is still using MMIO virtio devices instead of PCI transport. The fragment also disables `CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES` so the kernel prefers ACPI discovery instead of probing the same virtio devices twice.
- The builder now runs `make ... modules` and installs the resulting module tree into the guest rootfs with `modules_install` plus `depmod`, so guests have a matching `/lib/modules/<kernel>` tree for the separately booted custom kernel.
- The Docker-related bridge and overlay pieces are built as loadable modules, while the kernel fragment also forces the nftables `ip`, `ip6`, and `inet` families on. That avoids the previous broken state where the guest only had depmod metadata and `nft add table ip ...` failed with `Operation not supported`.
- The builder now validates the merged kernel `.config` and refuses to finish if required Docker or balloon/page-reporting symbols are dropped by Kconfig or if `modules_install` would ship only metadata without any real `.ko` files.
- The kernel fragment also enables Landlock and adds it to `CONFIG_LSM`, which keeps pacman's default download sandbox working inside the guest on current Arch releases. Without that, package installs can fail with `landlock is not supported by the kernel` unless you disable pacman's sandbox manually.
- If the kernel build still fails with a generic top-level `Makefile:... Error 2`, retry with `KERNEL_JOBS=1` to surface the first real error line.
- The rootfs intentionally still omits the Arch `linux` package. The custom kernel is supplied separately as `vmlinux`, which matches how Firecracker boots guests, and the builder disables `90-mkinitcpio-install.hook` during `pacstrap` because no guest initramfs is needed.
- The rootfs package set now includes `docker`, `docker-compose`, `go`, `gopls`, `lua-language-server`, `neovim`, `opencode`, `odin`, `odinfmt`, `ols`, `perf`, `shfmt`, `stylua`, `valgrind`, `git`, `fd`, `ripgrep`, `tree-sitter-cli`, `gcc`, `iptables-nft`, and `kmod`, while the image build also installs the upstream Pi release tarball under `/opt/pi`, which makes fresh guests ready for Docker-based workloads, Go or Odin development, low-level profiling/debugging, and both OpenCode and Pi agent workflows without additional package installs. Docker starts on demand through `docker.socket` instead of consuming memory at idle.
- Guest bootstrap now also manages `/root/.config/opencode/opencode.json` and Pi config under `/root/.pi/agent/` from MMDS/bootstrap metadata so the root account automatically targets per-VM host provider gateways when provider API keys are configured, without ever storing the real API keys inside the guest.
- The overlay now adds the repo-managed BMW LazyVim theme files and a default `bootstrap-theme.lua`, so new guests start on the same heritage amber colorscheme used by the `config` repo's LazyVim tooling.
- The builder also prewarms LazyVim inside the guest with `nvim --headless "+Lazy! sync"`, and the shipped config clears Mason's default auto-installs so that bootstrap work finishes before Neovim exits instead of leaving background package installs running.
- The builder uses its own minimal `pacman.conf` with only the standard Arch repositories so host-local repos and pacman hooks do not leak into the guest image build.
- `/etc/resolv.conf` is symlinked to `/proc/net/pnp` so the kernel `ip=` boot parameter inserted by `firecracker-go-sdk` provides working DNS before `tailscale up` runs.
- Journald is configured to forward logs to `ttyS0`, which makes the guest bootstrap flow visible in each instance's `serial.log`.
