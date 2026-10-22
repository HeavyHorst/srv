# Multi-host frontend

`srv-front` provides one SSH endpoint in front of independent `srv` hosts. It
keeps only a static host configuration and its own Tailscale/SSH identity, not a
VM registry. It can run on a small separate Linux server without KVM,
Firecracker, a reflink filesystem, or the srv helper services.

Each backend still owns VM state, permissions, admission, images, pools, and
backups. An unavailable frontend does not stop VMs; direct backend SSH remains
available. There is no HA failover, replication, or automatic rebalancing.

## Configure trust and start the frontend

Upgrade the backends to a version supporting forwarding, placement, and
migration before adding them. Give each backend a distinct `SRV_HOSTNAME`
(for example `srv-a`, `srv-b`); the frontend can take the friendly name `srv`.

Build and install the separate binary:

```bash
go build -o srv-front ./cmd/srv-front
sudo install -m 0755 srv-front /usr/local/bin/srv-front
sudo install -m 0644 contrib/systemd/srv-front.service /etc/systemd/system/
```

Create `/etc/srv-front.json` with explicit allowed user logins and pinned backend
host keys. Addresses must include the SSH port:

```json
{
  "hostname": "srv",
  "data_dir": "/var/lib/srv-front",
  "listen_addr": ":22",
  "allowed_users": ["alice@example.com", "ops@example.com"],
  "hosts": [
    {"name": "a", "address": "srv-a:22", "host_key": "ssh-ed25519 REPLACE_WITH_HOST_A_PUBLIC_KEY"},
    {"name": "b", "address": "srv-b:22", "host_key": "ssh-ed25519 REPLACE_WITH_HOST_B_PUBLIC_KEY"}
  ]
}
```

Obtain each public host key through a trusted administrative connection, not
an unauthenticated first-use network scan. Each upgraded backend logs its public
key at startup. Read the `public_key` value from its service journal:

```bash
sudo journalctl -u srv -b --no-pager | grep 'ssh host key'
```

Copy the `ssh-ed25519 ...` value without the surrounding log-field quotes.
Only the **public key**, never `/var/lib/srv/state/host_key`, goes into the frontend
configuration. Changing a backend key requires updating its pin.

Put the frontend's node-join credentials in `/etc/srv-front.env` (root-readable,
mode 0600). `TS_AUTHKEY`, `TS_CLIENT_ID`, `TS_CLIENT_SECRET`, `TS_CONTROL_URL`, and
`SRV_ADVERTISE_TAGS` are supported. Use a dedicated frontend node identity; it
does not need credentials for creating guest nodes. Restrict tailnet access so
the frontend can reach the configured backend SSH ports and intended users can
reach the frontend.

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now srv-front
sudo journalctl -u srv-front
```

The frontend logs its stable Tailscale node ID at startup. Configure that exact
ID on **each backend** in `/etc/srv/srv.env`:

```bash
SRV_TRUSTED_FRONTENDS=nREPLACE_WITH_FRONTEND_STABLE_NODE_ID
```

Restart each backend after updating its configuration. This trust is powerful:
the frontend can assert user identities, including administrators. Protect its
state directory and credentials accordingly. Re-enrolling it as a new node
requires updating the backend trust entries.

The frontend resolves the original user with Tailscale `WhoIs`; tagged client
nodes cannot impersonate human users. Backends accept forwarding only from the
configured stable node IDs, then apply their usual `SRV_ALLOWED_USERS`,
`SRV_ADMIN_USERS`, and per-instance ownership checks. Audit records retain the
user identity, actual frontend peer address, and forwarding node ID. No SSH
username grants delegation rights. A trusted frontend itself may be tagged.

## Commands and placement

```bash
ssh srv node list
ssh srv -- --json node list
ssh srv list
ssh srv status
ssh srv -- --json list
ssh srv new demo --ram 8G --rootfs-size 20G
ssh srv new other --host b
ssh srv inspect demo
ssh srv stop a/demo
ssh srv logs -f a/demo serial
ssh srv pool list --host a
```

`node list` shows one row per configured server: running/total VMs, allocated
vCPUs versus CPU budget, one-minute load average, RAM and disk reservations versus
their budgets, and actual available filesystem space. These are allocations and
capacity, not instantaneous CPU/RAM utilization percentages. Unreachable or
unauthorized hosts remain visible with an error rather than disappearing. Use
`node list --host a` to query just one host; `status` gives more detail.

`node list`, `list`, and `status` query hosts concurrently and label their results.
`node list` and `status` require backend admin access. With `--json`, aggregate responses contain
a `hosts` array with `host`, `data`, and optional `error`. Partial failures retain
successful results, print errors, and exit nonzero. Host-local operations such
as pools, integrations, snapshots, and raw imports require `--host`.

Short VM names resolve only when all hosts answer and exactly one active source
matches. Use `host/name` or `--host` when ambiguous or when another host is down.
Completed migration sources remain visible but are excluded from short-name
resolution. Keep guest names unique within the tailnet; the frontend does not
reserve names globally or rewrite guest DNS names. Interactive `top`/PTY sessions
must go directly to a backend.

Automatic `new` placement asks each backend whether the requested image, shape,
pool, and integrations fit. It ranks eligible hosts by remaining RAM budget,
then remaining disk space, then host name. RAM includes VM overhead; disk checks
both logical reservations and available filesystem space with the host reserve.
CPU remains advisory, consistent with srv's existing overcommit policy.

Preflight does not reserve capacity. The selected host still performs normal
admission. A failed create is **not retried on another host**: after a network
failure the original create may already have succeeded. Inspect that host before
retrying. `--host` bypasses automatic selection, not backend permissions or
admission checks.

## Cold migration

```bash
ssh srv migrate a/demo --to b
```

The frontend checks the destination before stopping the source, records a durable
source start fence, streams `export` into `import` through bounded pipes, confirms
the transfer, marks the source completed, and starts the destination only if the
source was previously running. An already stopped VM stays stopped. `migrate`
currently reports text and progress, and rejects `--json` before doing any work. The
frontend needs no disk-sized buffer, but all transfer traffic crosses its network
connection. This is cold migration: downtime includes transfer and startup.

For pooled VMs, explicitly select an existing destination pool or convert to fixed
memory. A fixed-memory VM can also be assigned to a destination pool:

```bash
ssh srv migrate a/demo --to b --pool shared
ssh srv migrate a/demo --to b --fixed-memory
```

Pools are host-local memory reservations and shared limits, not shared disks.
Pool IDs are never copied and pools are never created automatically. The target
pool must support this VM's guest-visible RAM; it is charged once at host level,
not again for every member. This does not guarantee spare memory under concurrent
pool usage. The source pool stays allocated until an operator removes it.
Without an explicit policy, pooled migration is rejected before stopping the VM.
Raw imports of pooled exports likewise require `import --pool NAME` or
`import --fixed-memory`. Upgrade both backends: pooled exports use a format that
older importers reject instead of silently changing the memory policy.

VMs with enabled integrations remain blocked: credentials, gateway routes, and
bindings are host-local and are not part of the artifact. An administrator must
configure the intended integration on the destination, disable every source
binding, migrate, and explicitly enable the destination bindings:

```bash
ssh srv integration disable demo openai --host a
ssh srv migrate a/demo --to b
ssh srv integration enable demo openai --host b
```

Expect integration-dependent workloads to be unavailable during this sequence.
The destination must have a compatible kernel/initrd under the same image name;
its rootfs template is not used for migration. Matching names do not prove kernel
compatibility; operators must deploy compatible image versions. Per-VM backups
are not copied.

The source is retained and fenced, not automatically deleted. After verifying
the destination, explicitly delete the completed source:

```bash
ssh srv delete a/demo
```

Deleting a completed source removes its local copy without revoking the shared
guest Tailscale identity. Never start both copies: the rootfs contains the same
durable guest identity.

### Interrupted migrations

There is no automatic rollback or automatic resumption. Keep the reported source,
target, and migration token. Inspect both hosts directly:

```bash
ssh srv-a -- --json migration status demo
ssh srv-b -- --json migration receipt demo
ssh srv-b inspect demo
```

An unknown target outcome must leave the source fenced. A successful import is
recorded durably on the destination. Only after verifying the destination receipt
belongs to the intended transfer may an operator complete the source and start
the destination. A completed source cannot be aborted.

| Source phase | Destination observation | Safe next action |
| --- | --- | --- |
| No migration record | No transfer started | Inspect both hosts before issuing a fresh migration. |
| `preparing` | Stop not yet confirmed | Keep source fenced. Resolve its stop failure, then repeat `migration prepare NAME TARGET TOKEN` directly with the same token and memory-policy flags. Do not export yet. |
| `prepared` | Missing or `importing` receipt | Keep source fenced. Inspect/discard partial destination data before manually repeating export/import with the selected memory policy, or follow the abort procedure below. |
| `prepared` | `imported`, with matching token and source instance ID | Complete the source with `migration complete NAME TOKEN`; then start the destination if wanted. |
| `completed` | Matching committed import | Never abort or restart the source. Retry destination start if necessary; delete the source only after verification. |
| Any active phase | Unreachable host or mismatched receipt | Stop recovery actions until the outcome is established. Neither a timeout nor a same-name VM is proof of this transfer. |

Recovery mutations go directly to the backends; `srv-front` intentionally has no
automatic recovery loop. `migration status` records the selected memory policy.
When manually resuming a transfer, pass that same policy to `import`. After abort,
any fresh migration needs a new token.

If abandoning an incomplete migration, an administrator must first establish
that **all destination copies of the guest identity are destroyed**, including
saved transfer artifacts that might be booted. Then the explicit recovery command
on the source removes the fence without starting the VM:

```bash
ssh srv-a migration abort demo TOKEN --confirm-destination-destroyed
```

This confirmation is an operator assertion, not a distributed proof. Do not use
it merely because the destination is unreachable. Preserve the source until the
outcome is understood; do not manually edit migration database rows.
