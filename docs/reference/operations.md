# Operations runbook

This runbook is for the supported prepared-host path: systemd-managed `srv`, `srv-net-helper`, and `srv-vm-runner` on a Linux host with cgroup v2, `/dev/kvm`, `SRV_DATA_DIR` on a reflink-capable filesystem such as `btrfs` or reflink-enabled `xfs`, `SRV_BASE_ROOTFS` on the same filesystem, and the official static Firecracker/jailer release pair.

Same-host reboot recovery is already built into the control plane: when `srv` comes back under systemd, previously active instances are restarted automatically. The steps below cover the larger operator workflows that were previously only implied.

## Supported Upgrade Lanes

- Guest-local maintenance is still guest-local. Running `pacman -Syu` inside a guest is supported when you want that guest to drift independently, but it is not the control plane's golden-image rollout path.
- Kernel roll-forward is a host-managed lane. Existing stopped guests pick up the current `SRV_BASE_KERNEL` and optional `SRV_BASE_INITRD` on their next `start` or `restart`.
- Rootfs golden-image rollout is a new-guest lane. Updating `SRV_BASE_ROOTFS` changes what future `new` clones from, but it does not rewrite existing guests' writable disks.
- Schema rollout is tied to the control-plane binary. SQLite migrations run during `srv` startup and are currently additive; rollback means restoring the pre-upgrade backup together with the previous binary set.

## Backup

Take backups from a quiesced host so `SRV_DATA_DIR` and the SQLite WAL state are self-consistent.

If you need a fast host-local point-in-time copy without shutting the control plane down first, use the built-in snapshot barrier instead:

```bash
ssh srv snapshot create
```

That command is admin-only. It briefly rejects every other SSH command, waits for already admitted commands to finish, checkpoints SQLite, flushes the filesystem, and then creates a readonly btrfs snapshot of `SRV_DATA_DIR` under `SRV_DATA_DIR/.snapshots/<timestamp>`.

Snapshot semantics are intentionally limited and explicit:

- control-plane consistent
- stopped guests fully safe
- running guests crash-consistent

Important caveats for this path:

- `SRV_DATA_DIR` itself must be a btrfs subvolume root. A plain directory on btrfs is not enough.
- The app snapshots `SRV_DATA_DIR` only. `/etc/srv`, environment files, and unit overrides still need the existing operator-managed backup flow below.
- Remote `btrfs send/receive` replication is intentionally out of the barrier path. If you use it for DR or warm standby, run it after the local snapshot already exists.

1. Stop the services.

```bash
sudo systemctl stop srv srv-net-helper srv-vm-runner
```

2. Capture the state directory, environment file, and any unit overrides.

```bash
sudo tar --xattrs --acls --numeric-owner \
  --ignore-failed-read \
  -C / \
  -czf /var/tmp/srv-backup-$(date -u +%Y%m%dT%H%M%SZ).tar.gz \
  etc/srv \
  var/lib/srv \
  etc/systemd/system/srv.service.d \
  etc/systemd/system/srv-net-helper.service.d \
  etc/systemd/system/srv-vm-runner.service.d \
  etc/systemd/system.control
```

3. Start the services again if this was only a backup window.

```bash
sudo systemctl start srv-vm-runner srv-net-helper srv
```

Notes:

- Preserve the configured paths in `/etc/srv/srv.env`. Instance rows store absolute runtime paths such as `SRV_DATA_DIR/instances/<name>/rootfs.img`, so changing `SRV_DATA_DIR`, `SRV_JAILER_BASE_DIR`, or the base artifact paths during restore is not a supported relocation workflow.
- Keep `SRV_JAILER_BASE_DIR` on the same filesystem as `SRV_DATA_DIR`; the runner hard-links log files into the jail and cross-filesystem links fail.

## Move One Stopped VM Between Hosts

For single-VM cutover, use the portable stopped-VM stream instead of copying SQLite rows or the whole host state directory:

```bash
ssh srv-a export demo | ssh srv-b import
```

Operational notes:

- The source VM must be stopped before export.
- Import recreates the VM under the same name and leaves it stopped. Start it explicitly on the destination after the stream completes.
- The artifact preserves portable metadata such as name, creator, machine shape, rootfs size, and last-known Tailscale name or IP.
- Serial and Firecracker logs are included when present, but each is capped to the newest `256 MiB` during export.
- Import regenerates destination-local runtime state such as absolute file paths, tap device wiring, guest MAC, and VM subnet allocation.
- The copied rootfs carries the guest's durable Tailscale identity, so do not boot the source and destination copies at the same time.
- The destination host uses its currently configured `SRV_BASE_KERNEL` and optional `SRV_BASE_INITRD` on the first later `start`; only the writable guest disk and optional logs come from the streamed artifact.

## Restore Or Rebuild A Host

1. Prepare a fresh host with the normal prerequisites: Tailscale, cgroup v2, `/dev/kvm`, reflink-capable storage such as `btrfs` or reflink-enabled `xfs` shared by `SRV_DATA_DIR` and `SRV_BASE_ROOTFS`, and the repo checkout.
2. Reinstall the managed assets.

```bash
sudo ./contrib/systemd/install.sh
```

3. Re-enable IPv4 forwarding for guest NAT on the rebuilt host.

```bash
sudo tee /etc/sysctl.d/90-srv-ip-forward.conf >/dev/null <<'EOF'
net.ipv4.ip_forward = 1
EOF
sudo sysctl --system
```

4. Restore `/etc/srv/srv.env` from backup and verify that `SRV_FIRECRACKER_BIN`, `SRV_JAILER_BIN`, `SRV_DATA_DIR`, `SRV_BASE_KERNEL`, `SRV_BASE_ROOTFS`, and any optional `SRV_BASE_INITRD` still point at the intended paths.
5. Restore the saved `SRV_DATA_DIR` tree to the same path.
6. Reload systemd and start the services.

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now srv-vm-runner srv-net-helper srv
```

7. Run the prepared-host validation gate before handing the host back to users.

```bash
sudo ./contrib/smoke/host-smoke.sh
```

That smoke pass is part of the supported restore workflow now, not an optional extra.

## Upgrade And Rollback

### Control Plane And Schema

1. Take the quiesced backup above before starting the upgraded `srv` binary for the first time.
2. Build and install the new `srv`, `srv-net-helper`, and `srv-vm-runner` binaries.
3. If you also refreshed Firecracker, use the matching official static `firecracker` and `jailer` pair and verify `/etc/srv/srv.env` still points at those paths.
4. Restart the services.

```bash
sudo systemctl stop srv srv-net-helper srv-vm-runner
sleep 5
sudo systemctl start srv-vm-runner srv-net-helper srv
```

5. Run the host smoke test.

```bash
sudo ./contrib/smoke/host-smoke.sh
```

Rollback for control-plane or schema regressions is restore-based:

1. Stop the services.
2. Reinstall the previous binaries and previous static Firecracker/jailer pair if those changed.
3. Restore the pre-upgrade backup of `/etc/srv` and `SRV_DATA_DIR`.
4. Restart the services.
5. Run the same host smoke test again.

### Kernel Rollout For Existing Guests

1. Rebuild the kernel artifact under [images/arch-base/](../../images/arch-base/README.md).
2. Update `SRV_BASE_KERNEL` and optional `SRV_BASE_INITRD` in `/etc/srv/srv.env`.
3. Restart the units if needed so the runner sees the new base paths.
4. Stop and start guests one at a time, or let already stopped guests pick up the new boot artifacts on their next `start`.
5. Use a canary guest first, then roll wider once it passes the workload check you care about.

Rollback is just pointing `SRV_BASE_KERNEL` or `SRV_BASE_INITRD` back to the previous artifact and restarting the affected guests again.

### Golden Rootfs Rollout

1. Rebuild `rootfs-base.img` under [images/arch-base/](../../images/arch-base/README.md).
2. Point `SRV_BASE_ROOTFS` at the new image.
3. Create a canary guest with `ssh srv new <name>` and validate it.
4. After the canary passes, new guests will clone from the new base image.

Rollback for the golden rootfs lane is also path-based: point `SRV_BASE_ROOTFS` back to the previous image before creating more guests.

Important caveat: existing guests keep their own writable `rootfs.img`. There is no host-driven in-place existing-rootfs conversion workflow yet. For an existing guest that needs OS updates today, the supported choices are:

- manage that guest locally with `pacman -Syu`, accepting guest-local drift
- create a replacement guest from the refreshed golden image and migrate the workload or data to it

## Host Hardening And Caveats

- cgroup v2 is required. The runner now depends on a delegated cgroup v2 subtree to place each VM into its own `firecracker-vms/<name>` leaf with enforced `cpu.max`, `memory.max`, `memory.swap.max`, and `pids.max`.
- IPv4 forwarding must stay enabled on the host. Guest egress depends on forwarding packets from each TAP device through the host's outbound interface after the helper installs MASQUERADE and `FORWARD` rules.
- `srv-vm-runner.service` must keep `User=root`, `Group=srv`, `Delegate=cpu memory pids`, `DelegateSubgroup=supervisor`, and a group-accessible socket under `/run/srv-vm-runner/`.
- Do not add `NoNewPrivileges=yes` to `srv-vm-runner.service`; the jailer must drop privileges and `exec` Firecracker on real hosts.
- Keep using the official static Firecracker and jailer release pairing. Distro-provided dynamically linked binaries can fail after chroot before the API socket appears.
- Preserve `/etc/srv/srv.env` across reinstall or upgrade unless you are intentionally changing configuration and have accounted for the stored absolute paths.
- Keep `SRV_DATA_DIR` and `SRV_BASE_ROOTFS` on the same reflink-capable filesystem, such as `btrfs` or reflink-enabled `xfs`. Fast per-instance provisioning still depends on reflink cloning the configured base rootfs.
- Run `sudo ./contrib/smoke/host-smoke.sh` after install, restore, control-plane upgrade, and base-image changes.
