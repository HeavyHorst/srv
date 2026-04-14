# Throwaway debug VM

One of the most immediate uses for srv is spinning up an isolated Linux environment where you can install packages, run risky commands, or reproduce a bug — then delete it without any trace on the host.

## Create

```bash
ssh srv new debug-vm
```

By default the VM gets 1 vCPU, 1 GiB of RAM, and a 10 GiB rootfs. Adjust if you need more:

```bash
ssh srv new debug-vm --cpus 2 --ram 4G --rootfs-size 30G
```

## Watch it boot

```bash
ssh srv logs -f debug-vm serial
```

You will see the kernel boot, `srv-bootstrap.service` set up networking and Tailscale, and `tailscale up --ssh` complete. Once `inspect` reports `state: ready`:

```bash
ssh srv inspect debug-vm
```

Look for the `tailscale-name` and `tailscale-ip` fields.

## Connect and use

```bash
ssh root@debug-vm
```

The guest image comes with Docker, Go, Neovim, Git, `perf`, `valgrind`, and common development tools preinstalled. Install anything else with `pacman -S`.

## Clean up

When you are done:

```bash
ssh srv delete debug-vm
```

This removes the VM's rootfs, runtime directory, TAP device, cgroup, and jailer workspace. No trace remains on the host.

## Tips

- Use `-- --json inspect <name>` to get machine-readable output for scripting
- The serial log under `ssh srv logs <name> serial` is append-only — always check the newest lines
- If a VM gets stuck in `provisioning` or `failed`, check `ssh srv inspect <name>` for the `last_error` field and `ssh srv logs <name> firecracker` for VMM errors
