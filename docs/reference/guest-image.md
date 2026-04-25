# Guest image reference

The default guest image is an Arch Linux rootfs designed for srv. It is built by `images/arch-base/build.sh` and contains a full Linux userspace with developer tooling preinstalled.

## Artifacts

The builder produces:

| File | Description |
|------|-------------|
| `vmlinux` | x86_64 Firecracker-compatible kernel, built from 6.12 LTS with Firecracker's microvm config as baseline |
| `rootfs-base.img` | Sparse ext4 image populated via `pacstrap` |
| `manifest.txt` | Build manifest with version info |

## Included packages

The image is intentionally not minimal — it includes tooling for development and AI agent workflows:

- Docker, docker-compose with `docker.socket` activation instead of an idle `dockerd`
- Go, gopls
- Odin, odinfmt, OLS
- Neovim with prewarmed LazyVim (BMW heritage amber theme)
- OpenCode and Pi CLIs with per-VM Zen gateway bootstrap
- Git, fd, ripgrep, tree-sitter-cli, gcc, perf, valgrind
- iptables-nft with IPv4/IPv6 nftables support
- Kernel module tree matching the custom kernel (overlay, br_netfilter, Docker-related modules)

## Bootstrap service

The guest includes `srv-bootstrap.service`, which runs on every boot:

1. Discovers the primary virtio interface from the kernel-provided default route
2. Adds a route to Firecracker MMDS at `169.254.169.254/32`
3. Reads the MMDS payload from `http://169.254.169.254/` with `Accept: application/json`
4. Sets the hostname from `srv.hostname`
5. Starts `tailscaled`
6. Runs `tailscale up --auth-key=... --hostname=... --ssh` on the first authenticated boot (relies on persisted state on later boots)
7. Writes `/root/.config/opencode/opencode.json` plus Pi config under `/root/.pi/agent/` to the per-VM host Zen gateway when `SRV_ZEN_API_KEY` is configured, or removes those managed defaults when the gateway is disabled
8. Writes `/var/lib/srv/bootstrap.done` with the latest successful bootstrap timestamp

The `--ssh` flag on `tailscale up` is intentional — it enables Tailscale SSH so the control plane's `connect: ssh root@<name>` output works through the tailnet without per-user OpenSSH keys.

Docker is installed but not started during boot. The image enables `docker.socket`, so the first Docker CLI/API use starts `dockerd` and `containerd` on demand. Getty/serial-getty and udev units are masked to avoid idle daemons that are not needed for the Firecracker bootstrap path.

## Kernel details

- Starts from Firecracker's `microvm-kernel-ci-x86_64-6.1.config` and runs `olddefconfig` against the selected source tree
- Enables `CONFIG_PCI=y` for ACPI initialization (required by current Firecracker x86 builds)
- Disables `CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES` so the kernel prefers ACPI discovery
- Enables Landlock and adds it to `CONFIG_LSM` (keeps pacman's download sandbox working)
- Enables virtio ballooning and page reporting so fixed and pooled VMs can return idle guest pages to the host
- Builds real `.ko` module files for Docker, overlay, br_netfilter, and nftables

## DNS

`/etc/resolv.conf` is symlinked to `/proc/net/pnp` so the kernel `ip=` boot parameter provides working DNS before `tailscale up` runs.

## Logging

journald is configured to forward logs to `ttyS0`, making the guest bootstrap flow visible in each instance's serial log (`ssh srv logs <name> serial`).
