# Provider gateways

When API keys are configured on the host, srv sets up per-instance HTTP proxies that allow guest VMs to reach LLM provider APIs without storing real API keys inside the guest.

## Providers

| Provider | Env var | Default upstream | Gateway port |
|----------|---------|-----------------|-------------|
| OpenCode Zen | `SRV_ZEN_API_KEY` | `https://opencode.ai/zen` | `11434` (`SRV_ZEN_GATEWAY_PORT`) |
| DeepSeek | `SRV_DEEPSEEK_API_KEY` | `https://api.deepseek.com` | `11436` (`SRV_DEEPSEEK_GATEWAY_PORT`) |

Both gateways share the same architecture. Only providers with a configured API key are enabled.

## How it works

<!-- srv-manual:block=diagram -->
```
┌───────────────────────────────────────────────────┐
│                       Host                        │
│                                                   │
│  ┌───────────────┐   ┌────────────────────────┐   │
│  │ srv control   │   │ Provider gateways      │   │
│  │ plane         │   │ :11434 (Zen)           │   │
│  │               │   │ :11436 (DeepSeek)      │   │
│  └───────────────┘   │ on gateway IP          │   │
│                      │                        │   │
│                      │ injects API key        │   │
│                      │ into Authorization     │   │
│                      │ header                 │   │
│                      └───────────┬────────────┘   │
│                                  │                │
│                     ┌────────────┴───────────┐    │
│                     │ upstreams:              │    │
│                     │ opencode.ai/zen/v1      │    │
│                     │ api.deepseek.com/v1     │    │
│                     └────────────────────────┘    │
└───────────────────────────────────────────────────┘
                  │
                  │ /30 network
                  │
        ┌─────────┴─────────┐
        │ Guest VM          │
        │                   │
        │ opencode / pi ->  │
        │ http://gateway    │
        │ :11434/v1 (Zen)   │
        │ :11436/v1 (DS)    │
        └───────────────────┘
```

For each VM and each enabled provider, `srv` binds an HTTP proxy on the VM's host/gateway IP address and the provider's configured port. The proxies:

- Only accept requests from that VM's guest IP
- Forward `/v1/...` requests to the upstream API
- Inject the host's API key into the `Authorization` header (Bearer auth)
- The real keys never leave the host

DeepSeek uses OpenAI-compatible Bearer auth exclusively. The Zen gateway additionally supports Anthropic (`X-API-Key`) and Google (`X-Goog-Api-Key`) auth styles based on request path.

## Guest bootstrap

When any provider gateway is enabled, the guest `srv-bootstrap.service` writes `/root/.config/opencode/opencode.json` targeting the per-VM gateways:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "opencode": {
      "options": {
        "baseURL": "http://<gateway-ip>:11434/v1",
        "apiKey": "srv-provider-gateway"
      }
    },
    "deepseek": {
      "options": {
        "baseURL": "http://<gateway-ip>:11436/v1",
        "apiKey": "srv-provider-gateway"
      }
    }
  }
}
```

The `apiKey` is a local placeholder only so the CLIs keep the paid model catalogs visible — the real credentials still live only on the host and are injected by the proxies.

Bootstrap also writes Pi config under `/root/.pi/agent/` so the preinstalled `pi` CLI uses both gateways by default:

```json
{
  "providers": {
    "opencode": {
      "baseUrl": "http://<gateway-ip>:11434/v1",
      "apiKey": "srv-provider-gateway"
    },
    "deepseek": {
      "baseUrl": "http://<gateway-ip>:11436/v1",
      "apiKey": "srv-provider-gateway",
      "compat": {
        "supportsDeveloperRole": false,
        "supportsStore": false
      }
    }
  }
}
```

When all gateways are disabled, bootstrap removes those managed default config files.

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `SRV_ZEN_API_KEY` | (empty) | OpenCode Zen API key |
| `SRV_ZEN_BASE_URL` | `https://opencode.ai/zen` | Upstream Zen API base URL |
| `SRV_ZEN_GATEWAY_PORT` | `11434` | TCP port for Zen gateway proxy |
| `SRV_DEEPSEEK_API_KEY` | (empty) | DeepSeek API key |
| `SRV_DEEPSEEK_BASE_URL` | `https://api.deepseek.com` | Upstream DeepSeek API base URL |
| `SRV_DEEPSEEK_GATEWAY_PORT` | `11436` | TCP port for DeepSeek gateway proxy |

## Using the gateways from the guest

The preinstalled `opencode` and `pi` CLIs work out of the box. For other agents or HTTP clients:

```bash
# Inside the VM — Zen
curl http://$(ip route show default | awk '{print $3}'):11434/v1/models

# Inside the VM — DeepSeek
curl http://$(ip route show default | awk '{print $3}'):11436/v1/models
```

## Disabling gateways

Remove or leave the respective API key env var unset. After the next guest boot, the bootstrap service will remove the managed config files for disabled providers.
