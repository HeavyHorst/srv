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
| `delete` | Delete the VM and disk; retain a small local deletion record. |

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
Common `inspect`, `start`, `stop`, `delete`, backup and capacity commands still
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
record. Back it up and keep it private. It never stores
the provider or orb tokens. The bootstrap token travels over control SSH stdin
and the host-only vsock channel,
briefly enters a root-only environment file under `/run`, and is removed after
systemd starts bootstrap. It remains in the bootstrap process environment;
root inside the VM is trusted. Guest bootstrap output is not relayed to provider
logs because it could contain credentials.

The adapter records `creating` **before** sending `orb new`. If the SSH response is
lost, a later job searches for the VM and records its host. It never automatically
retries a missing, uncertain create. Initial discovery through srv-front requires
every host to answer; after placement is recorded, only the selected host is
queried, so an unrelated backend outage does not prevent wake-up.

If an uncertain create remains absent, stop the provider, inspect every eligible
backend and establish that no create is still running or can complete late.
Only after that verification may an operator remove that orb's `creating` JSON
record and retry. Do not remove lock files while dispatch jobs may still run.
Keep the record if the outcome is unknown. A VM found without a record is not
silently adopted: restore the state backup instead.

Do not manually migrate, delete/recreate or reuse managed orb names. A missing
managed disk causes spawn to fail rather than creating an empty replacement.
Migration state is rejected for operator recovery. Completed deletion records
prevent a late spawn from resurrecting a deleted orb. This does not solve upstream
reordering of pause/spawn jobs: the documented contract has no sequence number.

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

Local automated checks: `go test ./cmd/srv-orb-dispatch`. These use a subprocess
SSH stand-in, not Firecracker or the Amp service, and do not prove guest bootstrap
or Tunnel interoperability. Provision tests additionally check real temporary
ext4 images, identity cleanup, retained settings/skills, immutable templates and
cancellation. Actual Amp setup-skipping still requires the live cycle above;
its internal completion-marker format is not a documented provider contract.
