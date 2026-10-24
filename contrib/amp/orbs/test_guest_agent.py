import importlib.util
import json
import http.server
import ssl
import os
from pathlib import Path
import re
import shutil
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
                        b'{"action":"bootstrap","token":"bad\\nsecret"}\n',
                        b'{"action":"bootstrap","token":"t","extra":1}\n']:
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

    def test_provider_proxy_bypass_destinations(self):
        # Absent keeps the built-in bypass list; the provider's destination
        # list is appended to it, and malformed values are refused without
        # effects and without consuming a generation.
        with tempfile.TemporaryDirectory() as tmp:
            runtime, unit = Path(tmp) / "runtime", Path(tmp) / "unit"
            seen = {}
            def command(*args):
                if args[1] in {"start", "is-active"}:
                    seen["env"] = (runtime / "bootstrap.env").read_text()
            with patch.object(guest, "RUNTIME", runtime), patch.object(guest, "UNIT", unit), \
                    patch.object(guest, "command", command), patch.object(guest, "stopped"):
                guest.initialize_generation()
                result = self.exchange(json.dumps({"action": "bootstrap", "token": "fresh-secret",
                                                   "proxy": "http://proxy",
                                                   "proxy_bypass": ".example.com,.corp.test",
                                                   "generation": 1}).encode() + b"\n")
            self.assertEqual(json.loads(result), {"ok": True, "generation": 1})
            no_proxy = re.search(r'^NO_PROXY="([^"]*)"$', seen["env"], re.M).group(1)
            # Built-ins stay, each provider suffix matches its own hosts at a
            # label boundary (not glued names), others still use the gateway.
            for host, bypass in [("box.tail1234.ts.net", True), ("a.example.com", True),
                                 ("a.corp.test", True), ("evilexample.com", False),
                                 ("evilcorp.test", False), ("evil-ts.net", False),
                                 ("localhost", True), ("ampcode.com", False)]:
                self.assertEqual(bool(urllib.request.proxy_bypass_environment(host, {"no": no_proxy})), bypass, host)
            for bad in ["a,b,c,d,e,f,g,h,i", ",a", "a,", "a,,b", "a, b", "a b", 'a"b',
                        "a\\b", "a\nb", "a:b", "a/b", "a*b", "a?b", "a#b", "a@b", ".", "..",
                        "-", "_", "*", 5, "x" * 254, ".x.com,.x.com", "localhost", ".ts.net",
                        "100.64.0.0/10", ",".join("e%d.%s" % (i, "x" * 64) for i in range(8))]:
                with patch.object(guest, "RUNTIME", runtime), patch.object(guest, "UNIT", unit), \
                        patch.object(guest, "command", command), patch.object(guest, "stopped"):
                    payload = json.dumps({"action": "bootstrap", "token": "t", "proxy_bypass": bad,
                                          "generation": 2}).encode() + b"\n"
                    self.assertEqual(json.loads(self.exchange(payload)), {"ok": False})
            with patch.object(guest, "RUNTIME", runtime):
                self.assertEqual(guest.read_generation(), {"generation": 1, "phase": "done"})
            # The FQDN trailing-dot form is accepted verbatim; urllib simply
            # does not match it against dotless hostnames (a client fact).
            with patch.object(guest, "RUNTIME", runtime), patch.object(guest, "UNIT", unit), \
                    patch.object(guest, "command", command), patch.object(guest, "stopped"):
                result = self.exchange(json.dumps({"action": "bootstrap", "token": "fresh-secret",
                                                   "proxy_bypass": ".example.com.",
                                                   "generation": 3}).encode() + b"\n")
            self.assertEqual(json.loads(result), {"ok": True, "generation": 3})
            no_proxy = re.search(r'^NO_PROXY="([^"]*)"$', seen["env"], re.M).group(1)
            self.assertTrue(no_proxy.endswith(",.example.com."))
            self.assertFalse(urllib.request.proxy_bypass_environment("a.example.com", {"no": no_proxy}))

    def test_invalid_proxy_bypass_never_consumes_a_generation(self):
        # Validation happens in handle(), before bootstrap(): a malformed
        # list — including one with a valid first entry — is refused like an
        # unknown key, nothing is appended, and the same generation stays
        # usable, with no bootstrap effects at all.
        with tempfile.TemporaryDirectory() as tmp, patch.object(guest, "RUNTIME", Path(tmp)):
            guest.initialize_generation()
            for bad in ["a,b,", ",.good.example", "a,b,c,d,e,f,g,h,i", ".good.example,.good.example"]:
                with patch.object(guest, "apply_bootstrap") as apply:
                    payload = json.dumps({"action": "bootstrap", "token": "t", "proxy_bypass": bad,
                                          "generation": 1}).encode() + b"\n"
                    self.assertEqual(json.loads(self.exchange(payload)), {"ok": False})
                apply.assert_not_called()
            self.assertEqual(guest.read_generation(), {"generation": 0, "phase": "new"})
            with patch.object(guest, "apply_bootstrap"):
                payload = json.dumps({"action": "bootstrap", "token": "t",
                                      "generation": 1}).encode() + b"\n"
                self.assertEqual(json.loads(self.exchange(payload)), {"ok": True, "generation": 1})

    def test_bypass_matches_real_clients(self):
        # Exercise matching through real curl over https:// against loopback
        # listeners only: no DNS, no external network, no assumption about
        # unused ports. The test uses the exact uppercase HTTPS_PROXY and
        # NO_PROXY values a mocked guest bootstrap wrote for the same
        # destination list — the contract the guest actually writes; curl
        # honors both for https (its uppercase-HTTP_PROXY refusal is an
        # http://-only rule and nothing here tests that path). A temporary
        # self-signed fixture cert (generated per run, never committed)
        # serves the direct path, with hosts pinned via --resolve;
        # non-bypassed requests must issue a proxy CONNECT and fail at our
        # accept-and-close listener. Only a verified TLS response that the
        # fixture actually served counts as bypassed, so no unrelated failure
        # (timeout, DNS, TLS) can pass as either outcome.
        if shutil.which("curl") is None or shutil.which("openssl") is None:
            self.skipTest("curl or openssl not installed")
        proxy = socket.socket()
        proxy.bind(("127.0.0.1", 0))
        proxy.listen(8)
        self.addCleanup(proxy.close)
        def refuse():
            while True:
                try:
                    conn, _ = proxy.accept()
                except OSError:
                    return
                conn.close()
        threading.Thread(target=refuse, daemon=True).start()
        proxy_url = f"http://127.0.0.1:{proxy.getsockname()[1]}"
        with tempfile.TemporaryDirectory() as tmp:
            runtime, unit = Path(tmp) / "runtime", Path(tmp) / "unit"
            seen = {}
            def command(*args):
                if args[1] in {"start", "is-active"}:
                    seen["env"] = (runtime / "bootstrap.env").read_text()
            with patch.object(guest, "RUNTIME", runtime), patch.object(guest, "UNIT", unit), \
                    patch.object(guest, "command", command), patch.object(guest, "stopped"):
                guest.initialize_generation()
                result = self.exchange(json.dumps({"action": "bootstrap", "token": "t",
                                                   "proxy": proxy_url,
                                                   "proxy_bypass": ".example.test,.corp.test",
                                                   "generation": 1}).encode() + b"\n")
            self.assertEqual(json.loads(result), {"ok": True, "generation": 1})
            def env_value(key):
                return re.search(rf'^{key}="([^"]*)"$', seen["env"], re.M).group(1)
            guest_env = {key: env_value(key) for key in ["HTTPS_PROXY", "NO_PROXY"]}
        fixtures = tempfile.TemporaryDirectory()
        self.addCleanup(fixtures.cleanup)
        cert = str(Path(fixtures.name) / "fixture.pem")
        key = str(Path(fixtures.name) / "fixture-key.pem")
        hosts = "a.example.test b.corp.test evilexample.test evilcorp.test".split()
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                        "-keyout", key, "-out", cert, "-days", "1", "-subj", "/CN=fixture",
                        "-addext", "subjectAltName=" + ",".join(f"DNS:{h}" for h in hosts)],
                       check=True, timeout=30, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        hits = []
        class direct(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                hits.append(self.headers.get("Host", "").split(":")[0])
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b"ok")
            def log_message(self, *args):
                pass
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), direct)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(cert, key)
        server.socket = context.wrap_socket(server.socket, server_side=True)
        port = server.server_address[1]
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.shutdown)
        self.addCleanup(server.server_close)
        for host, bypass in [("a.example.test", True), ("b.corp.test", True),
                             ("evilexample.test", False), ("evilcorp.test", False)]:
            env = dict(os.environ, **guest_env)
            for inherited in ["HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy",
                              "NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy"]:
                env.pop(inherited, None)
            env.update(guest_env)
            result = subprocess.run(["curl", "-s", "-o", os.devnull, "--max-time", "5",
                                     "--cacert", cert,
                                     "--resolve", f"{host}:{port}:127.0.0.1",
                                     f"https://{host}:{port}/"], env=env, timeout=15)
            served = host in hits
            if bypass:
                self.assertTrue(result.returncode == 0 and served,
                                f"{host} not bypassed (curl exit {result.returncode}, served={served})")
            else:
                self.assertTrue(result.returncode in (52, 56) and not served,
                                f"{host} unexpectedly bypassed or ambiguous (curl exit {result.returncode}, served={served})")

    def test_failed_start_is_cancelled_before_acknowledgment(self):
        # A systemctl client timeout leaves PID1's start job queued. The
        # failure ack must follow credential removal and a proven stop; if the
        # stop fails, the generation must stay uncertain, never done.
        for cancel_fails in [False, True]:
            with tempfile.TemporaryDirectory() as tmp:
                runtime = Path(tmp) / "runtime"
                events = []
                def command(*args):
                    started = any(e[0] == "start" for e in events)
                    events.append((args[1], (runtime / "bootstrap.env").exists()))
                    if args[1] == "start":
                        raise subprocess.TimeoutExpired(args, 15)
                    if args[1] == "stop" and started and cancel_fails:
                        raise subprocess.TimeoutExpired(args, 15)
                def stopped():
                    events.append(("stopped", (runtime / "bootstrap.env").exists()))
                with patch.object(guest, "RUNTIME", runtime), patch.object(guest, "UNIT", Path(tmp) / "unit.service"), \
                        patch.object(guest, "command", command), patch.object(guest, "stopped", stopped):
                    guest.initialize_generation()
                    request = lambda g: json.dumps({"action": "bootstrap", "token": "t", "generation": g}).encode() + b"\n"
                    self.assertEqual(json.loads(self.exchange(request(1))), {"ok": False})
                    after = events[[e[0] for e in events].index("start") + 1:]
                    self.assertEqual(after[:1], [("stop", False)])
                    self.assertEqual(after[1:], [] if cancel_fails else [("stopped", False)])
                    self.assertFalse((runtime / "bootstrap.env").exists())
                    self.assertEqual(guest.read_generation(), {"generation": 1, "phase": "executing"})
                    self.assertEqual(json.loads(self.exchange(request(1))), {"ok": False})
                    with patch.object(guest, "command", lambda *args: None):
                        self.assertEqual(json.loads(self.exchange(request(2))), {"ok": True, "generation": 2})
                    self.assertEqual(guest.read_generation(), {"generation": 2, "phase": "done"})

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
