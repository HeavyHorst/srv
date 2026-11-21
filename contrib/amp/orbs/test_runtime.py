"""Run offline integration checks with AMP_RUNTIME_LAYER=/path/to/pinned.tar.gz."""
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("runtime", Path(__file__).with_name("install-runtime.py"))
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)


class RuntimeTests(unittest.TestCase):
    def test_checksum_failure_does_not_modify_root(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "root"
            layer = Path(tmp) / "bad.tar.gz"
            layer.write_bytes(b"invalid upstream content")
            result = subprocess.run(["python3", str(Path(runtime.__file__)), str(root),
                                     "--layer", str(layer)], capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(b"checksum mismatch", result.stderr)
            self.assertFalse(root.exists())

    @unittest.skipUnless(os.environ.get("AMP_RUNTIME_LAYER"), "set AMP_RUNTIME_LAYER to test the real pinned layer")
    def test_real_layer_install_and_environment_refresh(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            layer = Path(os.environ["AMP_RUNTIME_LAYER"]).read_bytes()
            runtime.install(root, layer)
            runtime.install(root, layer)
            expected = {
                "etc/systemd/system/amp.slice", "etc/systemd/system/amp-control.slice",
                "etc/systemd/system/amp-workload.slice", "etc/systemd/system/amp-headless.service",
                "etc/systemd/system/amp-headless-env.path", "etc/systemd/system/amp-headless-env.service",
                "etc/systemd/system/multi-user.target.wants/amp-headless-env.path",
                "etc/sudoers.d/amp-systemd-run", "usr/local/bin/amp-headless-update-binary",
                "usr/local/bin/amp-headless-apply-env", "usr/local/bin/amp-run-workload",
                "usr/local/share/srv/amp-runtime-layer.sha256",
            }
            self.assertEqual({str(p.relative_to(root)) for p in root.rglob("*") if p.is_file()}, expected)
            self.assertEqual((root / "etc/sudoers.d/amp-systemd-run").stat().st_mode & 0o777, 0o440)
            for name in ["amp-run-workload", "amp-headless-apply-env", "amp-headless-update-binary"]:
                self.assertTrue(os.access(root / "usr/local/bin" / name, os.X_OK))
            link = root / "etc/systemd/system/multi-user.target.wants/amp-headless-env.path"
            self.assertEqual(link.readlink(), Path("../amp-headless-env.path"))
            self.assertEqual((root / "usr/local/share/srv/amp-runtime-layer.sha256").read_text(), runtime.LAYER_SHA256 + "\n")

            events = root / "events"
            systemctl = root / "systemctl"
            systemctl.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$EVENTS"\n')
            systemctl.chmod(0o755)
            env_file = root / "headless.env"
            staged = root / "staged.env"
            applied = root / "applied.sha256"
            env = dict(os.environ, AMP_HEADLESS_ENV_FILE=str(env_file),
                       AMP_HEADLESS_APPLIED_HASH_FILE=str(applied),
                       AMP_HEADLESS_ENV_LOCK_FILE=str(root / "env.lock"),
                       AMP_HEADLESS_SYSTEMCTL=str(systemctl), AMP_HEADLESS_CHOWN="true",
                       EVENTS=str(events))
            helper = root / "usr/local/bin/amp-headless-apply-env"
            staged.write_text("TOKEN=synthetic-one\n")
            subprocess.run(["bash", str(helper), "--stage-only", str(staged)], env=env, check=True)
            self.assertEqual(env_file.read_text(), "TOKEN=synthetic-one\n")
            self.assertFalse(events.exists())
            self.assertFalse(applied.exists())
            for _ in range(2):
                subprocess.run(["bash", str(helper)], env=env, check=True)
            self.assertEqual(events.read_text().splitlines().count("restart amp-headless.service"), 1)
            staged.write_text("TOKEN=synthetic-two\n")
            subprocess.run(["bash", str(helper), "--stage-only", str(staged)], env=env, check=True)
            self.assertEqual(events.read_text().splitlines().count("restart amp-headless.service"), 1)
            subprocess.run(["bash", str(helper)], env=env, check=True)
            self.assertEqual(events.read_text().splitlines().count("restart amp-headless.service"), 2)


if __name__ == "__main__":
    unittest.main()
