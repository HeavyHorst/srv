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
- Permission errors — verify `srv-vm-runner.service` keeps `User=root`, `Group=srv`, `Delegate=cpu memory pids`, and no `NoNewPrivileges=yes`
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