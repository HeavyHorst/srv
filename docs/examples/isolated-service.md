# Isolated service

Each srv VM runs in its own cgroup v2 leaf with its own `/30` network subnet, TAP device, and Tailscale identity. This makes it straightforward to run services that need full isolation — container runtimes, network services, databases — without sharing the host's PID, network, or filesystem namespace.

## Run a database

```bash
ssh srv new db --cpus 2 --ram 4G --rootfs-size 20G
```

Once the VM is ready:

```bash
ssh root@db
pacman -S postgresql
# ... configure and start postgresql ...
```

The database is now reachable at its Tailscale IP from any other machine on the tailnet.

## Run Docker workloads

The guest image includes Docker and docker-compose:

```bash
ssh srv new builder --cpus 4 --ram 8G --rootfs-size 40G
```

```bash
ssh root@builder
docker run ...
docker compose up -d
```

The overlay and br_netfilter kernel modules are preloaded, and nftables supports both IPv4 and IPv6 families.

## Per-VM resource isolation

Each VM is enforced by cgroup v2:

| Resource | Limit |
|----------|-------|
| CPU | vCPU count (advisory, allows overcommit) |
| Memory | Requested RAM, no swap |
| PIDs | 512 by default (`SRV_VM_PIDS_MAX`) |

You can verify the live cgroup limits:

```bash
cat /sys/fs/cgroup/firecracker-vms/<name>/cpu.max
cat /sys/fs/cgroup/firecracker-vms/<name>/memory.max
cat /sys/fs/cgroup/firecracker-vms/<name>/memory.swap.max
cat /sys/fs/cgroup/firecracker-vms/<name>/pids.max
```

## Per-VM networking

Each VM gets its own `/30` subnet, TAP device, and NAT rules. VMs cannot reach each other's private networks. They can reach the host and the internet through MASQUERADE rules.

From any tailnet machine, you can SSH directly to the VM's Tailscale name or IP — no port forwarding needed.

## Clean separation

When the service is no longer needed:

```bash
ssh srv delete db
```

The rootfs, TAP device, cgroup, jailer workspace, and iptables rules are all cleaned up.