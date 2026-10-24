#!/usr/bin/python3
"""Disposable root-only systemd probe. No srv, Amp, network or real credentials.

Run: sudo python3 contrib/amp/orbs/probe_guest_systemd.py
Exercises the real handler and systemd with a unique temporary unit. The Unix
socketpair replaces only transport; this is not a Firecracker or Amp test.
"""
import importlib.util
import json
import multiprocessing
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import uuid

spec = importlib.util.spec_from_file_location("guest", Path(__file__).with_name("guest-agent.py"))
guest = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guest)


def wait_for(condition):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        if condition():
            return
        time.sleep(.02)
    raise AssertionError("condition did not complete")


def request(generation):
    return json.dumps({"action": "bootstrap", "token": f"synthetic-{generation}",
                       "generation": generation}).encode() + b"\n"


def main():
    assert os.geteuid() == 0, "requires root in a disposable systemd environment"
    assert Path("/proc/1/comm").read_text().strip() == "systemd"
    name = "srv-orb-probe-" + uuid.uuid4().hex + ".service"
    unit = Path("/run/systemd/system") / name
    with tempfile.TemporaryDirectory(prefix="srv-orb-probe-", dir="/run") as tmp:
        root = Path(tmp)
        guest.RUNTIME, guest.UNIT, guest.UNIT_NAME = root, unit, name
        script = root / "standin.py"
        script.write_text('''import json, os, signal, time
from pathlib import Path
root = Path(__file__).parent
child = os.fork()
if child == 0:
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
else:
    (root / "processes.json").write_text(json.dumps({"pids": [os.getpid(), child], "token": os.environ["ORB_TOKEN"]}))
while True: time.sleep(1)
''')
        gate = root / "gate"
        guest.BOOTSTRAP_UNIT = (guest.BOOTSTRAP_UNIT.replace("User=user", "User=root")
            .replace("Group=user", "Group=root").replace("WorkingDirectory=/home/user", f"WorkingDirectory={root}")
            .replace("EnvironmentFile=-/etc/amp-orb.env\n", "")
            .replace("/run/srv-orb/bootstrap.env", str(root / "bootstrap.env"))
            .replace("ExecStart=/usr/local/bin/amp orb bootstrap", f"ExecStartPre=/bin/sh -c 'while test -e {gate}; do sleep 0.1; done'\nExecStart=/usr/bin/python3 {script}")
            .replace("TimeoutStopSec=10", "TimeoutStopSec=1"))
        def exchange(generation, lose_reply=False):
            client, server = socket.socketpair()
            client.sendall(request(generation))
            if lose_reply:
                client.close()
            with server:
                guest.handle(server, 2)
            if lose_reply:
                return
            with client:
                return json.loads(client.recv(1024))

        def processes(generation):
            path = root / "processes.json"
            def applied():
                try:
                    return json.loads(path.read_text())["token"] == f"synthetic-{generation}"
                except (FileNotFoundError, json.JSONDecodeError):
                    return False
            wait_for(applied)
            value = json.loads(path.read_text())
            for pid in value["pids"]:
                assert f"ORB_TOKEN=synthetic-{generation}".encode() in Path(f"/proc/{pid}/environ").read_bytes()
            assert not (root / "bootstrap.env").exists()
            return value["pids"]

        def assert_gone(pids):
            for pid in pids:
                path = Path(f"/proc/{pid}/stat")
                assert not path.exists() or path.read_text().split()[2] == "Z", "old credential-holding process survived"

        child = None
        try:
            guest.initialize_generation()
            assert exchange(1) == {"ok": True, "generation": 1}
            old = processes(1)
            assert exchange(2) == {"ok": True, "generation": 2}
            processes(2)
            assert_gone(old)
            print("PASS old-before-new: cgroup-wide stop kills token-holding descendants")

            # Bytes from the old connection exist before the newer request but
            # are serviced afterwards, as with a vsock accept backlog.
            late_client, late_server = socket.socketpair()
            late_client.sendall(request(1))
            exchange(3, lose_reply=True)
            newest = processes(3)
            guest.initialize_generation()  # Agent restart, same guest boot.
            with late_server:
                guest.handle(late_server, 2)
            with late_client:
                assert json.loads(late_client.recv(1024)) == {"ok": False}
            assert processes(3) == newest
            assert guest.read_generation() == {"generation": 3, "phase": "done"}
            print("PASS lost acknowledgment, agent restart and newer-before-buffered-old: no replay")

            # Kill the handler while PID1 owns an unfinished start job. Its
            # finally block cannot unlink the old environment file.
            gate.touch()
            client, server = socket.socketpair()
            child = multiprocessing.get_context("fork").Process(target=guest.handle, args=(server, 2))
            child.start()
            server.close()
            client.sendall(request(4))
            def queued():
                value = subprocess.check_output(["systemctl", "show", "-p", "ActiveState", "-p", "Job", name], text=True)
                return "ActiveState=activating" in value and "Job=0" not in value and "Job=\n" not in value
            wait_for(queued)
            child.kill()
            child.join(5)
            client.close()
            assert (root / "bootstrap.env").exists()
            assert guest.read_generation() == {"generation": 4, "phase": "executing"}
            original = guest.stopped
            def stopped():
                original()  # Real inactive/no-job/empty-cgroup verification.
                gate.unlink(missing_ok=True)
            guest.stopped = stopped
            assert exchange(5) == {"ok": True, "generation": 5}
            processes(5)
            assert_gone(newest)
            assert exchange(4) == {"ok": False}
            print("PASS killed handler with orphan systemd job: newer stop cancels job before replacing credentials")
        finally:
            if child is not None and child.is_alive():
                child.kill()
                child.join(5)
            subprocess.run(["systemctl", "kill", "--kill-whom=all", "--signal=KILL", name],
                           check=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(["systemctl", "stop", name], check=False, stdout=subprocess.DEVNULL)
            unit.unlink(missing_ok=True)
            subprocess.run(["systemctl", "daemon-reload"], check=True)
            subprocess.run(["systemctl", "reset-failed", name], check=False, stderr=subprocess.DEVNULL)


if __name__ == "__main__":
    main()
