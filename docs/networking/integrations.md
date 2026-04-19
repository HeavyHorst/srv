# HTTP integrations

srv can expose selected upstream HTTP APIs to a VM through a host-side integration gateway without storing the real upstream credentials inside the guest.

This feature is intentionally narrow in v1:

- Admin or operator managed only
- HTTP integrations only
- No guest-side secret submission over SSH
- No discovery, tagging, or attachment system
- No transparent interception of arbitrary outbound HTTPS

## How it works

<!-- srv-manual:block=diagram -->
```
┌──────────────────────────────────────────────────────┐
│                       Host                           │
│                                                      │
│  srv control plane                                   │
│      │                                               │
│      │ integration add/enable                        │
│      ▼                                               │
│  SQLite metadata + /etc/srv/srv.env                  │
│      │                                               │
│      ▼                                               │
│  Integration gateway :11435 on VM gateway IP         │
│      │                                               │
│      │ /integrations/openai/...                      │
│      │ injects headers/auth from SRV_SECRET_*        │
│      ▼                                               │
│  upstream HTTP API                                   │
└──────────────────────────────────────────────────────┘
                       │
                       │ /30 network
                       │
              ┌────────┴────────┐
              │ Guest VM        │
              │                 │
              │ curl http://    │
              │ <gateway>:11435 │
              │ /integrations/  │
              │ openai/...      │
              └─────────────────┘
```

Each enabled VM gets access to a host-side HTTP listener on its gateway IP and `SRV_INTEGRATION_GATEWAY_PORT` (default `11435`). Requests are routed by path prefix:

- `http://<gateway-ip>:11435/integrations/<name>/...`

The host looks up the named integration, rewrites the request to the configured upstream target, injects the configured auth or headers, and forwards the request.

## Supported auth and header modes

An integration can use any combination of:

- Static headers via `--header NAME:VALUE`
- Env-backed headers via `--header-env NAME:SRV_SECRET_FOO`
- Bearer auth via `--bearer-env SRV_SECRET_FOO`
- Basic auth via `--basic-user USER --basic-password-env SRV_SECRET_BAR`

Secrets are referenced by env var name only. The raw values are expected to live on the host, typically in `/etc/srv/srv.env`.

For `--header-env`, the env var value becomes the exact header value sent upstream.

## Security behavior

The gateway is designed so the guest gets access to the upstream capability, not the underlying secret material.

- The gateway only accepts requests from the owning guest IP
- Guest-supplied `Authorization` and `Proxy-Authorization` headers are stripped on the host
- Env-backed secrets are read on the host at request time
- If a referenced `SRV_SECRET_*` env var is missing, the gateway returns `502 Bad Gateway`
- Request paths are normalized before routing so traversal attempts cannot escape the configured integration prefix

## Example setup

Add host secrets to `/etc/srv/srv.env`:

```bash
SRV_INTEGRATION_GATEWAY_PORT=11435
SRV_SECRET_OPENAI_PROD=sk-live-redacted
SRV_SECRET_VENDOR_API_KEY=vendor-key-redacted
```

Create and enable integrations from the control plane:

```bash
ssh srv integration add http openai --target https://api.openai.com/v1 --bearer-env SRV_SECRET_OPENAI_PROD
ssh srv integration add http vendor --target https://vendor.example/api --header-env X-API-Key:SRV_SECRET_VENDOR_API_KEY
ssh srv integration enable demo openai
ssh srv integration enable demo vendor
ssh srv inspect demo
```

The VM's `inspect` output will include URLs such as:

```text
integrations:
- openai: http://172.28.0.1:11435/integrations/openai
- vendor: http://172.28.0.1:11435/integrations/vendor
```

From inside the guest, requests go to the gateway IP:

```bash
curl http://$(ip route show default | awk '{print $3}'):11435/integrations/openai/models
curl http://$(ip route show default | awk '{print $3}'):11435/integrations/vendor/ping
```

## Related references

- [SSH command reference](../reference/ssh-commands.md)
- [Configuration reference](../reference/configuration.md)
- [Networking overview](overview.md)
