# Instance lifecycle

This page covers the full lifecycle of a srv VM from creation to deletion.

## Create

```bash
ssh srv new <name>
```

With custom sizing:

```bash
ssh srv new <name> --cpus <n> --ram <size> --rootfs-size <size>
```

`--ram` and `--rootfs-size` accept units like `2G`, `512M`, or plain MiB integers. `--cpus` must be 1 or an even number, up to 32.

The control plane:

1. Clones the base rootfs as a reflink
2. Allocates a `/30` network subnet and TAP device
3. Mints a one-off Tailscale auth key via MMDS
4. Boots the VM through the jailer with cgroup v2 enforcement
5. Polls until the guest reports `state: ready`

## Inspect

```bash
ssh srv inspect <name>
```

Shows instance state, vCPU count, memory, rootfs size, network addresses, Tailscale name and IP, and event history.

Machine-readable:

```bash
ssh srv -- --json inspect <name>
```

## List and status

```bash
ssh srv list
ssh srv status
```

`list` shows the VMs visible to the caller: all VMs for admins, or only the caller's own VMs for regular users. `status` is admin-only and reports host capacity — instance counts plus CPU, memory, and disk headroom.

## Logs

```bash
ssh srv logs <name>
ssh srv logs <name> serial
ssh srv logs <name> firecracker
ssh srv logs -f <name> serial
ssh srv logs -f <name> firecracker
```

The serial log is append-only across boots. The Firecracker log is reset when the VMM starts so it only contains the current Firecracker process's output.

## Stop

```bash
ssh srv stop <name>
```

Performs a graceful shutdown through Firecracker. The rootfs is preserved on disk.

## Start

```bash
ssh srv start <name>
```

Boots a previously stopped VM. Stopped guests pick up the currently configured `SRV_BASE_KERNEL` and optional `SRV_BASE_INITRD` on their next start.

## Restart

```bash
ssh srv restart <name>
```

Stops and starts the VM in one command. Also picks up the current kernel and initrd.

## Delete

```bash
ssh srv delete <name>
```

Removes the VM's rootfs, runtime directory, TAP device, cgroup, and jailer workspace. This is irreversible.

## Warm boot behavior

When a VM that already has `tailscaled` state reboots (via `start` or `restart`), it reuses its existing Tailscale identity instead of minting a new auth key. This is called a warm boot.

On cold boot (first `new`), the guest bootstrap service reads the Tailscale auth key from MMDS and runs `tailscale up --auth-key=... --ssh` exactly once.
