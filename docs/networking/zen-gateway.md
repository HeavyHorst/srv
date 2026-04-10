# Zen gateway

When `SRV_ZEN_API_KEY` is configured on the host, srv sets up a per-instance HTTP proxy that allows guest VMs to reach the OpenCode Zen API without storing the real API key inside the guest.

## How it works

```
┌─────────────────────────────────────────────┐
│                  Host                        │
│                                              │
│  ┌──────────────┐    ┌──────────────────┐  │
│  │  srv control  │    │  Zen gateway      │  │
│  │  plane        │    │  :11434 on         │  │
│  │              │    │  gateway IP         │  │
│  └──────────────┘    │                    │  │
│                       │  Injects          │  │
│                       │  SRV_ZEN_API_KEY   │  │
│                       │  into Authorization │  │
│                       │  header             │  │
│                       └────────┬───────────┘  │
│                                │              │
│                    ┌───────────┴───────────┐  │
│                    │  Upstream:             │  │
│                    │  opencode.ai/zen/v1    │  │
│                    └───────────────────────┘  │
└─────────────────────────────────────────────────┘
         │
         │ /30 network
         │
┌────────┴────────┐
│  Guest VM       │
│                 │
│  opencode →    │
│  http://gateway │
│  :11434/v1     │
└─────────────────┘
```

For each VM, `srv` binds an HTTP proxy on the VM's host/gateway IP address and the configured `SRV_ZEN_GATEWAY_PORT` (default `11434`). The proxy:

- Only accepts requests from that VM's guest IP
- Forwards `/v1/...` requests to the upstream Zen API
- Injects the host's `SRV_ZEN_API_KEY` into the `Authorization` header
- The real key never leaves the host

## Guest bootstrap

When the Zen gateway is enabled, the guest `srv-bootstrap.service` writes `/root/.config/opencode/opencode.json` targeting the per-VM gateway:

```json
{
  "provider": "opencode",
  "apiKey": "local-placeholder",
  "baseURL": "http://<gateway-ip>:11434/v1"
}
```

The `apiKey` is a local placeholder only so OpenCode keeps Zen's paid model catalog visible — the real credential still lives only on the host and is injected by the proxy.

Bootstrap also writes Pi config under `/root/.pi/agent/` so the preinstalled `pi` CLI uses the same gateway by default:

```json
{
  "providers": {
    "opencode": {
      "baseUrl": "http://<gateway-ip>:11434/v1",
      "apiKey": "srv-zen-gateway"
    }
  }
}
```

When the gateway is disabled (no `SRV_ZEN_API_KEY`), bootstrap removes those managed default config files.

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `SRV_ZEN_API_KEY` | (empty) | OpenCode Zen API key. When set, enables per-VM gateways. |
| `SRV_ZEN_BASE_URL` | `https://opencode.ai/zen` | Upstream Zen API base URL |
| `SRV_ZEN_GATEWAY_PORT` | `11434` | TCP port for each VM's gateway proxy |

## Using the gateway from the guest

The preinstalled `opencode` and `pi` CLIs work out of the box. For other agents or HTTP clients:

```bash
# Inside the VM
curl http://$(ip route show default | awk '{print $3}'):11434/v1/models
```

## Disabling the gateway

Remove or leave `SRV_ZEN_API_KEY` unset. After the next guest boot, the bootstrap service will remove the managed OpenCode and Pi config files.
