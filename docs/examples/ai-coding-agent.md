# Sandboxed AI coding agent

srv makes it straightforward to run AI coding agents in isolated microVMs. Each VM gets its own cgroup limits, per-instance Tailscale identity, and optional provider API proxies that inject host API keys without exposing them inside the guest.

For Amp users, the repository also includes an opinionated plugin that creates the VM, installs a dedicated Amp runner, opens a thread on that runner, and later pushes the thread's branch from the VM.

For an outcome-oriented overview, including Amp remote control, thread URLs, Tailscale development services, and a comparison with Orbs, start with [Amp runners on srv](amp-runners.md).

## Create a VM for an agent

```bash
ssh srv new agent-1 --cpus 4 --ram 8G --rootfs-size 30G
```

Wait for it to report ready:

```bash
ssh srv inspect agent-1
```

Look for `state: ready` and a `tailscale-ip`.

## Provider API proxies

When provider API keys such as `SRV_ZEN_API_KEY` or `SRV_DEEPSEEK_API_KEY` are configured on the host, `srv` binds per-instance HTTP proxies on the guest's gateway IP and the provider gateway ports. The proxies:

- Only accept requests from that VM's guest IP
- Forward `/v1/...` requests to upstream provider APIs with host keys injected
- The guest bootstrap writes `/root/.config/opencode/opencode.json` and Pi config under `/root/.pi/agent/` pointing at these gateways

This means the agent inside the VM can use `opencode`, `pi`, or any OpenAI-compatible client against the per-provider gateway URLs without ever seeing the real API keys.

## Connect the agent

```bash
ssh root@agent-1
```

The preinstalled `opencode` and `pi` CLIs are already configured to target the per-VM gateway. Chromium and `agent-browser` are also installed; `agent-browser` is configured to launch the system browser at `/usr/bin/chromium` instead of downloading a separate browser build.

If you are using a different agent framework, point its API client at:

```
http://<gateway-ip>:11434/v1
```

The gateway IP is the default route inside the VM. You can read it from the `inspect` output under `host-addr`.

## Run Amp on a dedicated VM runner

Install the bundled Amp plugin on the workstation where Amp is running:

```bash
mkdir -p ~/.config/amp/plugins
ln -sf "$PWD/contrib/amp/plugins/srv-runner.ts" ~/.config/amp/plugins/srv-runner.ts
```

Reload plugins from Amp's command palette after installing or updating it. The plugin expects:

- A Git repository with an `origin` remote, a checked-out branch that already exists on `origin`, and no detached HEAD
- Working `ssh srv ...` and `ssh root@<vm-name>` access from the workstation
- An authenticated Amp installation at `~/.local/share/amp/secrets.json`
- A local SSH agent when the repository uses an SSH remote

!!! note
    The plugin always copies the Amp credential required by the runner. To add workstation-wide default skills, binaries, private files, guest symlinks, or `NO_PROXY` entries, create `~/.config/amp/srv-runner.json`; see the [plugin README](../../contrib/amp/plugins/README.md) and its full example profile. Put repository-specific dependencies and setup in an executable `.agents/setup` script, which runs inside the cloned repository before the runner starts.

Run **srv Runner: New isolated thread** from the command palette. The plugin asks for the task, Amp mode, VM size, and explicit consent to copy the Amp credential and configured defaults. It then:

1. Creates an `amp-<repository>-<suffix>` VM and waits for guest SSH.
2. Copies the Amp credential and optional workstation-wide profile into the persistent VM.
3. Installs Amp, makes a fresh clone of the current branch under `/workspace/repository`, initializes submodules, and runs `.agents/setup` when that executable exists.
4. Configures `amp-runner.service` to use the VM's per-instance Amp CONNECT gateway, starts a dedicated Amp runner, creates a thread on it, and sends the requested task.

The clone comes from `origin`; uncommitted and untracked workstation changes are not copied. Credentials remain in the persistent guest until the VM is deleted, so treat the VM as sensitive state.

The plugin also provides commands to show the VM for the current thread, list all managed VMs, start or stop the current VM, retry failed provisioning, and permanently delete a managed VM. Failed provisioning leaves the VM intact for inspection.

### Amp remote control and thread features

The dedicated process runs with `--remote-control-terminal`. It is a normal Amp runner thread rather than a separate agent UI, so the thread remains available through ampcode.com on desktop or mobile with its conversation, visibility, changes UI, and remote terminal while the VM is online.

Amp plugins and agents can target the live runner for additional threads. Scheduled or plugin-created work needs the runner to be online, and Amp cannot currently wake a stopped srv VM; start it first with **srv Runner: Start current thread VM**. Amp currently limits Multiplayer to Orb-backed threads.

### Development services over Tailscale

The VM's Tailscale name is a private network endpoint for more than SSH. Services that listen on a Tailscale-reachable address can be opened directly from authorized tailnet devices over TCP or UDP—for example a web app on `http://<vm-name>:3000` or a database on `<vm-name>:5432`.

This covers the connectivity role of an Amp Portal and supports more protocols, but it does not embed the service in the Amp thread, create Amp-managed HTTPS, or grant access based on thread visibility. Tailnet ACLs control access. See [Networking overview](../networking/overview.md#application-and-development-services).

## Push an isolated thread's branch

From a plugin-managed thread, run **srv Runner: Push current VM branch…**. This command only supports SSH Git remotes. Before asking for confirmation, it reads the VM's current branch and commit over host-key-verified SSH. The confirmation shows the repository, branch, and abbreviated commit that will be pushed.

After confirmation, the plugin temporarily forwards the workstation's SSH agent and pushes that exact commit to the same branch. The push:

- Aborts if the VM's branch or commit changes after confirmation
- Uses a temporary bare repository and disables repository hooks and global/system Git configuration for the operation
- Requires strict host-key checking for the Git server
- Ends agent forwarding when the command completes, with a two-minute overall timeout

The push does not include uncommitted changes. Commit all intended VM changes first.

## Resource limits

Each VM runs in its own cgroup v2 leaf with:

- `cpu.max` — capped at the vCPU count
- `memory.max` — capped at the requested guest RAM plus a small Firecracker overhead reserve
- `memory.swap.max` — set to 0 (no swap)
- `pids.max` — default 512, configurable via `SRV_VM_PIDS_MAX`

This prevents a misbehaving agent from consuming the entire host.

## Clean up

```bash
ssh srv delete agent-1
```

For a plugin-managed thread, **srv Runner: Delete current thread VM** removes the VM, persistent checkout, copied Amp credential, workstation profile files, and local plugin record.

## Multiple agents

Create as many agent VMs as the host can hold. Each gets independent networking, identity, and resource limits:

```bash
ssh srv new agent-2 --cpus 2 --ram 4G
ssh srv new agent-3 --cpus 2 --ram 4G
```

Use `ssh srv status` to check remaining host capacity before creating more.
