# Export and import

The `export` and `import` commands stream a portable VM artifact between hosts. This is the supported way to move a VM from one srv host to another.

## Export

On the source host:

```bash
ssh srv stop demo
ssh srv export demo
```

The command writes a tar stream to stdout. The artifact contains:

- A versioned manifest
- The writable `rootfs.img`
- Serial and Firecracker logs when present (each capped to the newest 256 MiB)

## Import

Pipe the export stream directly into the destination host:

```bash
ssh srv-a export demo | ssh srv-b import
```

Import creates the VM under the same name on the destination host, allocates new runtime paths and network state, and leaves the VM stopped.

## Start after import

```bash
ssh srv-b start demo
```

The destination host uses its currently configured `SRV_BASE_KERNEL` and optional `SRV_BASE_INITRD` — only the writable disk and optional logs come from the streamed artifact.

## Important semantics

Because the guest's durable Tailscale identity lives in the copied rootfs:

- **Do not boot the source and destination VMs at the same time** — this would cause a Tailscale key conflict
- Treat export/import as **cutover or move semantics**, not cloning semantics

The destination host regenerates:

- Absolute file paths (runtime directories)
- TAP device wiring
- Guest MAC address
- VM `/30` subnet allocation

The artifact preserves:

- VM name
- Creator identity
- Machine shape (vCPUs, memory, rootfs size)
- Last-known Tailscale name and IP (as cached state)

## Save to a file

You can also save the export to a file for later or offline transfer:

```bash
ssh srv export demo > demo-backup.tar
```

Then on the destination:

```bash
cat demo-backup.tar | ssh srv import
```