# Walkthrough

This walkthrough assumes you have completed the [installation](install.md) and the smoke test passes.

## Create a VM

```bash
ssh srv new demo
```

<!-- srv-manual:block=output -->
```
demo created — state: provisioning
inspect:  ssh srv inspect demo
connect:  ssh root@demo
```

The control plane creates a reflink clone of the base rootfs, allocates a `/30` network, mints a one-off Tailscale auth key, injects it through MMDS, and boots the VM through the jailer.

With custom sizing:

```bash
ssh srv new demo --cpus 4 --ram 8G --rootfs-size 20G
```

## Check status

```bash
ssh srv list
```

<!-- srv-manual:block=output -->
```
NAME     STATE   CPUS  MEMORY   DISK     TAILSCALE
demo     ready   1     1.0 GiB  10.0 GiB 100.64.0.2
```

For detailed info:

```bash
ssh srv inspect demo
```

Machine-readable output for scripting:

```bash
ssh srv -- --json list
ssh srv -- --json inspect demo
```

## Connect to the VM

Once the VM reports `state: ready` and shows a Tailscale IP, connect over the tailnet:

```bash
ssh root@demo
```

Because the guest image bootstraps `tailscale up --ssh`, Tailscale SSH handles authentication without needing per-user OpenSSH keys in the guest.

You can also get the IP from inspect:

```bash
ssh srv inspect demo
# look for tailscale-ip and tailscale-name fields
```

## View logs

```bash
ssh srv logs demo
ssh srv logs demo serial
ssh srv logs demo firecracker
ssh srv logs -f demo serial
```

The serial log shows guest boot, bootstrap, and `tailscaled` output. The Firecracker log shows VMM lifecycle events.

## Stop, restart, and delete

```bash
ssh srv stop demo
ssh srv start demo
ssh srv restart demo
ssh srv delete demo
```

Stopping a VM does a graceful shutdown. The guest rootfs persists across stop/start cycles.

## Resize a VM

Resize requires the VM to be stopped. CPU and memory can be increased or decreased within limits, while rootfs can only grow:

```bash
ssh srv stop demo
ssh srv resize demo --cpus 4 --ram 8G --rootfs-size 20G
ssh srv start demo
```

See [Resize a VM](../tasks/resize-a-vm.md) for details.

## Back up and restore

```bash
ssh srv stop demo
ssh srv backup create demo
ssh srv backup list demo
ssh srv restore demo <backup-id>
```

See [Backup and restore](../tasks/backup-and-restore.md) for the full workflow.

## Move a VM between hosts

```bash
ssh srv-a export demo | ssh srv-b import
```

See [Export and import](../tasks/export-import-vm.md) for the semantics.

## Next steps

- [Instance lifecycle](../tasks/instance-lifecycle.md) — full command reference
- [Networking overview](../networking/overview.md) — how VM networking works
- [Configuration](../reference/configuration.md) — all environment variables
