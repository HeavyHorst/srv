# Sandboxed AI coding agent

srv makes it straightforward to run AI coding agents in isolated microVMs. Each VM gets its own cgroup limits, per-instance Tailscale identity, and optional provider API proxies that inject host API keys without exposing them inside the guest.

For Amp users, the repository also includes an opinionated plugin that creates the VM, installs a dedicated Amp runner, opens a thread on that runner, and later pushes the thread's branch from the VM.

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
- The personal skills, CLI binaries, and credential files declared near the top of `contrib/amp/plugins/srv-runner.ts`

!!! note
    The bundled profile is deliberately workstation-specific. Review and customize `personalSkillNames`, `personalBinaries`, and `personalConfigs` before using the plugin on another workstation. Provisioning stops before VM creation if any declared source is missing.

Run **srv Runner: New isolated thread** from the command palette. The plugin asks for the task, Amp mode, VM size, and explicit consent to copy the personal profile. It then:

1. Creates an `amp-<repository>-<suffix>` VM and waits for guest SSH.
2. Copies the Amp credential, selected skills, CLI binaries, and root-only personal configuration into the persistent VM.
3. Installs Amp, makes a fresh clone of the current branch under `/workspace/repository`, initializes submodules, and runs `.agents/setup` when that executable exists.
4. Configures `amp-runner.service` to use the VM's per-instance Amp CONNECT gateway, starts a dedicated Amp runner, creates a thread on it, and sends the requested task.

The clone comes from `origin`; uncommitted and untracked workstation changes are not copied. Credentials remain in the persistent guest until the VM is deleted, so treat the VM as sensitive state.

The plugin also provides commands to show the VM for the current thread, list all managed VMs, start or stop the current VM, retry failed provisioning, and permanently delete a managed VM. Failed provisioning leaves the VM intact for inspection.

## Push an isolated thread's branch

From a plugin-managed thread, run **srv Runner: Push current VM branch…**. This command only supports SSH Git remotes. Before asking for confirmation, it reads the VM's current branch and commit over host-key-verified SSH. The confirmation shows the repository, branch, and abbreviated commit that will be pushed.

After confirmation, the plugin temporarily forwards the workstation's SSH agent and pushes that exact commit to the same branch. The push:

- Aborts if the VM's branch or commit changes after confirmation
- Uses a temporary bare repository and disables repository hooks and global/system Git configuration for the operation
- Requires strict host-key checking for the Git server
- Ends agent forwarding when the command completes, with a two-minute overall timeout

The push does not include uncommitted changes. Commit all intended VM changes first.

After a successful push, the plugin asks the current Amp agent to use the `maintaining-room-memory` skill to distill the thread into a compact set of durable NRC notes, add useful note edges, and cite the Amp thread URL. The memory step is queued after the push; if it cannot be queued or later fails, the successful Git push is not rolled back.

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

## Multiple agents

Create as many agent VMs as the host can hold. Each gets independent networking, identity, and resource limits:

```bash
ssh srv new agent-2 --cpus 2 --ram 4G
ssh srv new agent-3 --cpus 2 --ram 4G
```

Use `ssh srv status` to check remaining host capacity before creating more.
