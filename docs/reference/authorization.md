# Authorization

srv uses Tailscale identity for authentication and authorization. The SSH surface does not use traditional SSH username/password authentication — instead, it resolves the caller's Tailscale identity from the incoming tailnet connection.

## How it works

1. A user connects to `srv` over the tailnet: `ssh srv <command>`
2. tsnet resolves the connection using `WhoIs` to get the Tailscale login and node
3. The control plane checks the resolved identity against the configured allow and admin lists
4. If authorized, the command runs; the decision is recorded in the audit store

## User roles

### Regular users

If `SRV_ALLOWED_USERS` is unset or empty, any authenticated tailnet user can invoke commands. Regular users can:

- Create VMs (`new`)
- Manage their own VMs (`inspect`, `logs`, `stop`, `start`, `restart`, `delete`)
- View their own VMs in `list`
- Create backups and restores for their own VMs
- Resize their own VMs
- Export their own VMs

### Admin users

Users listed in `SRV_ADMIN_USERS` can additionally:

- View and manage **all** VMs, not just their own
- Run `status` (host capacity summary)
- Run `snapshot create` (host-level btrfs snapshot)
- Import VMs

## Configuration

| Variable | Description |
|----------|-------------|
| `SRV_ALLOWED_USERS` | Comma-separated Tailscale login allowlist. Empty means allow all tailnet users. |
| `SRV_ADMIN_USERS` | Comma-separated Tailscale logins with cross-instance visibility and admin rights. |

Example `/etc/srv/srv.env`:

```bash
SRV_ALLOWED_USERS=alice@example.com,bob@example.com
SRV_ADMIN_USERS=ops@example.com
```

## Audit trail

Every command invocation is recorded in the SQLite store with:

- Actor Tailscale login and display name
- Actor node name
- Remote address
- SSH user
- Command and arguments
- Whether the command was allowed
- Reason for allow/deny
- Duration in milliseconds
- Error text if any

This audit trail is visible through `ssh srv -- --json inspect <name>` in the instance events.