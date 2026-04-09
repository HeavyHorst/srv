# Sandboxed AI coding agent

srv makes it straightforward to run AI coding agents in isolated microVMs. Each VM gets its own cgroup limits, per-instance Tailscale identity, and an optional Zen API proxy that injects the host's API key without exposing it inside the guest.

## Create a VM for an agent

```bash
ssh srv new agent-1 --cpus 4 --ram 8G --rootfs-size 30G
```

Wait for it to report ready:

```bash
ssh srv inspect agent-1
```

Look for `state: ready` and a `tailscale-ip`.

## Zen API proxy

When `SRV_ZEN_API_KEY` is configured on the host, `srv` binds a per-instance HTTP proxy on the guest's gateway IP and port `11434` (configurable via `SRV_ZEN_GATEWAY_PORT`). The proxy:

- Only accepts requests from that VM's guest IP
- Forwards `/v1/...` requests to the upstream OpenCode Zen API with the host key injected
- The guest bootstrap writes `/root/.config/opencode/opencode.json` pointing at this gateway

This means the agent inside the VM can use `opencode` or any OpenAI-compatible client against `http://<gateway-ip>:11434/v1` without ever seeing the real API key.

## Connect the agent

```bash
ssh root@agent-1
```

The preinstalled `opencode` CLI is already configured to target the per-VM gateway. If you are using a different agent framework, point its API client at:

```
http://<gateway-ip>:11434/v1
```

The gateway IP is the default route inside the VM. You can read it from the `inspect` output under `host-addr`.

## Resource limits

Each VM runs in its own cgroup v2 leaf with:

- `cpu.max` — capped at the vCPU count
- `memory.max` — capped at the requested RAM
- `memory.swap.max` — set to 0 (no swap)
- `pids.max` — default 512, configurable via `SRV_VM_PIDS_MAX`

This prevents a misbehaving agent from consuming the entire host.

## Clean up

```bash
ssh srv delete agent-1
```

## Multiple agents

Create as many agent VMs as the host can hold. Each gets independent networking, identity, and resource limits:

```bash
ssh srv new agent-2 --cpus 2 --ram 4G
ssh srv new agent-3 --cpus 2 --ram 4G
```

Use `ssh srv status` to check remaining host capacity before creating more.