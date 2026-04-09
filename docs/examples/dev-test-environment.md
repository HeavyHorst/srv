# Dev/test environment

srv's reflink-based cloning makes it fast to spin up a VM from a base image, modify it, and then use backup/restore to reset it to a known-good state.

## Create a dev VM

```bash
ssh srv new dev --cpus 4 --ram 8G --rootfs-size 30G
```

## Install your toolchain

```bash
ssh root@dev

# Inside the VM
pacman -Syu
pacman -S nodejs npm
# ... set up your project ...
```

## Back up the clean state

Once the VM is configured the way you like, take a checkpoint before making risky changes:

```bash
ssh srv stop dev
ssh srv backup create dev
ssh srv start dev
```

## Wipe and reset

After a bad experiment:

```bash
ssh srv stop dev
ssh srv backup list dev
ssh srv restore dev <backup-id>
ssh srv start dev
```

The restore rolls the rootfs back to the exact state captured at backup time. This is fast because it replaces the writable disk image rather than copying file by file.

## Repeat

You can create multiple backups at different points:

```bash
ssh srv stop dev
ssh srv backup create dev    # backup 1: clean
ssh srv start dev

# ... make changes ...

ssh srv stop dev
ssh srv backup create dev    # backup 2: with toolchain
ssh srv start dev
```

Then restore whichever snapshot you need:

```bash
ssh srv stop dev
ssh srv restore dev <backup-id>
ssh srv start dev
```

## Key constraints

- Backups and restores only work on stopped VMs
- Backups are tied to the original VM record — they cannot be restored onto a differently created VM even if the name is reused
- Restore is in-place: it overwrites the VM's current rootfs with the backup's rootfs