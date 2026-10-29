# Operations runbook

This runbook is for the supported prepared-host path: systemd-managed `srv`, `srv-net-helper`, and `srv-vm-runner` on a Linux host with cgroup v2, `/dev/kvm`, `SRV_DATA_DIR` as a btrfs subvolume root, all base rootfs images on the same btrfs filesystem, `btrfs-progs`, the `user_subvol_rm_allowed` mount option, and the official static Firecracker/jailer release pair.

Same-host reboot recovery is already built into the control plane: when `srv` comes back under systemd, previously active instances are restarted automatically. The steps below cover the larger operator workflows that were previously only implied.

Hybrid memory pools are now part of the supported runtime model:

- Fixed memory remains the default and preserves the original dedicated-reservation behavior.
- Pools reserve host memory immediately at `pool create` time.
- Pooled member VMs consume guest-visible RAM from that shared reservation and are not charged against host memory a second time.
- Pooled guest RAM changes are stopped-only and must stay within the current pool's reserved size.

## Supported Upgrade Lanes

- Guest-local maintenance is still guest-local. Running `pacman -Syu` inside a guest is supported when you want that guest to drift independently, but it is not the control plane's golden-image rollout path.
- Kernel roll-forward is a host-managed lane. Existing stopped guests pick up the current `SRV_BASE_KERNEL` and optional `SRV_BASE_INITRD` on their next `start` or `restart`.
- Rootfs golden-image rollout is a new-guest lane. Updating `SRV_BASE_ROOTFS` changes what future `new` clones from, but it does not rewrite existing guests' writable disks.
- Schema rollout is tied to the control-plane binary. SQLite migrations run during `srv` startup and are currently additive; rollback means restoring the pre-upgrade backup together with the previous binary set.

## Backup

Take backups from a quiesced host so `SRV_DATA_DIR` and the SQLite WAL state are self-consistent.

Include `SRV_JAILER_BASE_DIR/.start-receipts` in the same backup and restore it
with the database. These runner-owned launch records and tombstones fence delayed
requests. They can live outside `SRV_DATA_DIR`; a data-directory snapshot alone
does not capture them. Do not delete receipts to unblock a quarantined launch.
For the archive command below, add the configured receipt directory as a path
relative to `/` if it is not already covered by `var/lib/srv`.

If you need a fast host-local point-in-time copy without shutting the control plane down first, use the built-in snapshot barrier instead:

```bash
ssh srv snapshot create
```

That command is admin-only. It briefly rejects every other SSH command, waits for already admitted commands to finish, checkpoints SQLite, flushes the filesystem, and then creates a bundle of readonly root and disk subvolume snapshots with a manifest under `SRV_DATA_DIR/.snapshots/<timestamp>`. Follow the [host snapshot bundle runbook](../tasks/host-snapshots.md) for component layout, replication, restore, and archive migration.

Snapshot semantics are intentionally limited and explicit:

- control-plane consistent
- stopped guests fully safe
- running guests crash-consistent
- no global atomic point across guest disks; the barrier does not pause guest writes

Important caveats for this path:

- `SRV_DATA_DIR` itself must be a btrfs subvolume root. A plain directory on btrfs is not enough.
- Root snapshots are not recursive: retain every disk component and the manifest, not just the root. `/etc/srv`, environment files, unit overrides, and external runner receipts still need operator-managed backup.
- Remote `btrfs send/receive` replication is intentionally out of the barrier path. If you use it for DR or warm standby, run it after the local snapshot already exists.

1. Stop every VM and Orb and verify no guest remains running, then stop the services.

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

- Preserve the configured paths in `/etc/srv/srv.env`. Instance rows store absolute runtime paths such as `SRV_DATA_DIR/instances/<name>/disk/rootfs.img` (or the legacy `instances/<name>/rootfs.img` before migration), so changing `SRV_DATA_DIR`, `SRV_JAILER_BASE_DIR`, or the base artifact paths during restore is not a supported relocation workflow.
- Keep Orb runtime and jail directories in the same subvolume, not merely the same filesystem: only the Orb socket is hard-linked. The runner bind-mounts the disk, kernel, initrd, and logs.
- A tar archive preserves files, not btrfs subvolume boundaries or readonly properties. Record and recreate those boundaries and properties before starting services; prefer the [bundle restore procedure](../tasks/host-snapshots.md) for btrfs backups.

## Storage Layout Migration

The btrfs-only layout applies to **all VMs and Orbs**. Writable disks live at
`SRV_DATA_DIR/instances/<name>/disk/rootfs.img`, with `disk` a dedicated subvolume.
Legacy instances with `instances/<name>/rootfs.img` migrate automatically on their
next start while stopped, using a durable migration journal for interrupted
attempts. Do not manually move the image, edit database paths, or remove a journal
to bypass an incomplete migration. Retain the pre-upgrade backup for rollback.
This is a storage-layout migration, not a guest filesystem or OS conversion.

Before upgrading:

1. Schedule an outage, stop every VM and Orb, verify all guests are stopped, and
   stop all three services. Take and verify an offline backup of the complete data
   tree, database/WAL, configuration, boot artifacts, and matching runner receipts
   as described above. Keep the original storage untouched until validation.
2. For an existing btrfs host, verify the data directory is a subvolume root and
   every base rootfs is on the same filesystem. Install `btrfs-progs`, configure
   `user_subvol_rm_allowed`, and verify service ownership and directory permissions.
   The option permits owners to delete their subvolumes; it does not grant access
   to arbitrary subvolumes. Add no capabilities to the main `srv` service. An
   existing ordinary directory on btrfs is not sufficient: neither startup nor
   the installer converts it. With all writers stopped and the backup verified,
   prepare a dedicated subvolume at the same configured path, preserve contents,
   numeric ownership, permissions, ACLs, xattrs, hard links and sparse files, and
   retain the original tree until validation. Do not change stored absolute paths
   merely to point at another directory.
3. For XFS or any other non-btrfs host, prepare separate btrfs storage and a data-root
   subvolume **offline**. Copy or restore the stopped data and base images while
   preserving numeric owners, permissions, timestamps, ACLs, xattrs, hard links,
   and sparse disk contents. Verify the copy against the backup. Mount the prepared
   storage at the original configured paths before starting anything: stored
   absolute paths and runner receipts must remain valid. Do not reformat the source
   or assume merely starting srv on XFS will migrate it. There is no in-place XFS
   conversion, automatic formatting, or automatic destructive host-storage command.
4. Ensure Orb runtime and jail directories are in the same subvolume **and mount
   view**. A bind-mount alias of only the jailer directory does not work: socket
   hard-link publication fails with `EXDEV` even with equal subvolume IDs. Check
   complete socket path lengths and custom unit overrides using the
   [jailer upgrade checks](#jailer-path-and-custom-unit-upgrades) below. Follow the
   [snapshot archive migration guidance](../tasks/host-snapshots.md) for an existing
   `.snapshots` archive; do not treat a legacy single-root snapshot as a full bundle.
5. Install matching binaries, start services, and start a stopped canary instance
   to exercise automatic journaled disk migration. Verify its new disk path,
   subvolume, guest data, and stop/start behavior before starting the remaining
   guests. Run the host smoke test and verify a complete host snapshot bundle.
   If validation fails, keep services and guests stopped while restoring the
   matching pre-upgrade data, receipts, configuration, and binaries.

Migration on btrfs can be cheap through reflinks, but shared extents do **not**
guarantee incremental send lineage. Budget for a first full remote send of the
new disk subvolumes; later delta sends require suitable retained parent snapshots.
Remote replication remains outside the snapshot barrier. Cold offload is a
separate, optional disk lifecycle described below.

### Jailer Path And Custom Unit Upgrades

Before stopping guests, inspect the effective unit files and drop-ins, not just
the repository defaults. Build matching `srv`, `srv-vm-runner` and `srv-net-helper`
binaries and check custom flags against their `-help` output. Back up the current
binaries, configuration and units with the data and receipt namespace. Stop any
Orb provider that could create or wake guests before stopping those guests.

Custom stacks must also apply the installer's storage prerequisites without
overwriting their environment or units with defaults: precreate
`SRV_DATA_DIR/.host-locks/payload.lock` as a root-owned regular file, mode 0660,
with the stack's service group. The data-root ancestor chain and `.host-locks`
must be root-owned and not group/world-writable. Runtime does not create this
lock. Prepare the `.snapshots` subvolume and service-owned storage directories
as well; replacing only the executables is not a complete upgrade.

For Orb hosts, test both hard-link publication and Unix socket path lengths in a
disposable fixture before the outage. Equal filesystem/subvolume IDs alone do
not establish that paths can be hard-linked across mounts. The
[path constraints](configuration.md#path-constraints) include a verified short
`j`/`fc` layout for 60-character dispatcher names and srv's maximum 63-character
names. A short symlink to Firecracker is not equivalent to a regular executable
copy. Do not solve socket length by putting the jailer in a different subvolume
or by shortening only its mount alias.

If changing an existing jailer base or Firecracker executable basename:

1. At the quiesced backup boundary, use the **old** configuration to establish
   that its runtime workspaces, sockets, mounts and cgroups are no longer active.
   Resolve uncertain accepted starts using their original identities; never
   delete receipts or inspect an empty new workspace as a substitute for this
   proof.
2. Preserve the complete authoritative `.start-receipts` namespace, including
   locks and any cold/migration state, together with its database and disks when
   relocating it. Do not merge receipts from failed rollout trees or invent
   replacements. Retain original data/configuration for a coordinated rollback.
3. Keep control plane, runner, drain and cleanup-proof configuration consistent
   with the new base and executable. Update custom systemd access restrictions
   only as needed for the new trusted paths; preserve existing hardening.
4. Start one stopped canary per stack before returning other guests. Verify data
   identity, actual Orb agent/socket communication, repeated stop/start and exact
   receipt handling. Start providers last. Roll back matching data, receipts,
   configuration and binaries together if validation fails.

Two custom-unit compatibility checks are especially important:

- `ExecStop` must use `srv-vm-runner -drain-only -socket <this runner's socket>`.
  Drain now calls the running runner in its mount namespace; a custom stack must
  not accidentally drain the default stack. Keep the socket consistent with
  `ExecStart` and `SRV_VM_RUNNER_SOCKET`. Clear the inherited `ExecStop` before
  defining its replacement in a drop-in.
- Older private net-helper drop-ins may pass `--proof-instances-dir`,
  `--proof-jailer-base-dir`, `--proof-firecracker-bin` and `--proof-cgroup-root`.
  The current net-helper rejects those flags. Runtime absence proof now runs in
  the root VM runner at `/vm/cleanup-proof-v1`, using that runner's configured
  paths and delegated cgroup; network absence proof remains in the net-helper.
  Upgrade the binaries together and adjust only the obsolete arguments, keeping
  the intended socket, TAP user, client group and access restrictions. If replacing
  an `ExecStart` in a drop-in, clear its old value before setting the replacement.
  Do not bypass proof failures. The current deletion-proof operation is for
  native Orbs; legacy tailnet-VM proof callers are rejected, not transparently
  migrated to this API.

After successful conversion, future backups must include the data-root subvolume
and every nested disk subvolume. A parent-only snapshot no longer captures the
disks. See [host snapshot bundles](../tasks/host-snapshots.md); separately preserve
external configuration, binaries, receipts and `/boot` where applicable.

## Optional cold disks over SSH

Cold storage is disabled by default and applies to both VMs and Orbs. It uses
`btrfs send/receive` through the root VM runner, without adding capabilities to
the `srv` service. There is no demand paging: `start` restores the entire disk
before booting. A cold instance remains `stopped` in the instance lifecycle.

Use a dedicated, trusted btrfs storage server. Build `./cmd/srv-cold-store` and
install the binary as `/usr/local/sbin/srv-cold-store` there, owned by root and
not writable by other users. Prepare a dedicated root-owned directory (mode
0700) on btrfs for **each backend**. Every ancestor must also be root-owned,
nonsymlink and not group/other writable; a private directory under `/tmp` is
not accepted. Restrict the backend's SSH key to this forced
command in the storage server's root `authorized_keys`, for example:

```text
restrict,command="/usr/local/sbin/srv-cold-store --root /storage/backend-a" ssh-ed25519 <backend-public-key>
```

Do not use this key for an unrestricted remote shell. Protect the server,
namespace and SSH configuration: **btrfs receive is not a security boundary
against a malicious send stream**. The backend and storage server are mutually
trusted administrators. Pin the server host key through a verified channel;
never disable host-key checks. Keep the backend's private key and known-hosts
file root-owned. The private key must grant no group/other permissions (use
0600); known-hosts may be 0644. Keep both under root-owned protected
directories such as `/etc/srv/cold-storage/` (not `/tmp` or a user home).

Upgrade the storage helper and `srv-vm-runner` before enabling cold storage in
`srv`. The runner validates credentials and required executables at startup.
An old runner's missing API fails closed with upgrade advice; never bypass it
to delete an instance whose remote state is unknown. Configure the backend's
`/etc/srv/srv.env` and restart both matching binaries:

```sh
SRV_COLD_STORAGE_HOST=storage.example.net
SRV_COLD_STORAGE_KEY=/etc/srv/cold-storage/id_ed25519
SRV_COLD_STORAGE_KNOWN_HOSTS=/etc/srv/cold-storage/known_hosts
# Optional: attempt offload after a successful explicit stop command.
SRV_COLD_STORAGE_AFTER_STOP=true
```

The runner uses root SSH with batch mode, pinned host keys, no agent, and no SSH
config files. Transfers have a six-hour overall runner deadline (override with
runner `--cold-storage-timeout`), a 15-second SSH connect timeout and keepalives.
The storage helper's `--timeout` also defaults to six hours. Size both deadlines
and the caller's timeout for transfer plus verification; a shorter deadline wins.
Backend cold lifecycle commands send SSH keepalives every 30 seconds without
changing stdout/stderr; srv-front already keeps both SSH hops alive. Thus quiet
transfer work does not hit the five-minute idle timeout. `disk status` reads an
atomic journal snapshot without waiting for the transfer lock; it is advisory,
not permission to mutate a disk.
Each offload hashes its immutable local snapshot once and verifies the received
remote disk once before publication. An existing base is still hashed once
locally; metadata/UUID checks do not rehash it remotely. Hydration verifies the
received disk locally. `Stat` checks immutable publication and identity, not
media integrity; use filesystem scrubbing separately. These bounds are not a
throughput guarantee. The remote namespace lock serializes transfers, so a long
upload can delay another instance's hydration; deletion scans all manifests.
Both hosts need `btrfs-progs`; the backend also needs OpenSSH client and `losetup`.
The runner's private staging beneath `SRV_JAILER_BASE_DIR` must be on the same
btrfs filesystem as the instance disks; keep its receipt directories root-owned.
Allow transient space for receiving the full disk before publishing it. Disk
reservation accounting remains conservative: eviction does not remove the
instance's capacity reservation.

```sh
ssh srv disk offload demo
ssh srv disk status demo
ssh srv start demo          # restores first if cold
ssh srv disk hydrate demo  # restores without booting; must be stopped
ssh srv disk purge demo    # removes retained remote generation; local disk required
```

Names may be backend-qualified through srv-front, e.g. `disk status backend-a/demo`.
Explicit mutations on native Orbs require an admin; visible owners can inspect
status. Automatic offload follows the existing authorized stop operation.
The stop response may wait for the optional transfer; transfer failure is logged
but does not turn a completed guest shutdown into failure. Restart, host drain,
and migration shutdown do not automatically offload. Existing stopped instances
can be offloaded explicitly; there is no periodic eviction scheduler.

The runner keeps a durable incarnation-bound journal under its start receipts
and a `cold-disk.json` fence beside instance runtime metadata. Upload publication,
content SHA256, size and subvolume identity are verified before local eviction.
Lost upload replies retain the local disk and conservatively track a possible
remote copy. Retry `disk hydrate` to recover interrupted local work, then retry
offload. A `hot` disk remains usable during remote outages. If a genuinely cold
disk has no local copy, an unreachable remote makes start fail; srv never boots
the distribution base or an older remote generation as a substitute.

Hydration preserves the immutable remote generation until `disk purge`, ordinary
instance deletion, or the next offload removes it. Once local authority resumes,
that remote generation is **not** a fallback for subsequent local loss. Ordinary
deletion waits for acknowledged remote deletion and retains deleting intent on
failure. Cleanup proofs reject any retained cold-storage state rather than
claim remote erasure without evidence. Do not manually delete journals, markers,
remote objects, or bases. Keep the original SSH destination configured until all
its instances have been hydrated and purged; disabling automatic offload alone
does not prevent hydration.

New distribution clones can send incrementally against their matching immutable
content-addressed base. The base is copied once to the remote and retained.
Hydration uses that base when its exact identity still exists locally; otherwise
it requests a full stream. Legacy migrations and template clones without a
matching distribution base use full transfers. Remote snapshots share extents;
remote base retention is separate from per-instance generation deletion.
Physical space is reclaimed only when no local backup or host snapshot still
references the same extents. Small permanent deletion records remain on both
ends to reject delayed requests; do not garbage-collect them independently.

Payload checksum corruption does not prevent ordinary remote deletion. If a
manifest is corrupt, restore its authoritative metadata from backup or explicitly
abandon the affected objects using the storage server's local administrator CLI:

```sh
srv-cold-store --root /storage/backend-a --discard UUID1,UUID2
```

This is irreversible and unavailable through the SSH protocol. Include every
corrupt-manifest object and every dependent that must be discarded; all retained
manifests must be valid and must not reference the discard set. The command never
guesses dependencies or repairs unsafe filesystem layouts. It validates the
complete set, permanently fences all IDs, then removes them. After interruption,
retry the **same complete set**; never remove tombstones. Afterwards retry the
backend deletion/purge to settle its journal. Do not discard the sole copy of a
disk you intend to retain.

An administrator may run this CLI from a normal SSH login using `sudo`; it is
rejected in a forced-command environment (`SSH_ORIGINAL_COMMAND`). If the
manifest file itself is missing, first restore that file from authoritative
backup metadata. If the disk's read-only flag was cleared, stop all writers,
verify that the path is the intended object subvolume, and restore its read-only
flag before retrying. Discard is not a general filesystem-repair tool.

For `cold disk identity mismatch`, **do not purge a different incarnation's
journal or clear deletion records to make Start work**. Stop srv and the runner,
prevent remote writes, and preserve the database, instance directory, runner
receipts and remote namespace for inspection. Compare the database instance ID
with the journal's `instance_id`, disk UUID and remote object ID. Restore the
matching control-plane/disk/runner backup set using the host restore procedure;
do not combine an older database with newer journals. Restore the original remote
destination configuration, then use `disk status`, `disk hydrate` and `disk purge`
for that matching incarnation before deleting/reusing its name. If no matching
backup exists, keep the name fenced and recover the data on an isolated host;
there is deliberately no automatic cross-incarnation erasure. Disabling cold
storage with a missing public marker does not make a surviving root journal safe
to ignore.

An unexpected `.stage-*` name also fails closed. With the backend and remote
writers stopped, inspect and preserve the foreign entry outside the managed
namespace before retrying. Never delete unknown contents automatically or rename
a valid interrupted `.stage-put-UUID` / `.stage-delete-UUID`; their recovery is
managed by the helper.

Disk mutations, export and backup/template creation require hydration first.
Complete host snapshots reject cold/transitional disks **and hot disks with
retained cold-storage state**: hydrate and purge them before taking a bundle.
This preserves the self-contained snapshot contract rather than silently
producing a backup that depends on the storage server. Cold storage is not an
independent disaster-recovery backup: back up the control-plane metadata,
runner journals, configuration and remote namespace as well.

Before enabling eviction on production data, run the opt-in kernel integration
on two distinct disposable btrfs filesystems as root:

```sh
COLDSTORE_TEST_SOURCE=/mnt/test-source COLDSTORE_TEST_REMOTE=/mnt/test-remote \
  go test ./internal/coldstore -run TestBtrfsTwoFilesystems -count=1 -v
```

It exercises full and incremental send/receive in both directions and parent
retention. Also run a canary through the actual SSH forced command: offload,
disconnect remote access, verify failed cold start preserves metadata, reconnect,
hydrate, boot and compare guest data, then stop and purge/delete. Unit tests with
substitute filesystem operations do not establish kernel or live-SSH correctness.

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

If the exported VM was in pooled mode, import preserves that metadata. The destination host must already have the referenced memory pool state available through the restored control-plane database before the VM can be started successfully.

## Restore Or Rebuild A Host

1. Prepare a fresh host with the normal prerequisites: Tailscale, cgroup v2, `/dev/kvm`, a btrfs data-root subvolume sharing its filesystem with every base rootfs image, `btrfs-progs`, `user_subvol_rm_allowed`, and the repo checkout.
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
5. Restore the saved `SRV_DATA_DIR` tree to the same path, including its subvolume boundaries and readonly snapshot properties. For snapshot bundles use the [bundle restore procedure](../tasks/host-snapshots.md); extracting a tar alone does not recreate subvolumes.
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

The launch-outcome protocol requires matching `srv` and `srv-vm-runner` binaries.
Do not run the new control plane with an old helper or mix lifecycle writers.
Stopping the runner can stop its guests: use an approved maintenance window.

Cold enrollment also requires a rebuilt golden rootfs containing the updated
`srv-bootstrap` script. It reports the guest's stable Tailscale node ID with a
per-launch nonce over the serial channel. An old base image will leave new guests
pending rather than accepting an unrelated same-name Tailscale record. Existing
enrolled guests do not need an in-place rootfs update for warm starts.

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

If rolling back from a version that records `stopping`, first finish pending
stops with `ssh srv stop <name>` under that version. Older versions do not
recognize the state and may automatically start those guests. Never mix old and
new control-plane writers against the same database. Swapping only the binary
is not a substitute for the restore-based procedure below.

1. Stop the services.
2. Reinstall the previous binaries and previous static Firecracker/jailer pair if those changed.
3. Restore the pre-upgrade backup of `/etc/srv`, `SRV_DATA_DIR`, and the matching runner receipt directory. Confirm no post-backup guest remains running before replacing state; never combine old database rows with newer runtime state.
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

- cgroup v2 is required. The runner depends on a delegated cgroup v2 subtree for both fixed and pooled layouts:
- Fixed VMs use `firecracker-vms/<name>` leaves with enforced `cpu.max`, `memory.max`, `memory.swap.max`, and `pids.max`. The guest sees the configured RAM; the host `memory.max` adds a small Firecracker overhead reserve (`max(256 MiB, RAM/16)`) so VMM/page-table overhead does not consume guest-visible memory headroom.
- Pooled VMs use `firecracker-pools/<pool>/<name>` leaves. The pool parent holds the hard memory reservation with `memory.max=<pool size>` and `memory.swap.max=0`, while child VM cgroups still carry CPU and PID limits.
- IPv4 forwarding must stay enabled on the host. Guest egress depends on forwarding packets from each TAP device through the host's outbound interface after the helper installs MASQUERADE and `FORWARD` rules.
- `srv-vm-runner.service` must keep `User=root`, `Group=srv`, `Delegate=cpu memory pids`, `DelegateSubgroup=supervisor`, and a group-accessible socket under `/run/srv-vm-runner/`.
- Do not add `NoNewPrivileges=yes` to `srv-vm-runner.service`; the jailer must drop privileges and `exec` Firecracker on real hosts.
- Keep using the official static Firecracker and jailer release pairing. Distro-provided dynamically linked binaries can fail after chroot before the API socket appears.
- Preserve `/etc/srv/srv.env` across reinstall or upgrade unless you are intentionally changing configuration and have accounted for the stored absolute paths.
- Keep the `SRV_DATA_DIR` subvolume and every base rootfs image on the same btrfs filesystem. Fast per-instance provisioning still depends on reflink cloning the configured base rootfs.
- Use pooled mode only for workloads that tolerate an elastic memory contract. Fixed mode remains the supported choice for databases, JVMs, and other memory-sensitive services.
- `status` is now the operator view for host memory accounting: it separates fixed reservations from pool reservations so pooled members do not look like additional dedicated host claims.
- `inspect` and `top` are the operator views for VM memory behavior: `inspect` shows `memory-mode` and host reservation style, while `top` shows host-resident live memory against configured guest RAM. Fixed and pooled VMs both use a virtio balloon with free-page reporting so idle guest pages can be returned to the host; pooled rows also show the pool name.
- On reboot recovery, pooled VMs recreate their pool-parent cgroups before restart; if a recovered VM references a missing pool record, treat that as a control-plane state restore problem rather than a guest runtime issue.
- Run `sudo ./contrib/smoke/host-smoke.sh` after install, restore, control-plane upgrade, and base-image changes.
