import importlib.util
import json
from pathlib import Path
import re
import socket
import subprocess
import tempfile
import threading
import unittest
import urllib.request
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("guest", Path(__file__).with_name("guest-agent.py"))
guest = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guest)


class GuestTests(unittest.TestCase):
    def exchange(self, payload, cid=2):
        client, server = socket.socketpair()
        def run():
            with server:
                guest.handle(server, cid)
        thread = threading.Thread(target=run)
        thread.start()
        with client:
            try:
                client.sendall(payload)
                client.shutdown(socket.SHUT_WR)
                result = client.recv(1024)
            except (ConnectionResetError, BrokenPipeError):
                result = b""
        thread.join()
        return result

    def test_ping_and_rejection(self):
        self.assertEqual(self.exchange(b'{"action":"ping"}\n'), b'{"ok":true}\n')
        for payload in [b'{}\n', b'{"action":"exec","command":"id"}\n',
                        b'not json\n', b'{"action":"ping"}', b'x' * 65537 + b'\n',
                        b'{"action":"bootstrap","token":"bad\\nsecret"}\n']:
            self.assertEqual(self.exchange(payload), b'{"ok":false}\n')
        self.assertEqual(self.exchange(b'{"action":"ping"}\n', 3), b'')

    def test_credentials_and_cleanup(self):
        with tempfile.TemporaryDirectory() as tmp:
            runtime = Path(tmp) / "runtime"
            unit = Path(tmp) / "bootstrap.service"
            for fail in [False, True]:
                calls = []
                def command(*args):
                    calls.append(args)
                    self.assertNotIn("fresh-secret", repr(args))
                    env = runtime / "bootstrap.env"
                    self.assertEqual(runtime.stat().st_mode & 0o777, 0o700)
                    if args[1] in {"start", "is-active"}:
                        self.assertEqual(env.stat().st_mode & 0o777, 0o600)
                        self.assertIn('ORB_TOKEN="fresh-secret"', env.read_text())
                        self.assertIn('HTTPS_PROXY="http://proxy"', env.read_text())
                        no_proxy = re.search(r'^NO_PROXY="([^"]*)"$', env.read_text(), re.M).group(1)
                        # Tailnet names bypass the public-only gateway; others still use it.
                        for host, bypass in [("box.tail1234.ts.net", True), ("localhost", True),
                                             ("ampcode.com", False), ("evil-ts.net", False)]:
                            self.assertEqual(bool(urllib.request.proxy_bypass_environment(host, {"no": no_proxy})), bypass, host)
                        if fail:
                            raise subprocess.CalledProcessError(1, args)
                    else:
                        self.assertFalse(env.exists())
                with patch.object(guest, "RUNTIME", runtime), patch.object(guest, "UNIT", unit), patch.object(guest, "command", command), patch.object(guest, "stopped"):
                    guest.initialize_generation()
                    generation = 2 if fail else 1
                    result = self.exchange(json.dumps({"action": "bootstrap", "token": "fresh-secret", "proxy": "http://proxy", "generation": generation}).encode() + b'\n')
                    self.assertEqual(guest.read_generation()["phase"], "executing" if fail else "done")
                self.assertEqual(json.loads(result), {"ok": False} if fail else {"ok": True, "generation": generation})
                self.assertFalse((runtime / "bootstrap.env").exists())
                self.assertNotIn("fresh-secret", unit.read_text())
                self.assertIn("User=user", unit.read_text())
                self.assertIn("KillMode=control-group", unit.read_text())
                self.assertIn("EnvironmentFile=-/etc/amp-orb.env", unit.read_text())
                self.assertTrue(calls)

    def test_generations_serialize_stale_work_and_survive_agent_restart(self):
        for order in [(1, 2, 1), (2, 1, 2)]:
            with tempfile.TemporaryDirectory() as tmp, patch.object(guest, "RUNTIME", Path(tmp)):
                guest.initialize_generation()
                applied = []
                with patch.object(guest, "apply_bootstrap", lambda request: applied.append(request["token"])):
                    for generation in order:
                        result = json.loads(self.exchange(json.dumps({"action": "bootstrap", "token": f"token-{generation}", "generation": generation}).encode() + b'\n'))
                        guest.initialize_generation()  # Agent restart must not reset it.
                self.assertEqual(applied, ["token-1", "token-2"] if order[0] == 1 else ["token-2"])
                self.assertEqual(result, {"ok": False})
                self.assertEqual(guest.read_generation(), {"generation": 2, "phase": "done"})

    def test_crash_windows_never_turn_consumption_into_success(self):
        for boundary in ["before-save", "after-save", "effect", "after-effect"]:
            with tempfile.TemporaryDirectory() as tmp, patch.object(guest, "RUNTIME", Path(tmp)):
                guest.initialize_generation()
                original = guest.save_generation
                effects = []
                def save(state):
                    if boundary == "before-save":
                        raise OSError("injected fsync failure")
                    if boundary == "after-effect" and state["phase"] == "done":
                        raise OSError("injected terminal commit failure")
                    original(state)
                    if boundary == "after-save":
                        raise OSError("injected crash after consumption")
                def effect(request):
                    effects.append(request["generation"])
                    if boundary == "effect":
                        raise OSError("injected effect failure")
                with patch.object(guest, "save_generation", save), patch.object(guest, "apply_bootstrap", effect):
                    result = self.exchange(b'{"action":"bootstrap","token":"old","generation":1}\n')
                self.assertEqual(json.loads(result), {"ok": False})
                self.assertNotEqual(guest.read_generation()["phase"], "done")
                guest.initialize_generation()
                with patch.object(guest, "apply_bootstrap", lambda request: effects.append(request["generation"])):
                    result = self.exchange(b'{"action":"bootstrap","token":"new","generation":2}\n')
                self.assertEqual(json.loads(result), {"ok": True, "generation": 2})
                self.assertEqual(guest.read_generation()["phase"], "done")
                self.assertEqual(effects[-1], 2)

    def test_missing_state_and_unfinished_stop_fail_closed(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(guest, "RUNTIME", Path(tmp)), patch.object(guest, "UNIT", Path(tmp) / "unit"):
            guest.initialize_generation()
            with patch.object(guest, "command"), patch.object(guest, "stopped", side_effect=RuntimeError("old process alive")):
                result = self.exchange(b'{"action":"bootstrap","token":"new","generation":1}\n')
            self.assertEqual(json.loads(result), {"ok": False})
            self.assertFalse((Path(tmp) / "bootstrap.env").exists())
            (Path(tmp) / "generation.json").unlink()
            with self.assertRaises(FileNotFoundError):
                guest.initialize_generation()
            self.assertEqual(json.loads(self.exchange(b'{"action":"bootstrap","token":"new","generation":2}\n')), {"ok": False})

    def test_command_does_not_capture_output(self):
        # Real child process invocation, without systemd or root privileges.
        guest.command("/bin/true")
        with self.assertRaises(subprocess.CalledProcessError):
            guest.command("/bin/false")


if __name__ == "__main__":
    unittest.main()
