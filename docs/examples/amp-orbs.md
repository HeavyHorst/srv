# Self-hosted Amp orbs on srv

`srv-orb-dispatch` connects [Amp's self-hosted orb provider](https://ampcode.com/docs/orbs/self-hosted)
to existing srv SSH commands. It works with one srv host or with `srv-front` for
automatic placement. It does not replace srv's control plane or the separate
[Amp runner plugin](amp-runners.md).

This is an initial integration: subprocess tests cover the dispatch contract,
but image boot and the Amp connection still need validation on a real srv host.

| Amp job | srv behavior |
| --- | --- |
| First `spawn` | Create an Orb, wait for its host-only vsock agent, start `amp orb bootstrap` as `user`. |
| Later `spawn` | Start the same stopped Orb, or reuse the running Orb; restart bootstrap with the fresh token. |
| `pause` | Stop the VM and retain its disk; with `ORB_SNAPSHOT_ID`, save an immutable prepared disk template. |
| `delete` | Remove the native Orb; retain any legacy backups, shared snapshots, project templates and the exact cleanup receipt. |

Orbs use the same Firecracker, capacity accounting and placement infrastructure
as VMs, but have a separate workload kind and default inventory:

```bash
ssh srv list              # ordinary VMs only
ssh srv orb list          # Orbs only
ssh srv list --all        # both, with their kind
ssh srv orb new demo --image amp-orb
```

Names remain unique across both kinds on each backend. Existing records remain
VMs; they are not inferred or converted from image names. Previously created
Tailscale-based Orbs need a deliberate rebuild rather than automatic adoption.

Orbs do not support individual backups or restore. `backup create`, `backup list`,
`backup delete` and `restore` reject Orbs, including for admins. Legacy backups
are left untouched by deletion and may be cleaned up manually if desired. A reused
name may also have backups of a former VM. Project templates and shared host
snapshots are unchanged.

Common `inspect`, `start`, `stop`, `delete` and capacity commands still
operate on both kinds. The same list separation applies through srv-front.

There is **no RAM suspend or automatic migration**. New orbs reuse a known
`ORB_SNAPSHOT_ID` on its recorded backend; unknown IDs fall back to the base
image. Existing orbs always retain their own disk and use Amp's resume workflow. Processes, Terminal
sessions and in-memory state do not survive a pause. Let Amp manage development
services so they can be restarted after wake-up.

## Project snapshots

Amp requests a snapshot by sending `pause` with `ORB_SNAPSHOT_ID`, then wakes the
same orb again. srv stops the source, reflink-clones its disk, sanitizes the copy,
and publishes it atomically in an owner-scoped template namespace. The source
disk remains unchanged. A later new orb gets a clone of this prepared template
and fresh srv allocation, rather than a migration of the source VM. Templates
retain their workload kind: an Orb cannot clone a normal VM template or vice versa.

Templates are host-local. The dispatcher persists their backend mapping and pins
new template-based VMs to that host, even when `SRV_ORB_HOST` changes. There is no
cross-host template replication or failover. A known but incomplete mapping,
image mismatch, unavailable backend or missing template fails rather than silently
claiming snapshot reuse. A missing mapping permits Amp's base-image fallback.

Snapshot IDs are immutable: repeat requests from the same source reuse the saved
copy; another source cannot replace it. Templates survive source-orb deletion.
Back up both dispatcher mappings and backend templates. Restoring only one side
does not restore usable snapshots.

Amp documents no snapshot-delete dispatch operation. Retire a snapshot in Amp
first, stop the provider and wait for outstanding jobs, then delete the backend
template and its `project-*.json` dispatcher mapping. The mapping contains the
backend and template name. Keep lock files. Backend commands, executed as the
same srv owner, are:

```bash
ssh srv-front -- --json project-template create ORB_VM_NAME TEMPLATE_ID --host a
ssh srv-front -- --json orb new NEW_ORB_NAME --image amp-orb --template TEMPLATE_ID --host a
ssh srv-front -- --json project-template delete TEMPLATE_ID --host a
```

Use no `--host` when connecting directly to srv. Template creation requires a
stopped source. No RAM, running processes or terminal sessions are copied.

Backends need `debugfs` and `e2fsck` from e2fsprogs. Cleanup operates on the copied
ext4 disk in userspace; it never mounts the guest filesystem on the host.
It removes machine/Tailscale identities and named Amp authentication/session
files, while retaining prepared settings, skills, CLI binaries and project data.
Spaces and Unicode names are supported; ambiguous debugfs output and escape/control
characters in cleanup paths fail closed. Filesystem checks honor job cancellation.
Templates remain **confidential artifacts in the source's trust domain**:
removing active identity files is not secure erasure of free blocks/journals and
does not remove arbitrary project secrets. Do not distribute them as public images.

## Prepare an image

Build an [Arch](../../images/arch-base/README.md#optional-native-amp-orb-preparation)
or [Debian](../../images/debian-base/README.md#optional-native-amp-orb-preparation)
image with `SRV_AMP_ORB=1`. This adds `user:user`, passwordless sudo, Amp,
Node.js, Python, tmux, the Desktop stack and a narrow host-only bootstrap agent.
Orb builds omit Tailscale and mask native srv enrollment; ordinary image builds
retain their existing boot and enrollment behavior. Use an Orb-enabled kernel
with built-in virtio-vsock support and rebuild older Orb images for this transport.

Both image builders include Docker and Compose v2; the supported command is
`docker compose`. Amp runs as `user` with passwordless sudo, not as root:
use `sudo docker compose` for daemon operations with the default root-owned
Docker socket. This is an access convention, not a privilege boundary against
the agent, which already has sudo. No Docker-group grant or socket-mode change
is required. Browser/test dependencies belong in the repository's `.agents/setup`.
Podman, Nix and legacy Compose v1 are not interchangeable provider contracts;
projects that need them must prepare and qualify those environments explicitly.

Register the prepared kernel/rootfs as `amp-orb` in
[`SRV_IMAGES_FILE`](../reference/configuration.md#guest-artifacts) on every
eligible backend. Keep any existing catalog entries. Example additional entry:

```json
{
  "amp-orb": {
    "kernel": "/var/lib/srv/images/amp-orb/vmlinux",
    "rootfs": "/var/lib/srv/images/amp-orb/rootfs-base.img"
  }
}
```

Check the configuration reference for the catalog's complete format. Do not use
the raw Amp OCI image as a Firecracker rootfs. In this adapter, `ORB_IMAGE` is an
explicit prepared **srv image name**, not an OCI URL. Set it in the provider's
environment as shown below, otherwise Amp supplies its default OCI URL and the
adapter rejects the job before creating a VM.

Desktop preparation copies `/opt/amp-wayland` from a checksum-pinned layer of
Amp's official amd64 image and installs its distro-specific runtime dependencies.
It needs build-time access to `us-central1-docker.pkg.dev` and blob redirects, but
no Docker daemon. The source digest is retained inside the guest at
`/opt/amp-wayland/srv-layer.sha256`. Bootstrap and login shells load the bundled
program paths from `/etc/amp-orb.env`. The bundled executables use private RPATHs
for their libraries; a global `LD_LIBRARY_PATH` would break newer distro clients.

The catalog name is your choice. If you register the image as `amp-orb-desktop`,
normal srv clients can use:

```bash
ssh srv orb new demo --image amp-orb-desktop
```

For the Amp provider, set both `ORB_IMAGE=amp-orb-desktop` and
`SRV_ORB_IMAGE=amp-orb-desktop`. srv-front already considers image availability
when placing new VMs. This is provider configuration, not a new per-thread Amp
image selector. Existing VMs keep their original rootfs; rebuild and create a
new orb to pick up Desktop support. Desktop processes restart after pause rather
than resuming their old in-memory session.

## Provider machine and identity

Use a Linux machine with Amp, OpenSSH, network access to the control endpoint,
and a persistent local filesystem supporting `flock`, atomic rename and
`fsync`. It does not need KVM if the backends run elsewhere.

Give the provider a dedicated Tailscale node, optionally tagged, and explicitly
map its **stable node ID** to a service owner. For example, on srv backends:

```bash
SRV_PROVIDER_NODES='{"nPROVIDER1":"amp-orbs"}'
SRV_ALLOWED_USERS='service:amp-orbs'
```

Append to existing allowlists rather than replacing their entries. With
srv-front, also configure its `provider_nodes` map and `allowed_users` for the
same service owner; see [provider identities](../reference/configuration.md).
The frontend remains a separate trusted forwarding node, not a provider node.
Tags or SSH usernames alone never grant a service identity. Unconfigured tagged
clients remain rejected. Service owners cannot be configured as admins.

All orbs and project templates from this provider belong to `service:amp-orbs`,
isolated from normal users' VM ownership. The dispatch contract contains no Amp
member ID, so this does **not** separate members within one Amp workspace. Use
separate provider identities for separate trust domains. An existing untagged
user provider still works; switching its identity does not transfer existing
VMs or templates automatically.

Configure ACLs so the provider can reach:

- The srv or srv-front SSH endpoint, under the existing allowed-user rules.
- `ampcode.com` and `static.ampcode.com` for provider traffic and CLI updates.

Guests need outbound HTTPS/WSS to `ampcode.com`, `tunnel.ampcode.com` and
`static.ampcode.com`, plus their Git hosts and package registries. The dispatcher
uses the per-VM Amp CONNECT gateway reported by `inspect` when present; otherwise
bootstrap uses direct egress. The gateway's existing private-address restrictions
still apply. Private Git hosts or a private `AMP_URL` require an appropriate
network/proxy policy; this integration does not weaken it automatically.

Proxy bypass is provider configuration, not a security boundary. Bootstrap
writes a fixed bypass list for guest-local services, MMDS, the CGNAT range and
`.ts.net` tailnet MagicDNS names. To send additional destinations past the
gateway — an internal registry, a private DNS suffix — set
`SRV_ORB_PROXY_BYPASS` in the provider environment to a comma-separated list
of up to eight entries (512 characters joined). Each entry is a hostname, an
IPv4 literal, or a leading-dot DNS suffix; an optional trailing dot (the FQDN
form) is accepted verbatim but only matches clients that compare the same
form. The entries are client-native `NO_PROXY` tokens, not a universal
matcher: label-boundary subdomain matching agreed across urllib, curl and
Go's `httpproxy` in the runs verified for this change, but apex handling does
not — urllib and curl match the bare apex host for a leading-dot entry while
Go's `httpproxy` does not, so configure the bare hostname (which matched apex
and subdomains in all three) when the apex itself must be covered. CIDR
ranges and IPv6 literals are not accepted in this field for the same reason:
Python's urllib ignores CIDR entries entirely (including the built-in CGNAT
entry), while curl and Go honor them for IP literals.
Empty entries, duplicates and repeats of built-in entries are configuration
errors. The dispatcher validates the list and the guest independently
re-validates every entry, before any bootstrap generation is consumed, and
appends the list to the unchanged built-in entries; unset keeps exactly the
built-in list, and repository setup must not be used to repair this provider
networking. Bypass only removes the gateway from the path: the guest still
needs actual routing to the destination.

Setting `SRV_ORB_PROXY_BYPASS` adds a `proxy_bypass` field to the bootstrap
payload. A new dispatcher with the variable unset, and an older dispatcher
against a new guest image, both keep working because the field is simply
absent; but a *set* variable needs the dispatcher, srv backend and orb guest
image upgraded together, since older components reject the extra field and the
bootstrap fails rather than silently dropping the destination.

The control endpoint's SSH host key must be installed in the service user's
`known_hosts` through a trusted channel. Control connections use strict host-key
checking. The provider never connects to guest SSH: the authenticated backend
forwards bootstrap over a local Firecracker vsock endpoint. Its guest listener
accepts only host CID 2 and only ping/bootstrap requests, not arbitrary commands.
SSH username selection does not grant control-plane permissions.

### Optional project Tailscale

Guests do not require a Tailscale account, auth key or enrollment for readiness,
bootstrap, Terminal, Desktop or portals. The srv control plane and provider still
use Tailscale for their existing authenticated transport.

For project-specific tailnet access, follow
[Amp's Tailscale setup](https://ampcode.com/docs/orbs/customizing): install the
client in `.agents/setup`, then start/authenticate it in `.agents/resume` using
Amp OIDC and an ephemeral node. Tailscale is not preinstalled or masked; the
project owns its installation and lifecycle. Do not bake reusable credentials
into the image or snapshot.
Keep the Amp connection independent of project networking; avoid an exit node
and configure proxy bypasses for private destinations when necessary.

## Install and run

Build and install the adapter:

```bash
go build -o /tmp/srv-orb-dispatch ./cmd/srv-orb-dispatch
sudo install -m 0755 /tmp/srv-orb-dispatch /usr/local/bin/srv-orb-dispatch
```

Create a dedicated local account `srv-orbs` with home `/var/lib/srv-orbs`.
Install a current Amp CLI accessible as `/usr/local/bin/amp`; for self-updates,
keep its actual installation writable by this account. Configure its SSH trust
and test `ssh srv -- --json orb list` as that account before starting the provider.

Initialize a **new** empty dispatcher directory explicitly, using the same
endpoint and account as the service:

```bash
sudo -u srv-orbs env SRV_ORB_ENDPOINT=srv SRV_ORB_STATE_DIR=/var/lib/srv-orbs/dispatch \
  /usr/local/bin/srv-orb-dispatch init
```

Never initialize over lost provider state. Restore its backup instead. Update
srv, srv-front (when used), srv-vm-runner, srv-net-helper and the prepared guest
image together before enabling this protocol. Older components fail closed;
there is no fallback to ordinary `delete` or unbound bootstrap. Preexisting Orb
records without a backend bootstrap counter require deliberate retirement and
recreation, not a counter reset or automatic adoption. JSON inspection remains
available for diagnostics, omitting `bootstrap_generation` when its row is
missing; this does not initialize a counter or permit managed bootstrap.

Create a provider in **Amp workspace Settings → Orbs → Add Orb Provider**.
Store its token in `/etc/srv-orbs.env`, owned by root with mode `0600`. Copy the
non-secret settings from
[`srv-orbs.env.example`](../../contrib/amp/orbs/srv-orbs.env.example) and add
`AMP_ORB_PROVIDER_TOKEN` privately. Do not put tokens in shell arguments, Git,
images or thread messages.

```bash
sudo install -m 0644 contrib/amp/orbs/srv-orbs.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now srv-orbs
sudo journalctl -u srv-orbs -f
```

The service runs:

```bash
amp orb provider /usr/local/bin/srv-orb-dispatch
```

Select this provider in Amp's thread composer, or use:

```bash
amp -ox "Inspect this repository" --orb-provider YOUR_PROVIDER_NAME
```

### Configuration

| Environment variable | Default / meaning |
| --- | --- |
| `SRV_ORB_ENDPOINT` | `srv`; an OpenSSH destination/alias for a backend or srv-front. |
| `SRV_ORB_STATE_DIR` | Required absolute persistent directory. Example service: `/var/lib/srv-orbs/dispatch`. |
| `SRV_ORB_IMAGE` | `amp-orb`; prepared srv image for new VMs. |
| `ORB_IMAGE` | Set to the same prepared image name in the provider environment. |
| `SRV_ORB_HOST` | Optional srv-front host name; omit for automatic placement. Do not set for direct backend access. |
| `SRV_ORB_CPUS` | `2`; new VM vCPUs. |
| `SRV_ORB_RAM` | `4G`; new VM memory. |
| `SRV_ORB_DISK` | `20G`; new VM rootfs size. |

Sizes are operator configuration, not dispatch fields supplied by Amp. Changing
these settings affects new VMs only. Existing VMs keep their disks and placement.
`AMP_URL`, when provided by Amp on spawn, is passed to bootstrap.

Only one provider installation may own an Orb ID. Do not run independent
providers with separate state directories against the same orbs. The dispatcher
serializes same-orb jobs with local file locks; it is not an HA controller.
Different orbs can run concurrently. Jobs time out after 270 seconds, including
lock waits, leaving time inside Amp's five-minute dispatch limit.

## Recovery and security

The state directory contains an identity/host mapping, operation phase and deletion
record. Bindings include the backend's own SSH host-key fingerprint, VM UUID,
creation timestamp, workload kind and image, not just a frontend placement label.
Back it up and keep it private. It never stores
the provider or orb tokens. The bootstrap token travels over control SSH stdin
and the host-only vsock channel,
briefly enters a root-only environment file under `/run`, and is removed after
systemd starts bootstrap. It remains in the bootstrap process environment;
root inside the VM is trusted. Guest bootstrap output is not relayed to provider
logs because it could contain credentials.

The srv daemon and dispatcher disable process dumpability. For the daemon this
also restricts ptrace and changes `/proc` ownership/access; debugging must account
for those effects. Both token-bearing
systemd units set `LimitCORE=0`. The example provider unit additionally refuses a
piped host `core_pattern`: not all external collectors honor the process limit.
This intentionally excludes a stock systemd-coredump configuration until the
operator supplies a reviewed no-dump policy; the integration does not change
host-wide core settings. Guest root and its configured collector are trusted;
the guest unit's limit is not a guarantee against a privileged piped collector.
The srv-front forwarding process, SSH client and Amp CLI remain part of the
trusted transport/runtime boundary. Avoid diagnostic dumps of those processes.

The adapter records `creating` **before** sending `orb new`. If the SSH response is
lost, a later job refuses automatic adoption or replay. It never automatically
retries a missing, uncertain create. Initial discovery through srv-front requires
every host to answer; after placement is recorded, only the selected host is
queried, so an unrelated backend outage does not prevent wake-up.

An explicit `srv-orb-dispatch bind ORB_ID UUID`, under the provider's existing
configuration, can bind a visible native Orb on the **already recorded** host
with no import/migration history and bootstrap generation zero. A lost reply to
automatic frontend placement has no recorded host and cannot use this escape
hatch. Use a pinned host when deterministic create reconciliation is required.
Missing dispatch records, counters or lock-associated records fail closed.
Do not delete or hand-edit JSON/lock files to release a fence. A VM found without
a record is not silently adopted: restore the state backup instead. An unknown
allocation that cannot satisfy these checks remains unavailable pending a
separate reconciliation design.

### Discard compute and continue the same thread

An operator can permanently discard an old Orb's compute and workspace without
requesting deletion of its Amp thread. Use the normal srv SSH API as the instance
owner or an administrator. Here `srv-a` is the direct backend endpoint:

```bash
ssh srv-a orb list
ssh srv-a -- --json inspect ORB_NAME
ssh srv-a delete ORB_NAME
```

Confirm that the name, backend and instance identity match the intended Orb.
Through srv-front, add `--host HOST` to select its backend. For delayed automation
or retries that must survive name reuse, use
[`delete NAME --expect …`](../reference/ssh-commands.md#bound-deletion-and-retries)
with the saved identity and request ID instead of bare deletion.

**Deletion destroys the Orb's disk, including uncommitted work.** Save any
required files first. Unlike `pause`, it does not preserve the workspace. This
SSH command makes no Amp API call: conversation history and attachments
already stored by Amp remain subject to Amp's own access and retention rules.
They are not a backup of the whole filesystem. Amp's UI may not immediately
reflect provider-side deletion. The next Amp `spawn` creates fresh compute for
the same thread; no separate authorization command is needed.

Manual SSH deletion leaves the dispatch record untouched. On the next Amp job,
the dispatcher queries the recorded backend for `cleanup-receipt --instance UUID`
when its managed VM is absent. Only the complete, matching receipt permits it to
record deletion and release pending work. The query uses the original instance
UUID, not a reused name; the original owner and administrators can read it even
when a different administrator performed deletion. Amp `delete` still records its
own deletion request before submission. Both paths save the authoritative receipt
before publishing the deleted state.
Individual backups, project templates and shared host snapshots remain outside
deletion scope.

Failed preflight is rechecked on retry; incomplete SSH deletion resumes under its
existing fence through srv background reconciliation or another `delete`.
For a lost reply, query `cleanup-receipt --instance UUID` or repeat the saved bound
request. An absent VM alone never reports success. The dispatcher creates nothing
until cleanup succeeds and its receipt is saved locally; lookup or persistence
failures can be retried. A pending Amp deletion can also be completed by a later
`delete` or `spawn`. Repeated Amp `delete` and `pause` after completed deletion are
no-ops until another spawn. A saved bound deletion for the old instance never
deletes the successor.

To continue the **same retained thread**, send a new message in Amp. Its `spawn`
supplies the fresh token. Following Amp's
[dispatch contract](https://ampcode.com/docs/orbs/self-hosted#dispatch-contract),
the dispatcher resumes an existing VM or creates a new one after confirmed
deletion. This applies after both manual SSH deletion and ordinary Amp deletion.

The dispatcher validates the completed deletion's full v3, v4 or v5 receipt,
then durably publishes one successor with a distinct srv name before attempting
creation. The new VM uses normal base-image or ready project snapshot selection,
gets a new instance UUID, and bootstraps with the new token. Amp runs repository
setup again when needed; prepared snapshots may skip setup. This does **not**
restore the discarded thread workspace's uncommitted changes. Subsequent
pause/wake retains the successor's own disk and follows normal resume behavior.

**Job-ordering limitation:** Amp's documented dispatch contract has no upstream
incarnation or sequence field. A local job that observed the old incarnation
before waiting for its lock is rejected after the switch; old identity-bound srv
commands cannot target the new name/UUID. But an old upstream job first delivered
after the switch is indistinguishable from a current job. Incoming Amp jobs are
treated as current instructions, not subjected to a locally unverifiable queue
drain requirement. This does not establish an upstream ordering guarantee or
protect against arbitrary upstream reordering.

The stable root record and lock select the current incarnation. Cleanup receipts
remain scoped to each incarnation's srv name and are not overwritten by its
successor. No predecessor archives are created or traversed; existing archives
are left untouched and are not required to operate a bound successor. A crash
before publication leaves the completed deletion current. A crash after
publication but before creation resumes the same successor. Unknown create
outcomes retain their existing fences and require reconciliation, not another
allocation. An unreachable backend is never treated as proof of absence.

A successor can consume a ready project template but cannot capture into its
predecessor's snapshot ID. New mappings bind snapshot authorship to the instance
UUID. Legacy mappings may only be completed by the original, non-replacement
incarnation; a successor must use a new snapshot ID for new content.

Do not remove provider records, manually remove disk files, or reuse the managed
instance name. File removal does not produce a cleanup receipt. There is no
automatic expiry, bulk deletion or recreation of unexpectedly missing managed
disks without a matching receipt.

**Upgrade:** the `retire` and `fresh-start` commands have been removed, without
aliases. External deletion reconciliation requires the updated srv backend's
`cleanup-receipt --instance UUID` query. Legacy
`retiring` records resume as pending deletion, and `retired` or `deleted` records
allow a later spawn only with a valid bound cleanup receipt. A legacy deletion
before any allocation needs no receipt. Previously published successors continue
without their predecessor archives. In particular, `deleted` no longer means a
permanent ban on future compute for that Orb ID. Older dispatcher versions retain
the old behavior; do not mix versions against the same provider state. See the
coordinated deletion-protocol upgrade below.

### Operation receipts and bootstrap generations

The dispatcher persists a fresh request ID before start, stop, bootstrap or
delete. Bound start/stop accept `--expect REQUEST UUID CREATED_AT BACKEND_ID IMAGE`;
unbound human start/stop commands retain their previous behavior. srv checks the
binding and admission under the instance lock. Repeating an admitted start/stop
request never reexecutes it. `operation-result REQUEST` only reads its stored result.
After a lost outer SSH reply, a matching `done` result is sufficient to recover.

`srv-orb-dispatch cancel ORB_ID REQUEST` attempts cancellation of a pending
**start or stop**. srv takes the same instance lock and installs an exactly bound
cancel tombstone only if the request was never admitted. If the worker won the
race, cancel reports its actual phase and never overwrites it. No operator flag
asserts quiescence. Failed, executing or interrupted operations mean unavailable
pending later reconciliation, not safe to retry. A stopped/started observation
or srv reboot alone is insufficient: downstream runner work is not durably bound
to the ingress request at every crash boundary. Stop/start remain fenced, and
the dispatcher refuses delete while either is uncertain.

Bootstrap has a narrower recovery protocol. Before sending credentials the
dispatcher fsyncs its next generation and request ID. srv atomically consumes a
strictly increasing generation for the exact UUID/creation timestamp, together
with request admission. It holds the instance lock through the vsock exchange.
A new spawn may supersede an uncertain bootstrap with a **higher** generation;
an equal generation never executes again. Lower/missing/inconsistent provider
or backend counters fail closed, rather than catching up or resetting.
Pause remains refused while bootstrap is uncertain. Receipt-based deletion is
allowed without an intervening spawn, provided the normal cleanup proof succeeds.

The guest serializes generation check, consumption and effects with a lock in
`/run/srv-orb`. It first records `executing`, installs `KillMode=control-group`,
synchronously stops any old unit/job with systemd's replace job mode, and checks
inactive state, no queued job and an empty cgroup. A completed forced kill may
leave systemd's unit in failed state; only after checking no processes remain is
that bookkeeping cleared and inactive state checked again. Only then does it
write the new environment, start the unit, verify active, unlink the environment
file, record `done` and acknowledge the exact generation. Consuming a generation
is never reported as success. The acknowledgment certifies the old unit's
processes are gone and the replacement exec is active with new credentials;
**it does not certify Amp registration**.

The journal survives agent restarts within a guest boot and is never part of
disk templates. A guest reboot starts a new Firecracker process, discarding the
old guest's vsock/backlog and unit processes; srv's durable high-water excludes
older host-queued requests. This argument depends on that runtime boundary, not
on arbitrary state rollback. Deleting all `/run` journal state within a live
guest, restoring coherent old copies of all stores, or cloning backend host keys
is outside the supported recovery model. Preserve state and distinct host keys.
Partial journal loss refuses initialization. No reset/adoption command exists.
Old guests without the echoed generation acknowledgment are rejected.

### Authoritative native-Orb deletion

`delete NAME --expect REQUEST_ID UUID CREATED_AT BACKEND_ID IMAGE` supports both native
Orbs and ordinary VMs. Plain `delete NAME` and background recovery use the same
durable deletion engine. `cleanup-receipt REQUEST` reads the stored
result without invoking deletion. Both require an explicit `--host` through
srv-front. On a failed or lost reply, repeat the same deletion: the dispatcher
queries the receipt and, if missing, resubmits the original identity and request
ID. Never substitute a new ID or downgrade to plain `delete`.

The former `delete-proof` SSH command is removed, with no alias. Update the
dispatcher and other callers together with srv and srv-front. The durable
request format and existing receipts are unchanged, so the new syntax can resume
an already-admitted request using its original request ID and identity arguments.

New deletions publish v5 receipts with `gateways_absent: true`. After preflight
and durable cleanup admission, srv closes this instance's Amp CONNECT, provider
and integration listeners, cancels their upstream requests and upgraded streams,
and waits for admitted handlers (including pending dials) to finish. Gateway
reconciliation cannot reopen the fenced instance from stale inventory. A timeout
leaves cleanup fenced without a receipt; a preflight rejection leaves the live
gateways untouched. Neighboring instances' gateways remain available.
This proves absence of per-instance listeners and active handlers/streams, not
absence of every host socket: shared idle upstream transport pools are retained.

Upgrade srv, `srv-vm-runner`, `srv-net-helper`, its systemd unit and
`srv-orb-dispatch` together. Stop provider admission and srv first, then
stop/drain the old helpers and their children before starting the updated
services. Old unbound helper requests cannot be retrospectively fenced; do not
enable recovery while they can still run. The unit provisions
`/var/lib/srv-net-helper` for durable network deletion
records. Preserve it alongside the backend state; do not clear it or downgrade
to a helper that ignores incarnation fences. Manual installations must provision
this directory root-owned with mode 0700. The existing root-owned payload lock
must also be installed for storage deletion.

The dispatcher accepts v3, v4 and v5 receipts. Stored v2 receipts remain readable,
unchanged; they do **not** prove gateway quiescence and cannot finalize a
`deleting` dispatcher record under the new version. Already-finalized `deleted`
records backed by v2 receipts still make repeat delete a no-op but reject spawn;
that no-op does not upgrade the historical receipt. Do not rewrite receipts or
tombstones to bypass this distinction.
The existing coordinated runner/nethelper/guest-image protocol requirements still
apply; this change does not deploy or validate any installed host binary.

The backend fences ingress, drains gateways, stops the runtime, and removes cold
storage directly, without hydration. A remote-storage error leaves the instance
in `deleting` for retry. The runner durably fences the deleted incarnation before
proving runtime/jailer/cgroup absence through `cleanup-proof-v2`.
The exact recorded start receipt must be closed, and the runner
scans the entire name's receipt journal for corrupt identities or nonclosed
attempts. It checks for no live VM with that name, no jailer workspace or runtime
socket, and no delegated cgroup. This is name-scoped proof, not a separate
UUID-bound enumeration of every historical runtime path. A never-started instance
may have no start attempt: its exact durable deletion fence and vacant runtime
are still required. A missing expected attempt is not treated as absence.
Nethelper binds network
finalization to the incarnation, request and TAP/CIDR, fences late setup/cleanup,
and durably records completion after checking absence. A completed retry does no
more destructive work, even if another instance has since reused the network.
Mutating helper children retain the network lock until exit, including after a
helper crash. Missing/older helper endpoints and unreadable resources fail closed.

For both kinds, srv removes `instances/<name>` and fsyncs its parent, then atomically
commits the receipt, deleted tombstone and release of
the cleanup fence. Failed cleanup remains charged by the existing srv capacity
accounting. The same request can resume after timeout, service restart or partial
removal, including a missing disk or partially removed instance directory. Missing
owned trees are already complete; filesystem errors are not absence. Btrfs
children retain the existing payload lock so a retry cannot release the allocation
ahead of old filesystem work. Known preflight failures and startup-cancelled
admissions are rechecked; executing/interrupted cleanup requires its original
fence. Another request cannot take it over. Background reconciliation resumes
that same owning request. There is no added scheduler or independent slot counter.
Deletion retains all individual backups and v5 records `backups_absent: false`.
Ordinary VM deletion removes only the exact recorded Tailscale node, retaining the shared identity
for a completed migration source or an incomplete destination import.

`never-minted` is derived only for native Orbs with no enrollment-key row and no
node ID. It says nothing about project-managed Tailscale or external copies.
Project templates and shared host snapshots are neither scanned nor removed,
including copies of the Orb's disk/backups. v4 and v5 set `snapshots_absent: false`
and `retained_templates: null`: neither makes a copy-erasure or exhaustive template
inventory claim. Cold-storage cleanup must confirm remote absence; missing local
files alone do not establish it.
Receipt/request records survive name reuse. An uncertain bootstrap
can be deleted through this path: the runtime proof closes the guest work and
the backend tombstone prevents late host admission. A persisted start that never
reached the runner is resolved into a closed exact-attempt receipt, fencing its
late arrival. An outcome that remains unknown still blocks deletion.

Do not manually migrate, recreate or reuse managed orb names. A missing
managed disk without a committed cleanup receipt causes spawn to fail rather
than creating an empty replacement.
Migration state is rejected for operator recovery. After receipt-backed deletion,
a later Amp spawn can create a distinct successor for the same Orb ID. This does
not solve upstream reordering of jobs: the documented contract has no sequence
number.

For bootstrap errors, inspect the VM and run, through a trusted guest session:

```bash
systemctl status srv-orb-bootstrap.service
journalctl -u srv-orb-bootstrap.service
systemctl status srv-orb-guest.service
```

Treat journal contents as sensitive. No service is enabled at guest boot with an
old token: the next spawn supplies fresh credentials and restarts only bootstrap.

## Validate on a real host

Before relying on this provider, create a disposable Amp thread and verify:

1. The guest boots and registers with Amp without Tailscale or guest SSH;
   setup runs successfully. Verify separate VM/Orb lists and common capacity accounting.
2. Write a file and install a dependency; pause and wake the thread. Both survive,
   while terminal processes do not. Verify `.agents/resume` behavior.
3. Open an Amp Terminal, Portal and Desktop; launch a graphical application and
   verify reconnect/service startup after wake (the old desktop session is lost).
4. With srv-front, take an unrelated backend out of service and wake the orb on
   its recorded healthy host.
5. Let Amp save a project snapshot; create a second thread from it. Verify prepared
   dependencies survive, setup is skipped, and machine ID and bootstrap credentials
   differ. If the project enables Tailscale, verify a fresh node identity too.
   Verify the original still wakes.
6. Delete the source thread, then create another from its snapshot. Verify the
   template survives while the source VM/disk is removed.
7. Under the configured service identity, verify foreign users' VMs are invisible
   and admin commands are rejected, both directly and through srv-front.
8. On a disposable retained thread, use SSH `delete NAME`, then send a new message.
   Verify the thread/history remain readable, the replacement has a different
   name/UUID and fresh bootstrap credentials, old
   disk-only files are absent, and repository setup or snapshot activation runs.
   Change a replacement file, pause/wake and confirm it survives. Repeat the cycle
   using bound deletion; replay its saved request after replacement and confirm
   it returns the old receipt without resetting the workspace. Then delete the
   thread and verify cleanup with all prior receipts retained.

Local automated checks:

```bash
go test ./cmd/srv-orb-dispatch ./internal/store ./internal/service ./internal/provision ./internal/front ./internal/vmrunner ./internal/nethelper ./internal/orbguest
python3 -m unittest discover -s contrib/amp/orbs -p 'test_*.py'
# Disposable root/systemd environment only; unique temporary unit, synthetic token:
sudo python3 contrib/amp/orbs/probe_guest_systemd.py
```

The Go dispatch tests use a subprocess SSH stand-in, not Firecracker or Amp.
The root probe exercises the real guest handler, systemd jobs, token-holding
descendants and environment lifecycle over a socketpair, not real vsock. Neither
proves Amp/Tunnel interoperability or the guest-reset runtime boundary.
Provision tests additionally check real temporary
ext4 images, identity cleanup, retained settings/skills, immutable templates and
cancellation. Actual Amp setup-skipping still requires the live cycle above;
its internal completion-marker format is not a documented provider contract.
