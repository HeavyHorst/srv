# Amp runners on srv

Run Amp threads in dedicated, self-hosted Firecracker microVMs without giving up Amp's remote experience.

Each **New isolated thread** action creates a VM with a fresh repository clone, its own resource limits and Tailscale identity, and a dedicated Amp runner. The resulting thread is still a normal Amp thread: open it from the web or your phone, review its conversation and changes, and use its remote terminal while srv provides the machine underneath.

## One action creates the whole environment

Run **srv Runner: New isolated thread** from Amp's command palette. The plugin turns a task into a ready remote thread:

<!-- srv-manual:block=diagram -->
```
Task + mode + size
        │
        ▼
┌─────────────────────┐
│ Dedicated srv VM    │
│ Firecracker + cgroup│
└──────────┬──────────┘
           │ clone + .agents/setup
           │ workstation profile
           ▼
┌─────────────────────┐
│ Dedicated Amp runner│
│ persistent systemd  │
└──────────┬──────────┘
           │
           ▼
┌─────────────────────┐
│ Normal Amp thread   │
│ web · mobile · diff │
│ remote terminal     │
└─────────────────────┘
```

The clone comes from the current branch on `origin`. Local uncommitted and untracked files stay on the workstation.

## Keep the Amp thread experience

srv replaces the executor, not Amp's thread and control plane. The plugin starts `amp --no-tui` with a stable runner ID and `--remote-control-terminal`, so a thread running in the VM retains:

- Remote control from ampcode.com on desktop or mobile
- The normal Amp thread URL, conversation, visibility, and changes UI
- A web-accessible terminal while the runner is online
- Amp project and thread workflows that apply to CLI runner threads
- The ability for Amp plugins and agents to target the live runner for more threads that share that VM

The VM must be running for work to execute. srv has no Orb-style automatic wake integration: any schedule or external or plugin automation that targets this runner requires the VM to already be running. Amp's Multiplayer feature is currently restricted to Orb-backed threads. See Amp's [Owner's Manual](https://ampcode.com/manual) for the current runner, thread-sharing, and remote-control behavior.

## Tailscale services instead of HTTP-only portals

Every VM joins the tailnet with its own identity and MagicDNS name. A development service can be reached directly by authorized tailnet devices without port forwarding:

```text
http://<vm-name>:3000
postgresql://<vm-name>:5432/app
ssh root@<vm-name>
```

This is broader than an HTTP preview tunnel: Tailscale carries TCP and UDP, supports multiple simultaneous services, works with native clients, and applies tailnet ACLs to the whole VM identity. The service must listen on an address reachable through Tailscale, and the VM must be running.

Amp Portals provide conveniences that raw Tailscale connectivity does not: Amp-managed HTTPS, embedding and annotation in the thread, access derived from thread visibility, and automatic coupling to Orb sleep and wake. srv instead provides private, general-purpose network access to the complete VM. You can put a Tailscale service address in the thread, but it remains accessible only to authorized tailnet devices.

## Bring your environment with you

There are two setup layers:

- `~/.config/amp/srv-runner.json` defines workstation-wide skills, binaries, private files, guest symlinks, and `NO_PROXY` entries to copy into every runner VM.
- An executable `.agents/setup` in the repository installs project-specific dependencies after the clone.

Configured files are validated before VM creation. The confirmation shows how many profile items will enter the persistent VM.

## Host-controlled credentials and egress

The Amp credential is stored root-only in the VM. Optional provider and HTTP integration gateways keep upstream API credentials on the srv host and expose only per-VM proxy endpoints. General public HTTPS traffic from the runner uses a per-VM CONNECT gateway that rejects private, tailnet, host-local, and special-use destinations.

Treat the VM as sensitive persistent state: its Amp credential and configured private files remain until deletion.

## Persistent when useful, disposable when done

Unlike an Orb, an srv VM is persistent and consumes capacity on hardware you operate.

- **Stop** it to release runtime resources while preserving the checkout and disk.
- **Start** it again and the systemd-managed Amp runner reconnects.
- **Delete** it to remove the VM, checkout, copied credentials, and plugin state.
- **Inspect** or SSH into failed provisioning attempts instead of losing the environment.

## srv runner and Amp Orb at a glance

| | srv Amp runner | Amp Orb |
|---|---|---|
| Compute | Your Firecracker host | Amp-managed infrastructure |
| Capacity | Bounded by the host | Elastic hosted capacity |
| Lifecycle | Persistent; explicit start, stop, delete | Ephemeral; automatic pause and wake |
| Thread URL and remote control | Standard Amp thread and runner remote control | Standard Amp thread and Orb control |
| Service access | Private Tailscale TCP/UDP under tailnet ACLs | Amp-managed HTTP Portals |
| Environment | Workstation profile plus `.agents/setup` | Amp project and Orb setup |
| Networking and identity | Per-VM Tailscale identity | Amp-managed Orb networking |
| Multiplayer | Not supported for runner threads | Supported for eligible Orb threads |

Choose srv when you want Amp's remote thread experience on hardware, networks, credentials, and VM boundaries you control. Choose [Orbs](https://ampcode.com/what-are-orbs) when managed elasticity, automatic wake-up, Portals, or Multiplayer matter more than operating the executor yourself.

## Get started

After meeting the [runner prerequisites](ai-coding-agent.md#run-amp-on-a-dedicated-vm-runner), install the bundled plugin from the srv checkout:

```bash
mkdir -p ~/.config/amp/plugins
ln -sf "$PWD/contrib/amp/plugins/srv-runner.ts" ~/.config/amp/plugins/srv-runner.ts
```

Reload plugins from Amp's command palette, then run **srv Runner: New isolated thread**.

Continue with the [AI coding-agent guide](ai-coding-agent.md) for detailed provisioning behavior, profile configuration, secure branch pushing, resource limits, and cleanup.
