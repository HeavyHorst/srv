# Host snapshot bundles

`ssh srv snapshot create` is an admin-only local disaster-recovery operation.
It rejects new control-plane commands, drains admitted commands, inventories
storage, checkpoints SQLite, and flushes the filesystem under the existing
snapshot barrier. It snapshots the data root **and every managed disk subvolume**
read-only, flushes the complete bundle, atomically publishes it, then flushes the
archive before reporting success. Sending just the root snapshot is **not a
complete backup**: btrfs snapshots are not recursive.

## Layout and consistency

`SRV_DATA_DIR` must be a btrfs subvolume. Supported nested disk subvolumes are:

- `instances/<name>/disk` (both VMs and Orbs)
- `templates/<owner>/<template>/disk`
- `backups/<name>/<backup>/disk`
- `image-bases/<sha256>/disk` (content-addressed distribution image bases)

Each must contain a nonempty regular `rootfs.img`. Before lazy migration,
instances, templates and backups may instead have a regular `rootfs.img` directly
in their object directory, with no `disk` entry. These **legacy files are captured
in the root component**, so stopped or running guests need not be started just
to make host backups available. SQLite instance paths must match the actual
canonical or legacy layout. Image bases must use the new subvolume layout.

Missing/nonregular/empty disk images, simultaneous legacy and canonical layouts,
pending disk migration journals/stages, ordinary `disk` directories, storage-path
symlinks, and unknown nested subvolumes cause failure, not partial success.
Runtime links such as `instances/<name>/firecracker.sock` are allowed and captured
as links, not followed. Do not mutate host storage outside srv during capture.

`.snapshots` must itself be a subvolume on the same filesystem; srv creates it if
absent. This keeps earlier bundles out of subsequent root snapshots. An existing
ordinary `.snapshots` directory is rejected: with srv and guests stopped, move
the legacy archive **outside the live data root**, create the archive subvolume,
and retain/migrate the old snapshots separately. Do not simply rename the old
archive within the data root. Legacy single-root snapshots are not v1 bundles.

Published bundles are ordinary directories `.snapshots/<timestamp>/` containing
`manifest.json`, `root`, and flat `disk-000000`, `disk-000001`, … readonly
subvolumes. Manifest format v1:

```json
{
  "version": 1,
  "id": "20260101T000000.000000000Z",
  "created_at": "2026-01-01T00:00:00Z",
  "components": [
    {"source": ".", "path": "root", "source_uuid": "<live-root-uuid>", "snapshot_uuid": "<snapshot-root-uuid>"},
    {"source": "instances/example/disk", "path": "disk-000000", "source_uuid": "<live-disk-uuid>", "snapshot_uuid": "<snapshot-disk-uuid>"}
  ]
}
```

`source` is relative to the live data root; `path` is relative to the bundle.
The root is first, followed by all inventoried disks in lexical walk order.
UUID values above are illustrative; real manifests record btrfs source and
snapshot UUIDs for every component. Source identity is checked again after
capture; identity inspection errors or changes prevent publication. Legacy
regular files have no separate subvolume UUID or component: their ownership and
contents are within `root` and its captured SQLite/metadata records.
Directories named `.incomplete-<timestamp>-<random>` are never usable backups,
even if they have a manifest. A manifest describes the intended inventory; its
presence alone does not prove completion. Failure after publication but before
the final flush is reported as durability unconfirmed, not success.

- **Control-plane consistent:** SQLite is checkpointed and lifecycle mutations
  remain blocked for the entire bundle capture.
- **Stopped guests fully safe:** their disk contents are consistent on disk.
- **Running guests individually crash-consistent:** each disk is captured at a
  different instant, like losing power independently. This is **not** a global
  point in time or an application-consistent distributed backup. Guest RAM is
  not captured. Stop guests/application writers first when stronger consistency
  is required; a running per-VM backup is not application-consistent either.

## Replicate the whole bundle

Replication happens after publication, outside the barrier. Keep the source
bundle until every component is received and verified. On the receiving btrfs
filesystem create a private staging directory, e.g.
`/backup/srv/.incomplete-<id>`; never receive directly into a completed bundle.
For **each** `components[].path`, send its own stream:

```bash
# Bash; BUNDLE is an absolute, completed local bundle path.
set -euo pipefail
jq -e '.version == 1' "$BUNDLE/manifest.json" >/dev/null
while IFS= read -r component; do
  sudo btrfs send "$BUNDLE/$component" |
    ssh backup-host "sudo btrfs receive /backup/srv/.incomplete-$ID"
done < <(jq -r '.components[].path' "$BUNDLE/manifest.json")
scp "$BUNDLE/manifest.json" "backup-host:/backup/srv/.incomplete-$ID/manifest.json"
```

Set `ID` to the manifest ID; create the destination staging directory beforehand
with permissions allowing the receiving operator to copy the manifest. These
commands assume trusted srv-generated paths/IDs, not an untrusted manifest.
On the receiver check the version and ID, require unique source/path entries,
one root entry, supported relative disk paths without traversal, and **every**
listed component present as a readonly btrfs subvolume. Compare the received
manifest with the sender's. On the sender, `btrfs subvolume show` must report each
component's UUID equal to its `snapshot_uuid`. On the receiver, the **Received
UUID**, not its newly allocated local UUID, must equal `snapshot_uuid`. Do not
compare filesystem-local numeric subvolume IDs across hosts. Check successful exit status for every send/receive;
do not publish on a truncated stream or missing component. Flush with
`sync -f /backup/srv/.incomplete-<id>`, rename without overwriting to
`/backup/srv/<id>`, and `sync -f /backup/srv`. Protect the manifest and components
against modification. Incremental replication must track a valid parent for
each component separately; a root-only incremental stream is insufficient.

## Restore offline

1. Stop srv and **all** guest runners; stop external writers. Restore on an
   isolated host first and avoid concurrently starting duplicate guest/network
   identities. Preserve the current data tree for rollback.
2. Validate the complete bundle as above. Use a fresh, nonexistent restore path
   on the same btrfs filesystem as the bundle (receive there first if needed).
   Create a writable root clone:
   `btrfs subvolume snapshot "$BUNDLE/root" "$RESTORE"`.
3. For every non-root manifest entry, remove only the empty placeholder at
   `$RESTORE/<source>` using `rmdir` (fail if it is not empty); create a writable
   clone with `btrfs subvolume snapshot "$BUNDLE/<path>" "$RESTORE/<source>"`.
   Do not copy the disk image alone or flatten the subvolume. Check every exit
   status. Templates, backups and image bases must also be restored as disk
   subvolumes; use `snapshot -r` for those immutable components instead. Legacy
   file-only objects already exist in the root clone: leave them there for lazy
   migration, and do not invent additional components or move their files.
4. Remove the empty `$RESTORE/.snapshots` placeholder using `rmdir` and create
   it with `btrfs subvolume create "$RESTORE/.snapshots"`. The old archive is
   deliberately not inside the root snapshot; retain it separately. Verify all
   listed rootfs images and subvolumes, run an offline SQLite integrity check on
   `state/app.db`, and flush the filesystem.
5. With services still stopped, switch the reconstructed root into the original
   `SRV_DATA_DIR` location/mount. Preserve the original absolute path because
   stored paths refer to it. Restore host configuration and prerequisites, then
   start srv and deliberately start guests, checking disk recovery/application
   health. Running-guest disks may need journal replay. Do not regard a root-only
   restore as successful even if SQLite opens.

## Retention, failures, and confidential data

Bundles retain deleted guest/template/backup data until **all** matching archive
components are removed. For cleanup proof, inspect every completed manifest's
`components[].source`; also inspect `.incomplete-*` bundles, whose manifests may
be absent or only partially written. **The root component can contain legacy
guest/template/backup images even without a separate disk entry.** Inspect its
captured tree/metadata too. Match recorded snapshot UUIDs before deleting
components; do not treat a replaced path as the same snapshot. Never infer erasure from removing live
`instances/<name>/disk` alone. The archive can also contain legacy snapshots.
With no active snapshot/replication, delete each bundle's readonly component
using `btrfs subvolume delete`, then remove its manifest and empty directory;
do not use recursive filesystem deletion as a subvolume retention algorithm.
Confirm filesystem sync/deletion completion before claiming local reclamation.
Off-host copies require separate retention/erasure handling.

Not included: `/etc/srv/srv.env`, systemd overrides, external images/configuration,
Firecracker/jailer binaries, or any data outside `SRV_DATA_DIR`. Back these up
separately. Optional [cold storage](../reference/operations.md#optional-cold-disks-over-ssh)
is separate from host-bundle replication. Snapshot creation rejects every
`cold-disk.json` marker, including retained remote copies of now-hot disks. Hydrate
and purge those generations first; a successful bundle remains self-contained.

## Integration validation

On a disposable btrfs filesystem, create a data-root subvolume with VM, Orb,
template, backup and image-base disk subvolumes containing distinct data, plus
legacy file-only objects and runtime socket symlinks. Capture a bundle,
mutate live disks, and verify all snapshot images retain the original data.
Capture again and verify `root/.snapshots` is an empty placeholder. Check missing
images and unknown nested subvolumes fail. Inject a component/sync failure and
verify no success is reported. Send/receive **all** components to another test
filesystem and perform the offline reconstruction above; check SQLite and all
disk contents and boot stopped/restored guests. Repeat with running writers to
exercise crash recovery, without asserting cross-guest simultaneity. Unit tests
cover inventory and publication failure paths; they do not replace this test.
