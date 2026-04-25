# View logs

srv provides two log sources for each VM: serial output and Firecracker VMM logs.

## Serial log

Shows kernel boot, `srv-bootstrap.service`, `tailscaled`, and general guest console output:

```bash
ssh srv logs demo
ssh srv logs demo serial
ssh srv logs -f demo serial
```

## Firecracker log

Shows VMM lifecycle events, API requests, and microVM-level errors:

```bash
ssh srv logs demo firecracker
ssh srv logs -f demo firecracker
```

## Both logs at once (default)

Without a log source argument, `ssh srv logs <name>` shows the serial log by default.

## Where logs live on the host

| Log | Path |
|-----|------|
| Serial | `SRV_DATA_DIR/instances/<name>/serial.log` |
| Firecracker | `SRV_DATA_DIR/instances/<name>/firecracker.log` |

The serial log is append-only across boots. The Firecracker log is reset when the VMM starts so it only contains the current Firecracker process's output.

## Systemd logs

The VM runner and network helper also log to journald:

```bash
sudo journalctl -u srv-vm-runner -f
sudo journalctl -u srv-net-helper -f
sudo journalctl -u srv -f
```

## Debugging a failed VM

When a VM is stuck or has failed:

1. `ssh srv inspect <name>` — control-plane view, state, and recorded events
2. `ssh srv logs <name> serial` — guest boot and bootstrap errors
3. `ssh srv logs <name> firecracker` — VMM errors
4. `journalctl -u srv-vm-runner --no-pager` — jailer and stop-time cleanup failures
5. Check cgroup limits: `cat /sys/fs/cgroup/firecracker-vms/<name>/memory.max`
