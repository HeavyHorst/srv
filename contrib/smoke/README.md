# Host-Level Smoke Test

`contrib/smoke/host-smoke.sh` is the repo's repeatable end-to-end validation path for a real prepared host.

Treat it as the post-install, post-restore, and post-upgrade gate for the prepared-host path. The supported rebuild and upgrade workflows in [docs/operations.md](file:///home/rene/Code/srv/docs/operations.md) assume a clean smoke pass before the host is considered ready again.

It is intentionally not part of `go test ./...`. The harness assumes:

- a Linux host with systemd and root access
- `/dev/kvm` is available
- the configured Firecracker binary is a jailer-compatible static build, not a dynamically linked distro binary
- if `/etc/srv/srv.env` already existed before a reinstall, `SRV_FIRECRACKER_BIN` and `SRV_JAILER_BIN` were checked after install so they do not still point at older `/usr/bin` binaries
- Tailscale is installed and the host can reach the control-plane hostname
- `srv`, `srv-net-helper`, and `srv-vm-runner` are already installed and active
- `srv-vm-runner.service` keeps the repo's required privilege model: `User=root`, `Group=srv`, a group-accessible socket under `/run/srv-vm-runner/`, and no `NoNewPrivileges=yes` hardening that would block the jailer-to-Firecracker exec handoff
- `/etc/srv/srv.env` points at working guest artifacts and credentials

## What It Checks

The harness validates the host-managed deployment end to end by:

1. Verifying `/dev/kvm`, the configured base kernel/rootfs, and the required systemd units.
2. Running `ssh root@<srv> help` to confirm the SSH control surface is reachable.
3. Creating a real guest with `ssh root@<srv> new <name>`.
4. Polling `inspect <name>` until the guest reports `state: ready` plus `tailscale-name` and `tailscale-ip`, or timing out.
5. Polling for a real SSH session to the guest over the tailnet after each ready transition, so tailnet-ready and SSH-ready can converge separately on warm boots.
6. Verifying the instance appears in `list` while ready.
7. When `STRICT_HOST_ASSERTIONS=1` is set, verifying the live per-VM cgroup limit files (`cpu.max`, `memory.max`, `memory.swap.max`, and `pids.max`) under `srv-vm-runner.service` during each ready pass.
8. Stopping the guest, validating `inspect` reports `state: stopped` and `firecracker-pid: 0`, then starting it again and waiting for a second ready pass.
9. Capturing `inspect`, `logs`, `systemctl status`, `journalctl`, and `tailscale status` artifacts automatically on failure.
10. Deleting the guest, then confirming the instance disappears from `list` and its runtime directory is removed from `SRV_DATA_DIR/instances/<name>`.

## Run

```bash
sudo ./contrib/smoke/host-smoke.sh
```

For restore, rebuild, or upgrade validation, prefer the stricter mode:

```bash
STRICT_HOST_ASSERTIONS=1 sudo ./contrib/smoke/host-smoke.sh
```

Artifacts are written under `/var/tmp/srv-smoke/<instance>/` by default.

## Useful Overrides

- `ENV_PATH=/etc/srv/srv.env.alt` to point at a different environment file
- `SMOKE_SSH_HOST=srv-test` to target a different control-plane hostname
- `INSTANCE_NAME=smoke-manual` to force a predictable instance name
- `ARTIFACT_ROOT=/tmp/srv-smoke` or `ARTIFACT_DIR=/tmp/srv-smoke/run-1` to control artifact storage
- `KEEP_FAILED=1` to leave a failed instance intact for debugging
- `READY_TIMEOUT_SECONDS=300` to override the derived guest-ready timeout
- `GUEST_SSH_READY_TIMEOUT=45` to wait longer for guest SSH to become reachable after a ready transition
- `STRICT_HOST_ASSERTIONS=1` to additionally require the live per-VM cgroup limit files during ready passes, plus TAP, jailer workspace, and cgroup cleanup after stop/delete

## Failure Artifacts

On failure, the harness keeps a small artifact bundle that typically includes:

- `create.*`
- `inspect-final.*`
- `logs-serial.*`
- `logs-firecracker.*`
- `systemctl-status.*`
- `journalctl-services.*`
- `tailscale-status.*`
- `srv-list.*`
- `context.txt`

Those files are meant to be enough to debug the host without rerunning immediately.

## Debugging A Failed Run

When a host run fails, the fastest useful surfaces are:

- `ssh root@srv inspect <name>` for the control-plane view and recorded events
- `ssh root@srv logs <name> serial` for guest boot and bootstrap failures
- `ssh root@srv logs <name> firecracker` for Firecracker API and VMM failures
- `journalctl -u srv-vm-runner --no-pager` for jailer and stop-time cleanup failures

The serial and Firecracker log files are append-only. Always trust the newest lines first when comparing multiple attempts against the same instance name.
