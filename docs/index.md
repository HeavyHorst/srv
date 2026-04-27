# Introduction to srv

srv is a self-hosted control-plane service for creating Firecracker microVMs over SSH on a Tailscale tailnet.

It exposes an SSH command surface on one Linux host and manages Firecracker microVMs behind it. You create, inspect, stop, start, resize, back up, and delete VMs the same way you would run a remote command — through `ssh srv <command>`.

```bash
ssh srv new demo
# demo created — state: provisioning
# inspect:  ssh srv inspect demo
# connect:  ssh root@demo
```

## Where srv fits

srv is useful for both short-lived sandboxes and persistent isolated services.

- **Throwaway debug VMs** — spin up an isolated environment, break things, and delete it without affecting the host
- **Sandboxed agent VMs** — give AI coding agents their own cgroup-limited VM with per-instance Tailscale identity and scoped provider API proxies
- **Dev/test environments** — fast reflink-based clones from a single base image, with backup/restore for instant reset
- **Isolated workloads** — run services in separate microVMs with per-VM networking, auth, and resource limits

All VMs get a full Linux system with systemd, a real kernel, and their own Tailscale identity. Where containers are not enough — package managers, services that need root, bespoke networking configurations, predictable boot and teardown — srv gives you real isolated machines in seconds.

## Conceptual architecture

<!-- srv-manual:block=diagram -->
```
                    Tailscale tailnet
                           │
                     ┌─────┴─────┐
                     │   tsnet   │  (joins tailnet as "srv")
                     │  :22/tcp  │  (SSH API surface)
                     └─────┬─────┘
                           │
             ┌─────────────┼─────────────┐
             │             │             │
      ┌──────┴──────┐ ┌──────┴──────┐ ┌──────┴──────┐
      │ VM: demo    │ │ VM: ci      │ │ VM: test    │
      │ /30 net     │ │ /30 net     │ │ /30 net     │
      │ cgroup      │ │ cgroup      │ │ cgroup      │
      │ TAP + NAT   │ │ TAP + NAT   │ │ TAP + NAT   │
      └─────────────┘ └─────────────┘ └─────────────┘
```

Key components:

- **tsnet** — joins the tailnet as `srv` and exposes the control API on tailnet TCP port 22
- **gliderlabs/ssh** — handles `exec` requests and rejects shell sessions
- **SQLite** — stores instances, events, command audits, and authorization decisions
- **Reflinks** — clone the base rootfs for fast per-instance writable disks
- **Network helper** — root-only process owns TAP creation, iptables MASQUERADE, and FORWARD rules
- **VM runner** — root-owned process invokes Firecracker through the official jailer, drops to `srv-vm:srv`, and places each VM into its own cgroup v2 leaf
- **MMDS** — one-off Tailscale auth keys are injected through Firecracker metadata so guests self-bootstrap
- **Provider gateways** — per-instance HTTP proxies on the guest's gateway IP forward to LLM providers with host keys
- **HTTP integrations** — admin-defined host-side HTTP proxies inject headers or auth for selected guests without storing raw secrets inside the VM

## Where you can run it

srv runs on Linux with the following requirements:

- Linux host with cgroup v2 and `/dev/kvm`
- IPv4 forwarding enabled (`net.ipv4.ip_forward=1`)
- `SRV_DATA_DIR` on a reflink-capable filesystem (`btrfs` or reflink-enabled `xfs`)
- Tailscale installed and working on the host
- Tailscale OAuth client credentials with permission to mint auth keys for guest tags
- `ip`, `iptables`, `cp`, and `resize2fs` available on the host
- Official static Firecracker and jailer release pair

## Next steps

- [Install srv](getting-started/install.md)
- [Walkthrough](getting-started/walkthrough.md)
- [Architecture](reference/architecture.md)
- [SSH command reference](reference/ssh-commands.md)
- [RFC: Hybrid memory pools](reference/hybrid-memory-pools-rfc.md)
- [HTTP integrations](networking/integrations.md)
