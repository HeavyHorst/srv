#!/usr/bin/env python3
"""Mutation audit for the lifecycle guards.

Each mutant disables one durable guard in a disposable copy of the worktree.
A healthy unmodified baseline is required first. The focused tests must then
fail with a test assertion, not a build error, setup error, or timeout; a
surviving mutant also fails the audit. The original worktree is never modified.
"""

from __future__ import annotations

import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]

MUTANTS = [
    {
        "name": "cleanup-fence-delete",
        "file": "internal/store/cleanup.go",
        "old": "DELETE FROM cleanup_fences WHERE instance_id=? AND request_id=?",
        "new": "SELECT 1 WHERE 0",
        "package": "./internal/store",
        "run": "TestLifecycleStateMachine|TestCrashConsistency",
    },
    {
        "name": "cleanup-deleting-state",
        "file": "internal/store/cleanup.go",
        "old": "AND state='deleting' AND (kind='vm' OR NOT EXISTS",
        "new": "AND state IN ('deleting','ready') AND (kind='vm' OR NOT EXISTS",
        "package": "./internal/store",
        "run": "TestLifecycleStateMachine|TestCrashConsistency|TestCleanupCommitRejectsReadyInstanceWithFence",
    },
    {
        "name": "admit-rebinding",
        "file": "internal/store/requests.go",
        "old": "if old.Command != r.Command || old.Name != r.Name || old.Arguments != r.Arguments {",
        "new": "if false && (old.Command != r.Command || old.Name != r.Name || old.Arguments != r.Arguments) {",
        "package": "./internal/store",
        "run": "TestLifecycleStateMachine",
    },
    {
        "name": "begin-request-phase",
        "file": "internal/store/requests.go",
        "old": "UPDATE lifecycle_requests SET phase='executing' WHERE id=? AND actor=? AND phase='admitted'",
        "new": "UPDATE lifecycle_requests SET phase='executing' WHERE id=? AND actor=?",
        "package": "./internal/store",
        "run": "TestLifecycleStateMachine",
    },
    {
        "name": "reserve-collision-guard",
        "file": "internal/store/migration.go",
        "old": '''if n != 1 {
		return fmt.Errorf("import instance ID %q already exists", r.InstanceID)
	}''',
        "new": '''if false && n != 1 {
		return fmt.Errorf("import instance ID %q already exists", r.InstanceID)
	}''',
        "package": "./internal/store",
        "run": "TestLifecycleStateMachine",
    },
    {
        "name": "finalize-receipt-phase",
        "file": "internal/store/migration.go",
        "old": '''	result, err = tx.ExecContext(ctx, `UPDATE import_receipts SET phase = 'imported' WHERE instance_id = ? AND phase = 'importing'`, instanceID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("import receipt missing: %v", err)
	}
''',
        "new": "",
        "package": "./internal/store",
        "run": "TestLifecycleStateMachine",
    },
    {
        "name": "policy-delete-migration",
        "file": "internal/provision/cleanup.go",
        "old": 'if err == nil && !moved && migration.Phase != "aborted" {',
        "new": 'if false && err == nil && !moved && migration.Phase != "aborted" {',
        "package": "./internal/provision",
        "run": "TestProvisionPolicyMatrix",
    },
    {
        "name": "policy-prepare-import-fence",
        "file": "internal/provision/migration.go",
        "old": '''	if err == nil && r.Phase != "imported" {
		return fmt.Errorf("instance %q is fenced by incomplete import", name)
	}
''',
        "new": '''	if false && err == nil && r.Phase != "imported" {
		return fmt.Errorf("instance %q is fenced by incomplete import", name)
	}
''',
        "package": "./internal/provision",
        "run": "TestProvisionPolicyMatrix",
    },
]


def copy_worktree(destination: Path) -> None:
    shutil.copytree(
        ROOT,
        destination,
        ignore=shutil.ignore_patterns(".git", ".amp", "dist", ".hegel", "testdata"),
    )


def run_focused(worktree: Path, package: str, run: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["go", "test", "-count=1", "-timeout=180s", package, "-run", run],
        cwd=worktree,
        capture_output=True,
        text=True,
        timeout=240,
    )


def main() -> int:
    failures: list[str] = []
    with tempfile.TemporaryDirectory(prefix="srv-mutation-audit-") as tmp:
        worktree = Path(tmp) / "repo"
        copy_worktree(worktree)

        # A failing baseline would make every mutant look detected.
        baselines: dict[tuple[str, str], bool] = {}
        for mutant in MUTANTS:
            key = (mutant["package"], mutant["run"])
            if key in baselines:
                continue
            try:
                result = run_focused(worktree, *key)
            except subprocess.TimeoutExpired:
                failures.append(f"baseline {key[0]} -run {key[1]}: timed out")
                baselines[key] = False
                continue
            output = result.stdout + result.stderr
            baselines[key] = result.returncode == 0
            if not baselines[key]:
                failures.append(f"baseline {key[0]} -run {key[1]} failed:\n{output[-2000:]}")
        if failures:
            print("\nMutation audit failed:", file=sys.stderr)
            for failure in failures:
                print(f"  - {failure}", file=sys.stderr)
            return 1

        for mutant in MUTANTS:
            path = worktree / mutant["file"]
            original = path.read_text()
            if mutant["old"] not in original:
                failures.append(f"{mutant['name']}: anchor not found")
                continue
            path.write_text(original.replace(mutant["old"], mutant["new"], 1))
            try:
                result = run_focused(worktree, mutant["package"], mutant["run"])
            except subprocess.TimeoutExpired:
                failures.append(f"{mutant['name']}: timed out")
                continue
            finally:
                path.write_text(original)
            output = result.stdout + result.stderr
            if result.returncode == 0:
                failures.append(f"{mutant['name']}: survived")
            elif result.returncode < 0:
                failures.append(f"{mutant['name']}: terminated by signal {-result.returncode}")
            elif "build failed" in output:
                failures.append(f"{mutant['name']}: build failed")
            elif "setup failed" in output:
                failures.append(f"{mutant['name']}: test setup failed")
            elif "test timed out" in output:
                failures.append(f"{mutant['name']}: test timed out")
            elif "--- FAIL:" not in output:
                failures.append(f"{mutant['name']}: failed without a focused test failure:\n{output[-2000:]}")
            else:
                print(f"detected: {mutant['name']}")

    if failures:
        print("\nMutation audit failed:", file=sys.stderr)
        for failure in failures:
            print(f"  - {failure}", file=sys.stderr)
        return 1
    print(f"\nAll {len(MUTANTS)} mutants detected.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
