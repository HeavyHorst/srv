# Resize a VM

Resize lets you change the vCPU count, memory, and rootfs size of a stopped VM.

## Prerequisites

- The VM must be **stopped** — resize is rejected for running VMs
- **vCPUs** and **memory** can be increased or decreased as long as the requested values stay within the supported limits
- **Rootfs size** is **grow-only** — shrink requests are rejected
- The srv host must have both `e2fsck` and `resize2fs` available for a rootfs grow (and when creating a VM with a custom rootfs size)

## Resize

```bash
ssh srv stop demo
ssh srv resize demo --cpus 4 --ram 8G --rootfs-size 20G
ssh srv start demo
```

You can specify any combination of flags. Omitted flags keep the current value:

```bash
# Only increase RAM
ssh srv stop demo
ssh srv resize demo --ram 16G
ssh srv start demo
```

## How it works

- **vCPU count**: stored in the instance record and applied on the next boot
- **Memory**: stored in the instance record and applied on the next boot
- **Rootfs size**: runs `e2fsck -f -p` against the stopped ext4 image, expands the underlying file, and then uses `resize2fs` to grow the filesystem. Fatal or uncorrected filesystem errors stop the resize before the image is expanded. This operation only increases the filesystem — it never shrinks

If an earlier attempt expanded the image file but did not finish growing the filesystem, retrying the same `resize` request checks the filesystem and resumes the pending filesystem grow.

!!! warning
    Rootfs resize modifies the disk image in place. Take a [backup](backup-and-restore.md) first if you want a safety net.

## Limits

| Dimension | Minimum | Maximum |
|-----------|----------|---------|
| vCPUs | 1 | 32 |
| Memory | 128 MiB | host limit |
| Rootfs | current size | host disk limit |

vCPU count must be 1 or an even number. CPU and memory changes are applied on the next boot.
