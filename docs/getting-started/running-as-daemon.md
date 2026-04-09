# Running as a daemon

srv is designed to run as a set of systemd services on a prepared host. The installer sets up three units that must all be active for the control plane to function.

## Systemd units

| Unit | Purpose |
|------|---------|
| `srv.service` | The main control-plane process. Joins the tailnet via tsnet and exposes the SSH API on port 22. |
| `srv-net-helper.service` | Root-only helper that owns TAP device creation, iptables MASQUERADE, and FORWARD rules for guest NAT. |
| `srv-vm-runner.service` | Root-owned process that invokes Firecracker through the jailer, drops to `srv-vm:srv`, and manages per-VM cgroup v2 leaves. |

## Common systemd commands

```bash
# Check status
sudo systemctl status srv srv-net-helper srv-vm-runner

# View logs
sudo journalctl -u srv -f
sudo journalctl -u srv-vm-runner -f

# Restart all services
sudo systemctl restart srv-vm-runner srv-net-helper srv
```

!!! warning
    `srv-vm-runner.service` must keep `User=root`, `Group=srv`, `Delegate=cpu memory pids`, and a group-accessible socket under `/run/srv-vm-runner/`. Do not add `NoNewPrivileges=yes` — the jailer must drop privileges and `exec` Firecracker on real hosts.

## Host reboot recovery

Same-host reboot recovery is built into the control plane. When `srv` comes back under systemd, previously active instances are restarted automatically.

You do not need to manually restart VMs after a host reboot.

## Environment file

Configuration lives in `/etc/srv/srv.env`. See the [configuration reference](../reference/configuration.md) for every variable.

If `/etc/srv/srv.env` already exists before an upgrade, the installer keeps it by default. After upgrade, verify that `SRV_FIRECRACKER_BIN` and `SRV_JAILER_BIN` still point at the intended static binaries.

## Upgrading

1. Take a quiesced backup or a [host snapshot](../tasks/host-snapshots.md).
2. Build and install the new `srv`, `srv-net-helper`, and `srv-vm-runner` binaries.
3. If you also refreshed Firecracker, verify `/etc/srv/srv.env` still points at the correct binaries.
4. Restart:

    ```bash
    sudo systemctl restart srv-vm-runner srv-net-helper srv
    ```

5. Run the smoke test:

    ```bash
    sudo ./contrib/smoke/host-smoke.sh
    ```

See the [operations reference](../reference/operations.md) for full upgrade and rollback procedures.

## Smoke test as a validation gate

The smoke test is part of the supported workflow after install, restore, and upgrade — not an optional extra:

```bash
sudo ./contrib/smoke/host-smoke.sh
```

Overrides:

- `ENV_PATH=/etc/srv/srv.env.alt` — alternate environment file
- `SMOKE_SSH_HOST=srv-test` — alternate control-plane hostname
- `INSTANCE_NAME=smoke-manual` — force a predictable instance name
- `KEEP_FAILED=1` — leave a failed instance intact for debugging
- `READY_TIMEOUT_SECONDS=300` — override guest-ready timeout