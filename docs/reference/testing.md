# Testing

srv separates fast correctness checks, generated lifecycle models, process-crash
matrices, fuzzing, and mutation checks. Use the narrowest tier that exercises the
changed boundary.

## Fast

```bash
go test ./...
go vet ./...
gofmt -l cmd internal
```

The default suite covers the store, provision, service, vmrunner, nethelper,
config, front, and CLI packages. It needs no root, no KVM, and no network.

## Race

```bash
GORACE=atexit_sleep_ms=0 go test -race -count=1 -timeout=15m ./...
```

`cmd/srv-orb-dispatch` re-execs the race-instrumented test binary for every fake
SSH call. Disabling the race runtime's atexit sleep keeps that harness inside the
package timeout; without it the package can exceed the default limit.

## Generated lifecycle model

```bash
go test ./internal/store -run '^TestLifecycleStateMachine$' -count=1
```

Hegel generates up to 15 operations per case over a fresh SQLite store: create,
admit fresh/same/changed requests, begin/finish start requests, begin/commit
cleanup, reserve/fresh/existing/collision imports, create an import instance,
finalize, and interrupt. An independent model tracks instance state, request
binding/phase/exit, fence, cleanup receipt, and import receipt. Eligible IDs are
sorted before drawing so replay and shrinking act on the same rows.

The model is sequential and store-level. It does not cover provision/service
admission policy, migration, concurrency, or crash behavior. Set
`HEGEL_STATISTICS=1` to print rule-event counts.

## Provision policy matrix

```bash
go test ./internal/provision -run '^TestProvisionPolicyMatrix$' -count=1
```

Exhaustively combines migration phase (none/preparing/prepared/completed/aborted),
import receipt (none/importing/imported), and cleanup fence (none/fenced). It
asserts `PrepareMigration` and `DeleteWithProof` outcomes, file removal,
tombstones, and receipts. Interrupted `preparing` retries are exercised with the
matching token/target and with a changed binding. The preparing/prepared/completed
plus importing combinations are unreachable through the public import path and are
skipped.

## Process-crash matrices

- `internal/store`: SIGKILL at every scripted lifecycle boundary, reopen, and
  exact durable-prefix checks.
- `internal/vmrunner`: SIGKILL while holding the name lock, SIGKILL after the
  accepted receipt is durable, receipt-scan recovery, and a started receipt whose
  runtime cannot be verified.
- `internal/nethelper`: setup/cleanup failures at every command, retry
  idempotence, missing-state tolerance, and existing-rule skipping.
- `internal/provision`: import staging/commit-failure, cleanup admission window,
  restore retry after a partial replacement, and missing-rootfs start refusal.

These cover process crashes, not power-loss durability or fsync ordering.

## Fuzzing

```bash
go test -run='^$' -fuzz='^FuzzSetupHandler$' -fuzztime=20s ./internal/nethelper
```

Targets:

| Package | Targets |
| --- | --- |
| `internal/provision` | portable manifest validation, portable TAR parsing |
| `internal/nethelper` | setup HTTP handler |
| `internal/storage` | mountinfo line parsing, mountinfo unescaping |
| `internal/service` | command request, import options, size, integration gateway path |
| `internal/config` | provider base URL, guest auth tags, machine shape |
| `internal/vmrunner` | start-result validation, start-response validation |

A returned error is a normal result only when the target's contract expects one;
a panic, hang, or violated contract fails. The committed seeds run in the normal
`go test` tier. CI runs a 20-second campaign for nine high-value targets.

## Mutation audit

```bash
python3 test/mutation_audit.py
```

The audit copies the worktree, verifies the unmodified focused tests pass, then
disables one durable guard at a time. Each mutant must fail with a focused test
assertion; a surviving mutant, missing anchor, build/setup failure, or timeout
fails the audit. It covers cleanup fence deletion, deleting-state commit, request
rebinding, begin-phase gating, reserve collision, finalize receipt publication,
and the migration import-fence policy guard. The original worktree is not
modified.

## Benchmarks

```bash
make bench
```

Benchmarks cover store reads/list/admission and service parsing. They are not a CI
gate; run them when changing those paths.

## Known limits

- Process-crash tests do not model loss of kernel-cached writes or fsync ordering.
- The nethelper tests use a recording runner; real iptables/tap behavior still
  needs a disposable host.
- The Hegel model is store-level and sequential.
- `TestDeleteProofQuiescesGatewaysBeforeReceipt/late-dial` validates an implicit
  HTTP response only on the pending CONNECT path; established tunnels must close
  without any bytes.
