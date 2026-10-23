import importlib.util
import json
from pathlib import Path
import socket
import subprocess
import tempfile
import threading
import unittest
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
                    self.assertEqual(env.stat().st_mode & 0o777, 0o600)
                    self.assertEqual(runtime.stat().st_mode & 0o777, 0o700)
                    self.assertIn('ORB_TOKEN="fresh-secret"', env.read_text())
                    self.assertIn('HTTPS_PROXY="http://proxy"', env.read_text())
                    if fail:
                        raise subprocess.CalledProcessError(1, args)
                with patch.object(guest, "RUNTIME", runtime), patch.object(guest, "UNIT", unit), patch.object(guest, "command", command):
                    result = self.exchange(json.dumps({"action": "bootstrap", "token": "fresh-secret", "proxy": "http://proxy"}).encode() + b'\n')
                self.assertEqual(result, b'{"ok":false}\n' if fail else b'{"ok":true}\n')
                self.assertFalse((runtime / "bootstrap.env").exists())
                self.assertNotIn("fresh-secret", unit.read_text())
                self.assertIn("User=user", unit.read_text())
                self.assertIn("EnvironmentFile=-/etc/amp-orb.env", unit.read_text())
                self.assertTrue(calls)

    def test_command_does_not_capture_output(self):
        # Real child process invocation, without systemd or root privileges.
        guest.command("/bin/true")
        with self.assertRaises(subprocess.CalledProcessError):
            guest.command("/bin/false")


if __name__ == "__main__":
    unittest.main()
