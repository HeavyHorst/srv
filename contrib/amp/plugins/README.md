# srv Amp plugin

`srv-runner.ts` is the source for the Amp plugin that provisions isolated srv VMs and starts dedicated Amp runners in them.

Install it globally so it is available while working in any repository:

```bash
mkdir -p ~/.config/amp/plugins
ln -sf "$PWD/contrib/amp/plugins/srv-runner.ts" ~/.config/amp/plugins/srv-runner.ts
```

Run the command palette action `plugins: reload` after installing or updating it.

Use `srv Runner: Push current VM branch…` from a managed thread to push its current commit. The command asks for confirmation, forwards the local SSH agent only for the push, and closes the forwarding connection after at most two minutes.
