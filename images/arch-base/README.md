# Arch Base Image

This directory builds the Arch guest image expected by `srv`.

It produces two artifacts:

- `vmlinux`: an x86_64 guest kernel built from upstream Linux 7.3.1 using Firecracker's recommended 6.1 guest config as a seed plus a small fragment for Tailscale, Arch guest usability, Firecracker's current x86 ACPI boot requirements, and virtio balloon/page-reporting reclaim.
- `rootfs-base.img`: a sparse ext4 image populated with an Arch userspace via `pacstrap`, including Docker tooling with socket activation, Chromium and `agent-browser`, a small developer toolset (`go`, `gopls`, `lua-language-server`, `neovim`, `opencode`, `pi`, `odin`, `odinfmt`, `ols`, `shfmt`, `stylua`), a root LazyVim starter config preloaded with the BMW heritage amber theme assets mirrored from the `config` repo, default OpenCode and Pi configs that bootstrap rewrites to the per-VM host provider gateways when available, and a matching `/lib/modules/<kernel>` tree for the separately built guest kernel.

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

By default this builds Linux `7.3.1` while reusing Firecracker's `microvm-kernel-ci-x86_64-6.1.config` as the baseline config, pinned to revision `6ea6634994fa13473a0a8b61094989b6131f40d3`.

This kernel was tested locally with Firecracker 1.17.0 and both Arch and Debian guests; Linux 7 is not listed in Firecracker's supported guest-kernel matrix. A known limitation remains: Docker can report `OOMKilled=false` after a real cgroup OOM kill (exit 137). The same event loss was reproduced under QEMU/KVM and with Linux 6.12.79, but occurred more often with 7.3.1 in these tests. Do not rely on that flag alone to diagnose OOM kills.

The default guest rootfs size is `10G`. Override it with `ROOTFS_SIZE` if you want a different image size.

The default guest image is now intentionally less minimal: it includes `docker`, `docker-compose`, Chromium, `agent-browser`, `go`, `gopls`, `lua-language-server`, `neovim`, `opencode`, `pi`, `odin`, `odinfmt`, `ols`, `perf`, `shfmt`, `stylua`, `valgrind`, `git`, `fd`, `ripgrep`, `tree-sitter-cli`, `gcc`, a root LazyVim starter config with the BMW heritage amber theme preselected, boot-time module loading for `overlay` and `br_netfilter`, Docker-friendly sysctls, nftables IPv4/IPv6 family support for Arch's `iptables-nft` userspace, and a matching kernel module tree with real Docker-related `.ko` files installed from the custom Firecracker kernel build. Docker is installed but not started at boot; `docker.socket` starts `dockerd` and `containerd` on first Docker CLI/API use.

To keep idle memory low, the image masks getty/serial-getty and the udev device manager units. Firecracker's kernel-provided network configuration and devtmpfs device nodes are sufficient for the guest bootstrap path, including Tailscale SSH and Docker socket activation.

The LazyVim config lives under `/root/.config/nvim`. The image overlay now includes the shared BMW LazyVim colorscheme files, a managed `lua/plugins/bootstrap-theme.lua`, and overrides that point `lua_ls`, `gopls`, and `ols` at system binaries already installed in the image while clearing Mason's default `ensure_installed` list. That keeps the build-time `nvim --headless "+Lazy! sync"` prewarm deterministic instead of racing Mason's background installers, so fresh guests inherit the downloaded plugin set and do not need to bootstrap LazyVim on first launch.

OpenCode is installed from Arch's `extra` repository, while Pi is fetched from the upstream `badlogic/pi-mono` release tarball during the image build and installed under `/opt/pi` with `/usr/local/bin/pi` symlinked to the bundled binary. Guest bootstrap manages `/root/.config/opencode/opencode.json` and `/root/.pi/agent/{auth,models,settings}.json` at boot. When host-side provider gateways are enabled, bootstrap points both CLIs at the per-VM gateway URLs and uses a local placeholder `apiKey` only so the clients keep provider model catalogs available; the real upstream credentials still live only on the host and are injected by `srv`'s proxies.

If you want to pin a different Pi release, override `PI_VERSION`:

```bash
sudo PI_VERSION=0.70.2 ./images/arch-base/build.sh
```

`agent-browser` is installed as a pinned native binary with the matching upstream skill data and is configured to use the image's Arch-managed Chromium at `/usr/bin/chromium`. Override its release when rebuilding with `AGENT_BROWSER_VERSION`:

```bash
sudo AGENT_BROWSER_VERSION=0.32.0 ./images/arch-base/build.sh
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

## Wazuh agent

Native (non-Orb) images require Tailscale 1.104.4 or newer and ship the Wazuh agent 4.14.9, built from source by `packages/wazuh-agent/PKGBUILD`. The recipe is pinned to tag `v4.14.9`, commit `bed556db3f4f535cb3a585f3d9e7f35a6acf0f58`, and makepkg verifies the tag's upstream signature. Build the package in a clean Arch environment; rootless podman works:

```bash
podman run --rm --userns=keep-id:uid=0,gid=0 \
	-v "$PWD/images/arch-base/packages/wazuh-agent:/recipe:ro" -v "$PWD/out:/out" \
	docker.io/library/archlinux:base-devel sh -c '
	pacman-key --init && pacman-key --populate archlinux
	pacman -Syu --noconfirm --needed git cmake clang llvm python libtool autoconf automake pkgconf patchelf libelf
	useradd -m builder && cp -a /recipe /build && chown -R builder /build /out
	su builder -c "cd /build && gpg --batch --import keys/pgp/*.asc && PKGDEST=/out makepkg -f --noconfirm"'
```

The image build accepts only the package whose SHA-256 equals `WAZUH_PACKAGE_PINNED_SHA256` in `build.sh`.

Images with the agent also install `libelf` (its one dependency outside `base`) and `net-tools`, whose `netstat` the agent's rootcheck uses for its port check; without it rootcheck logs `netstat not available. Skipping port check.` Syscollector's port inventory does not need it. Existing guests can add it with `pacman -S net-tools`.

The package is stored, not rebuilt per image. Two builds of the same recipe differ (package build date, the OpenSSL build stamp inside `libwazuhext.so`), so a fresh build can never match the pin. The pin therefore names one reviewed artifact, and the image build reads that file from a stable path on the build host:

```text
/srv/artifacts/wazuh-agent/4.14.9-1/wazuh-agent-4.14.9-1-x86_64.pkg.tar.zst
```

Produce it once with the podman command above, check `sha256sum` against the pin, and copy it there (root-owned, not writable by the build user). Keep the build log with it. `WAZUH_PACKAGE=<file>` reads the package from elsewhere; the pin still applies. A rebuilt package means storing the new file under its own release directory and updating `WAZUH_PACKAGE_PINNED_SHA256` in a reviewed change.

```bash
sudo ./images/arch-base/build.sh                       # reads the stored artifact
sudo WAZUH_PACKAGE=/path/wazuh-agent-4.14.9-1-x86_64.pkg.tar.zst ./images/arch-base/build.sh
```

The build also refuses a package of another version or built from another commit, and records `wazuh_agent_version`, `wazuh_package_sha256` and `wazuh_source_commit` in `manifest.txt`. The image has no manager address or agent key, automatic enrollment is off, and `wazuh-agent.service` is not enabled. `srv-wazuh-enroll` on the controller registers the agent through the manager API and writes its identity to `/run/srv-wazuh/enrollment.json` (format in `/usr/local/lib/srv/wazuh-enroll.sh`) over SSH, then starts `srv-wazuh-enroll.service`, which installs `client.keys` and starts the agent. The agent refuses to start without an identity installed that way, and templates are stripped of it. Orb images omit the agent.

For development or emergency builds without the pinned package, `SRV_WAZUH_AGENT=0` builds a native image without the agent, its overlay files or its `libelf` and `net-tools` packages. Everything else, including the Tailscale floor, is unchanged, and the manifest records `wazuh_agent=disabled` so the gap stays visible. The default is `SRV_WAZUH_AGENT=1`; any value other than `0` or `1` is refused.

```bash
sudo SRV_WAZUH_AGENT=0 ./images/arch-base/build.sh
```

### Controller enrollment

When `SRV_WAZUH_API_URL` is set, the srv service enrolls every tailnet guest automatically once it is ready (tailnet presence), and again for every ready guest after the service restarts. The asset is `SRV_WAZUH_ASSET_PREFIX` plus the guest name, and the agent is named for the guest's hostname (the instance name), which the guest requires; the incarnation is the instance ID, sequenced by its creation time. The service pins the guest's ed25519 SSH host key as its tailnet node advertises it, connects over the tailnet with Tailscale SSH as root, and runs the same steps as the command below. Enrollment never delays readiness: while the manager is unreachable or refuses, the guest stays ready, its events show `monitoring pending`, and the service retries with backoff (30 s doubling to 15 min) until the agent reports. Deleting an instance leaves its agent registered; recreating the same name retires it, and `srv-wazuh-enroll retire` removes a decommissioned asset. An incorrect configuration stops the service at startup. Only guests booted from an image whose `manifest.txt` (beside its kernel) records `wazuh_agent_version` are enrolled: guests from older images, images built with `SRV_WAZUH_AGENT=0`, Orbs, and host-only or external-only guests are never contacted or registered. A guest whose kernel was later upgraded out of its image directory stops being enrolled.

An existing guest that predates the agent image can get the same agent in place: install the pinned package with `LocalFileSigLevel = Optional` and no repositories, with `/etc/tmpfiles.d/wazuh-agent.conf` masked to `/dev/null` first, copy the Wazuh overlay files (`var/ossec/etc/ossec.conf`, `usr/local/lib/srv/wazuh-{enroll,owners}.sh`, the `srv-wazuh-*` units, the `wazuh-agent.service.d` drop-in and `etc/tmpfiles.d/srv-wazuh.conf`), run `wazuh-owners.sh`, and leave `wazuh-agent.service` disabled. Then an admin marks the instance with `ssh srv wazuh in-place <name> on`. The mark only replaces the manifest check for that incarnation (a clean rebuild starts unmarked), Orbs and non-tailnet guests cannot be marked, and before registering anything the service confirms over SSH that the agent and the srv enrollment units are installed; a guest without them gets the event `monitoring not enrolled` and no registration. `off` clears the mark.

`cmd/srv-wazuh-enroll` (package `internal/wazuh`) enrolls a booted guest by hand:

```bash
srv-wazuh-enroll enroll -asset <NRC asset> -incarnation <instance ID> -seq <N> -name <hostname> \
	-guest <guest>:22 -host-key SHA256:<guest host key> [-ssh-key <file>]
srv-wazuh-enroll retire -asset <NRC asset> -incarnation <instance ID> -seq <N>
```

It registers the agent with `POST /agents` (the manager's local authd), reads its key with `GET /agents/{id}/key`, writes `/run/srv-wazuh/enrollment.json` over SSH as root (pinned host key), starts `srv-wazuh-enroll.service`, and waits until the manager reports the agent `active`. Each asset maps to one incarnation and one agent: re-enrolling the same incarnation reuses its agent, a higher `-seq` (clean rebuild) deletes only the agent mapped to that asset before registering again, an older one is refused, and a disconnected agent is never deleted. It reads its configuration from the environment:

| Variable | Meaning |
| --- | --- |
| `SRV_WAZUH_API_URL` | Manager API, `https://<manager>:55000` |
| `SRV_WAZUH_API_USER` | API user allowed to create, read and delete agents |
| `SRV_WAZUH_API_PASSWORD_FILE` | File (mode 0600 or stricter) holding its password; or `SRV_WAZUH_API_PASSWORD` |
| `SRV_WAZUH_API_CA_FILE` | PEM CA for the API certificate (system roots if unset) |
| `SRV_WAZUH_MANAGER_ADDRESS`, `SRV_WAZUH_MANAGER_PORT` | Where agents connect (port defaults to 1514) |
| `SRV_WAZUH_STATE` | Registration state file, shared by controllers on one host |
| `SRV_WAZUH_ASSET_PREFIX` | Optional prefix for asset IDs in the srv service's registration store, such as `gpu-rig-` |

The srv service also needs the tailnet policy to allow its tags to SSH as `root` to guest tags with action `accept` (not `check`).

## Optional native Amp orb preparation

The default build is unchanged. Opt in at build time:

```bash
sudo env SRV_AMP_ORB=1 ./images/arch-base/build.sh
```

For containerized builds, pass `-e SRV_AMP_ORB=1` to `podman run`.
The shared [`prepare-guest.sh`](../../contrib/amp/orbs/prepare-guest.sh) runs
as root **inside the guest chroot**, before DNS rewriting and package cleanup.
It installs the [custom-image prerequisites](https://ampcode.com/docs/orbs/self-hosted):
Bash/coreutils/util-linux, CA certificates, glibc, git, Node.js (checked to be
18 or later), tmux, curl, passwordless sudo, and recommended ripgrep, gh and
OpenSSH tools. It creates user/group `user` with writable `/home/user` and
installs Amp as that user under `/home/user/.amp`, with `/usr/local/bin/amp`
symlinked to the writable CLI so runtime self-updates can replace it.
The manifest gains `srv_amp_orb=1`; the Arch `packages` field still lists the
base package set, not this additional preparation's packages.

This fetches the current Amp installer and CLI from ampcode.com/static.ampcode.com;
it is not version-pinned. No login, token, repository checkout or project setup
is baked in. Orb builds omit Tailscale and disable native srv enrollment. A
host-only vsock agent configures DNS and launches `/usr/local/bin/amp orb bootstrap`
through systemd as user/group `user`, with `HOME=/home/user` and runtime-only
credentials. Preparation does not start bootstrap. Ordinary builds retain native
MMDS/Tailscale behavior. Projects may install optional Tailscale in `.agents/setup`
and authenticate it in `.agents/resume`; see the [Orb guide](../../docs/examples/amp-orbs.md).

Desktop is included alongside Portal and Terminal dependencies. Preparation
downloads only `/opt/amp-wayland` from a SHA-256-pinned layer of the official
Linux amd64 Amp image, verifies it before extraction, and installs Xwayland,
D-Bus, PulseAudio, FFmpeg, fonts, foot, swaybg and the required graphics libraries.
Arch also needs `libxml2-legacy` for the upstream compositor's `libxml2.so.2` ABI.
Allow build-time access to `us-central1-docker.pkg.dev` and its blob redirects.
No Docker daemon or container-image tooling is required for this download.

`/etc/amp-orb.env` supplies the bundled program paths to the dispatcher and
login shells. Private RPATHs keep the bundle's older libraries confined to its
binaries, rather than breaking newer Arch apps with a global `LD_LIBRARY_PATH`.
`/opt/amp-wayland/srv-layer.sha256` records the source layer.
Preparation checks library resolution and executable versions before completing.
For a different catalog name such as `amp-orb-desktop`, set both `ORB_IMAGE` and
`SRV_ORB_IMAGE` to that name in the provider configuration. Normal srv clients
can select it with `orb new NAME --image amp-orb-desktop` (not plain `new`).

Preparation also installs the SHA-256-pinned official Amp executor/workload
runtime and enables its environment watcher. srv owns bootstrap in
`amp-control.slice`; the independent executor launches workloads into
`amp-workload.slice`. The runtime pin is recorded in
`/usr/local/share/srv/amp-runtime-layer.sha256`. See the
[asset allowlist and validation limits](../../docs/examples/amp-orbs.md#executor-and-workload-runtime).

Real srv/KVM registration, CLI updates, Desktop/Tunnel operation, and wake-up
still require host validation. Ordinary Amp pauses save RAM. Project snapshot
pauses stop a running source and retain only its disk in the template.
RAM resume can preserve independent workloads. Validate authenticated Amp
fresh-token behavior and Desktop continuity on your host.

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

- The kernel build starts from Firecracker's `microvm-kernel-ci-x86_64-6.1.config` and runs `olddefconfig` against the selected source tree, so newer kernels can reuse Firecracker's microVM baseline.
- On current Firecracker x86 builds, srv launches Firecracker with PCI transport enabled. Because the image does not use an initramfs, the kernel fragment keeps the ACPI/PCI core and the virtio PCI block/network path built in (`CONFIG_ACPI=y`, `CONFIG_PCI=y`, `CONFIG_VIRTIO_PCI=y`, `CONFIG_VIRTIO_BLK=y`, `CONFIG_VIRTIO_NET=y`) so the guest can mount `/dev/vda` directly. The fragment also disables `CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES` so the kernel avoids the deprecated virtio-mmio command-line discovery path.
- The builder now runs `make ... modules` and installs the resulting module tree into the guest rootfs with `modules_install` plus `depmod`, so guests have a matching `/lib/modules/<kernel>` tree for the separately booted custom kernel.
- The Docker-related bridge and overlay pieces are built as loadable modules, while the kernel fragment also forces the nftables `ip`, `ip6`, and `inet` families on. That avoids the previous broken state where the guest only had depmod metadata and `nft add table ip ...` failed with `Operation not supported`.
- The builder now validates the merged kernel `.config` and refuses to finish if required Docker or balloon/page-reporting symbols are dropped by Kconfig or if `modules_install` would ship only metadata without any real `.ko` files.
- The kernel fragment also enables Landlock and adds it to `CONFIG_LSM`, which keeps pacman's default download sandbox working inside the guest on current Arch releases. Without that, package installs can fail with `landlock is not supported by the kernel` unless you disable pacman's sandbox manually.
- If the kernel build still fails with a generic top-level `Makefile:... Error 2`, retry with `KERNEL_JOBS=1` to surface the first real error line.
- The rootfs intentionally still omits the Arch `linux` package. The custom kernel is supplied separately as `vmlinux`, which matches how Firecracker boots guests, and the builder disables `90-mkinitcpio-install.hook` during `pacstrap` because no guest initramfs is needed.
- The rootfs package set now includes `docker`, `docker-compose`, Chromium, `go`, `gopls`, `lua-language-server`, `neovim`, `opencode`, `odin`, `odinfmt`, `ols`, `perf`, `shfmt`, `stylua`, `valgrind`, `git`, `fd`, `ripgrep`, `tree-sitter-cli`, `gcc`, `iptables-nft`, and `kmod`, while the image build also installs the pinned native `agent-browser` release with matching skill data and the upstream Pi release tarball under `/opt`. This makes fresh guests ready for browser automation, Docker-based workloads, Go or Odin development, low-level profiling/debugging, and both OpenCode and Pi agent workflows without additional package installs. Docker starts on demand through `docker.socket` instead of consuming memory at idle.
- Guest bootstrap now also manages `/root/.config/opencode/opencode.json` and Pi config under `/root/.pi/agent/` from MMDS/bootstrap metadata so the root account automatically targets per-VM host provider gateways when provider API keys are configured, without ever storing the real API keys inside the guest.
- The overlay now adds the repo-managed BMW LazyVim theme files and a default `bootstrap-theme.lua`, so new guests start on the same heritage amber colorscheme used by the `config` repo's LazyVim tooling.
- The builder also prewarms LazyVim inside the guest with `nvim --headless "+Lazy! sync"`, and the shipped config clears Mason's default auto-installs so that bootstrap work finishes before Neovim exits instead of leaving background package installs running.
- The builder uses its own minimal `pacman.conf` with only the standard Arch repositories so host-local repos and pacman hooks do not leak into the guest image build.
- `systemd-resolved` receives the nameservers from `/proc/net/pnp` during guest bootstrap. `/etc/resolv.conf` uses its stub resolver so Tailscale can add MagicDNS and split-DNS routes after `tailscale up` runs.
- Journald is configured to forward logs to `ttyS0`, which makes the guest bootstrap flow visible in each instance's `serial.log`.
