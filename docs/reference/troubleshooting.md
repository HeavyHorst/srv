# Troubleshooting

## VM is stuck in provisioning or failed

Check the control-plane view first:

```bash
ssh srv inspect <name>
```

Look for the `last_error` field and `state`. Then check the guest and VMM logs:

```bash
ssh srv logs <name> serial
ssh srv logs <name> firecracker
```

Common causes:

- Missing or misconfigured base rootfs/kernel
- Tailscale OAuth credentials expired or invalid
- Network helper not running (`systemctl status srv-net-helper`)
- VM runner not running (`systemctl status srv-vm-runner`)
- `/dev/kvm` not accessible

## Running VM remains awaiting-tailnet after a start timeout

A disconnected caller or readiness timeout can leave a running VM in
`awaiting-tailnet`. Once the VM has booted, srv preserves it rather than stopping
it because the caller disconnected. The background reconciler retries readiness
automatically. Check `last_error` and the serial log; for an immediate retry:

```bash
ssh srv start <name>
```

For an `awaiting-tailnet` VM whose recorded process is still running, this checks
the existing runtime and tailnet peer without rebooting or re-enrolling it. srv
matches the API socket to the recorded process, checks the enrolled node against
the local tailnet peer map, and sends a bounded TSMP probe. An unchanged
Tailscale `LastSeen`, an idle peer with `InEngine=false`, or a zero reported RTT
does not prevent success. Missing or ambiguous identity and failed probes leave
the running VM pending. If the process has exited, `start` retains its normal
behavior of starting the VM.

Warm-start `ready` means the guest responded on the tailnet. It does not prove
SSH access or application health; test those separately. Allow the SSH caller
more time than `SRV_GUEST_READY_TIMEOUT` (default `2m`), plus transport overhead.
`list` and `inspect` can wait behind an in-progress lifecycle operation because
their runtime cleanup uses the same instance lock.

After startup recovery, a background sweep runs every 30 seconds. Busy instances
are skipped until a later sweep; snapshots exclude background mutations. Readiness
and outcome lookup get a 10-second attempt budget per instance. Shutdown and
network cleanup retain their own bounded completion deadlines. Slow sweeps can
delay later checks; 30 seconds is not a recovery-time guarantee.

The sweep resumes `stopping` and `deleting` intent and checks pending readiness.
It does not start stopped guests, restart crashed guests, or monitor application
health. Startup recovery remains the separate path for reboot-time auto-start.

Do not repair this state with raw SQL or restart shared services just to refresh
it. Deployments and rollback still require the maintenance and backup procedure
in the operations reference.

## Stop was interrupted

`stopping` records an unfinished stop. The PID and resource reservations remain
until the VM runner and network cleanup succeed. The background sweep retries
the stop; `ssh srv stop <name>` also retries immediately. Likewise, `deleting`
resumes an already requested deletion after failures. Backup/restore refuses
instances with unfinished shutdown cleanup;
neither `start` nor startup recovery may override this state, even if the process
has already exited.

Before downgrading to a version without `stopping`, finish all pending stops
under the new version. Older versions can treat this unknown state as eligible
for automatic startup. Do not run old and new control-plane writers together.
This is an additional rollback constraint, not a replacement for the documented
maintenance and backup procedure.

## Launch outcome is unknown

Each new launch has a durable request ID in SQLite and a runner receipt under
`SRV_JAILER_BASE_DIR/.start-receipts`. After a lost HTTP response, srv asks the
runner about that same attempt. It adopts a verified running VM, or cleans up
only after the runner confirms absence. Unknown outcomes preserve disks,
networking, and reservations; another `start` cannot launch a duplicate.

If the helper dies between accepting a launch and recording its process identity,
the attempt remains quarantined for that host boot. Restoring helper connectivity
can recover committed launches, but does not make an unverified launch safe.
Inspect the runner logs and runtime before planning recovery. Do not clear the
receipt, edit the PID in SQLite, or restart the host without a maintenance plan.
A host reboot makes old-boot attempts recoverable only after the runner verifies
that no conflicting runtime occupies the name.

Cold guests need a serial `srv-enrollment` report from the updated base image.
Missing reports, wrong nonces, and mismatched node IDs leave them pending even if
a same-name device responds. Warm starts require a stored node ID or a unique
existing API device; a missing legacy device must be investigated before retrying.
Deleting a legacy VM without a stored node ID leaves its Tailscale device for
manual identification and cleanup rather than deleting a potentially unrelated
same-name device.

## VM boots but Tailscale SSH doesn't work

If `ssh srv inspect <name>` shows `state: ready` and a Tailscale IP but you can't SSH in:

1. Check the serial log for bootstrap errors: `ssh srv logs <name> serial`
2. Look for `srv-bootstrap.service` failures
3. Verify that `SRV_GUEST_AUTH_TAGS` matches a tag your OAuth client can mint keys for
4. Verify that the guest can reach the Tailscale coordination server (check DNS and outbound connectivity from inside the VM if possible)

## Guest can't reach the internet

Guest egress depends on IPv4 forwarding and MASQUERADE rules:

```bash
# Check forwarding
sysctl net.ipv4.ip_forward

# Check iptables rules
sudo iptables -t nat -L -n
sudo iptables -L FORWARD -n

# Check the network helper
sudo systemctl status srv-net-helper
```

If `net.ipv4.ip_forward` is 0, re-enable it:

```bash
sudo sysctl -w net.ipv4.ip_forward=1
```

## VM runs out of disk space

Check from inside the VM:

```bash
df -h
```

If the rootfs is full, you can resize the VM:

```bash
ssh srv stop <name>
ssh srv resize <name> --rootfs-size 20G
ssh srv start <name>
```

On the host, check `SRV_DATA_DIR` usage:

```bash
df -h /var/lib/srv
```

## Check cgroup limits

If a VM is being throttled or OOM-killed, check its cgroup v2 limits:

```bash
cat /sys/fs/cgroup/firecracker-vms/<name>/cpu.max
cat /sys/fs/cgroup/firecracker-vms/<name>/memory.max
cat /sys/fs/cgroup/firecracker-vms/<name>/memory.swap.max
cat /sys/fs/cgroup/firecracker-vms/<name>/pids.max
```

## Firecracker or jailer errors

The VM runner logs to journald:

```bash
sudo journalctl -u srv-vm-runner -f
```

Common issues:

- Jailer chroot setup failures — check that `SRV_JAILER_BASE_DIR` is on the same filesystem as `SRV_DATA_DIR`
- Permission errors — verify `srv-vm-runner.service` keeps `User=root`, `Group=srv`, `Delegate=cpu memory pids`, `DelegateSubgroup=supervisor`, and no `NoNewPrivileges=yes`
- `/dev/kvm` not available — check permissions and that KVM is loaded

## Smoke test

The end-to-end smoke test validates the full host setup:

```bash
sudo ./contrib/smoke/host-smoke.sh
```

Overrides:

| Variable | Default | Description |
|----------|---------|-------------|
| `ENV_PATH` | `/etc/srv/srv.env` | Alternate environment file |
| `SMOKE_SSH_HOST` | `srv` | Alternate control-plane hostname |
| `INSTANCE_NAME` | `smoke-<random>` | Force a predictable instance name |
| `ARTIFACT_ROOT` | `/var/tmp/srv-smoke` | Artifact storage root |
| `KEEP_FAILED` | (unset) | Leave a failed instance intact for debugging |
| `READY_TIMEOUT_SECONDS` | derived from config | Override guest-ready timeout |
| `GUEST_SSH_READY_TIMEOUT` | `45` | Seconds to wait for guest SSH after ready |

On failure, artifacts are written to `/var/tmp/srv-smoke/<instance>/` including `inspect`, `logs`, `systemctl status`, and `journalctl` output.
