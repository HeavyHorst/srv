# Backup and restore

srv provides in-place backup and restore for stopped VMs. This is useful for checkpointing VMs before risky changes and rolling them back when something goes wrong.

## Create a backup

```bash
ssh srv stop demo
ssh srv backup create demo
```

This copies the current rootfs image into the backup store under `SRV_DATA_DIR/backups/demo/<backup-id>/`.

## List backups

```bash
ssh srv backup list demo
```

Each backup has a unique ID and timestamp.

## Restore from a backup

```bash
ssh srv restore demo <backup-id>
```

This replaces the current rootfs with the backup's rootfs. The VM must be stopped.

## Full workflow

```bash
# Create and configure a VM
ssh srv new demo
ssh root@demo  # install packages, configure services

# Checkpoint before risky changes
ssh srv stop demo
ssh srv backup create demo
ssh srv start demo

# ... make changes that break things ...

# Reset to the checkpoint
ssh srv stop demo
ssh srv backup list demo
ssh srv restore demo <backup-id>
ssh srv start demo

# Verify the restore worked — the VM should be back to the checkpoint state
```

## Constraints

- **Stopped only**: both backup and restore require the VM to be stopped
- **In-place only**: backups are tied to the original VM record. They cannot be restored onto a newly created VM that reuses the same name
- **Single host**: backups live on the same host. For cross-host migration, use [export/import](export-import-vm.md)
- **Rootfs only**: backups capture the writable rootfs. The kernel and initrd come from the host's current configuration on the next `start`

## Backup storage

Backups are stored under:

```
SRV_DATA_DIR/backups/<name>/<backup-id>/
```

Monitor disk usage if you create many backups — each one is a full copy of the rootfs at that point in time.