# Networking overview

Every srv VM gets its own isolated network stack: a dedicated `/30` subnet, TAP device, and NAT rules. Guest egress is routed through the host's outbound interface after MASQUERADE.

## How it works

<!-- srv-manual:block=diagram -->
```
Internet
    │
    │ (host outbound interface)
    │
┌───┴─────────────────────┐
│      Linux host         │
│  ┌──────────────────┐   │
│  │ iptables MASQ    │   │
│  │ + FORWARD rules  │   │
│  └────────┬─────────┘   │
│           │             │
│  ┌────────┴─────────┐   │
│  │ TAP device       │   │
│  │ (per-VM /30)     │   │
│  └────────┬─────────┘   │
│           │             │
│  ┌────────┴─────────┐   │
│  │ Firecracker VM   │   │
│  │ gateway = .1     │   │
│  │ guest   = .2     │   │
│  └──────────────────┘   │
└─────────────────────────┘
```

When `ssh srv new demo` runs, the network helper:

1. Allocates the next free `/30` from `SRV_VM_NETWORK_CIDR` (default `172.28.0.0/16`)
2. Creates a TAP device for the VM
3. Installs MASQUERADE and FORWARD rules for guest egress
4. Configures the gateway address on the host side

The VM's bootstrap configures the guest interface with:

- Gateway: `SRV_DATA_DIR`-derived host address (first usable IP in the `/30`)
- Guest IP: second usable IP in the `/30`
- DNS: configurable via `SRV_VM_DNS` (default `1.1.1.1, 1.0.0.1`)

## Tailscale integration

Each VM gets its own Tailscale identity. The control plane mints a one-off auth key and injects it through Firecracker MMDS metadata. The guest bootstrap service:

1. Reads the MMDS payload
2. Starts `tailscaled`
3. Runs `tailscale up --auth-key=... --ssh` on the first authenticated boot
4. Persists `tailscaled` state for warm reboots

This means any machine on the tailnet can reach the VM by its Tailscale name or IP — no port forwarding needed.

### SSH access

Guests expose SSH through Tailscale's `--ssh` flag, so `ssh root@<tailscale-name>` works from any tailnet machine. Per-user OpenSSH keys are not injected — Tailscale SSH handles authentication based on tailnet identity.

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `SRV_VM_NETWORK_CIDR` | `172.28.0.0/16` | IPv4 network reserved for VM `/30` allocations |
| `SRV_VM_DNS` | `1.1.1.1,1.0.0.1` | Comma-separated guest nameservers |
| `SRV_OUTBOUND_IFACE` | auto-detected | Optional override for the host interface used for NAT |

## IPv4 forwarding

Guest NAT depends on IP forwarding:

```bash
sudo tee /etc/sysctl.d/90-srv-ip-forward.conf >/dev/null <<'EOF'
net.ipv4.ip_forward = 1
EOF
sudo sysctl --system
```

This must stay enabled. Disabling it breaks guest egress.

## Network cleanup

When a VM is deleted, the network helper removes:

- The TAP device
- The MASQUERADE rule
- The FORWARD rule
- The gateway address from the host interface

## Host-side API gateways

srv can also expose host-side HTTP gateways on each VM's gateway IP:

- Provider gateways on `SRV_ZEN_GATEWAY_PORT` and `SRV_DEEPSEEK_GATEWAY_PORT` proxy `/v1/...` to configured LLM upstreams with host API keys injected.
- The generic integration gateway on `SRV_INTEGRATION_GATEWAY_PORT` proxies `/integrations/<name>/...` to operator-defined HTTP integrations with host-managed auth or headers injected.

Both gateway types only accept requests from the owning guest IP. See [Provider gateways](provider-gateways.md) and [HTTP integrations](integrations.md).
