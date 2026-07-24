# srv Amp plugin

`srv-runner.ts` is the source for the Amp plugin that provisions isolated srv VMs and starts dedicated Amp runners in them.

Install it globally so it is available while working in any repository:

```bash
mkdir -p ~/.config/amp/plugins
ln -sf "$PWD/contrib/amp/plugins/srv-runner.ts" ~/.config/amp/plugins/srv-runner.ts
```

Run the command palette action `plugins: reload` after installing or updating it.

New isolated VMs receive the configured personal Amp skills, the `nrc`, `sourcebot`, `planner`, and `zoho` CLIs, and their root-only NRC, Planner, and Zoho credentials before the Amp runner starts. Provisioning dereferences local skill symlinks so the guest receives complete skill directories rather than workstation-specific links. The creation confirmation lists the credentials copied into the persistent VM.

Use `srv Runner: Push current VM branch…` from a managed thread to push its current commit. The command asks for confirmation, forwards the local SSH agent only for the push, and closes the forwarding connection after at most two minutes.
