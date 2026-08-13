# srv Amp plugin

`srv-runner.ts` is the source for the Amp plugin that provisions isolated srv VMs and starts dedicated Amp runners in them.

Install it globally so it is available while working in any repository:

```bash
mkdir -p ~/.config/amp/plugins
ln -sf "$PWD/contrib/amp/plugins/srv-runner.ts" ~/.config/amp/plugins/srv-runner.ts
```

Run the command palette action `plugins: reload` after installing or updating it.

The plugin always copies the Amp credential needed by the dedicated runner. An optional workstation-wide profile at `~/.config/amp/srv-runner.json` can also copy the skills, binaries, private files, guest symlinks, and `NO_PROXY` entries you want in every VM. Without that file, no optional defaults are copied.

Start with the included example, which contains the full profile this plugin originally used:

```bash
cp contrib/amp/plugins/srv-runner.example.json ~/.config/amp/srv-runner.json
```

Edit it for the tools available on your workstation. Skill entries without a slash resolve under `~/.agents/skills`; they can also be absolute paths or start with `~/`. Binary and file sources must be absolute or start with `~/`, and their targets must be absolute guest paths. Binaries default to mode `0755`; files default to `0600` and may specify an octal `mode`. Symlink entries use `target` as the existing guest path and `link` as the link to create. `noProxy` entries are appended to the runner service's `NO_PROXY` environment.

The plugin validates every configured source before creating a VM, dereferences skill-directory symlinks while copying, and shows profile item counts in the creation confirmation. Repository-specific setup still belongs in the repository's executable `.agents/setup` script, which runs after cloning.

Use `srv Runner: Push current VM branch…` from a managed thread to push its current commit. The command asks for confirmation, forwards the local SSH agent only for the push, and closes the forwarding connection after at most two minutes.

See [Amp runners on srv](../../../docs/examples/amp-runners.md) for the feature overview and [Sandboxed AI coding agent](../../../docs/examples/ai-coding-agent.md#run-amp-on-a-dedicated-vm-runner) for prerequisites, provisioning behavior, lifecycle commands, and the branch-push security model.
