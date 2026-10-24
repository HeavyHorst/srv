#!/usr/bin/python3
"""Narrow host-CID-only Orb bootstrap agent; no shell/exec RPC."""
import json
import fcntl
import os
import socket
import subprocess
import sys
import time
import tempfile
from pathlib import Path

PORT = 4050
MAX_MESSAGE = 65536
RUNTIME = Path("/run/srv-orb")
UNIT = Path("/run/systemd/system/srv-orb-bootstrap.service")
UNIT_NAME = UNIT.name
BOOTSTRAP_UNIT = """[Unit]
Description=Amp orb bootstrap
After=network-online.target
[Service]
Type=exec
User=user
Group=user
WorkingDirectory=/home/user
Environment=HOME=/home/user USER=user
EnvironmentFile=-/etc/amp-orb.env
EnvironmentFile=/run/srv-orb/bootstrap.env
ExecStart=/usr/local/bin/amp orb bootstrap
KillMode=control-group
TimeoutStopSec=10
LimitCORE=0
StandardOutput=null
StandardError=null
"""


def command(*args):
    subprocess.run(args, check=True, timeout=15, stdin=subprocess.DEVNULL,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def save_generation(state):
    fd, name = tempfile.mkstemp(prefix=".generation-", dir=RUNTIME)
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(state, stream)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, RUNTIME / "generation.json")
        directory = os.open(RUNTIME, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        Path(name).unlink(missing_ok=True)


def initialize_generation():
    # /run belongs to this boot, not to the image or a project template. A
    # service restart keeps both files. Missing state after initialization is
    # an error, never an implicit counter reset.
    RUNTIME.mkdir(mode=0o700, exist_ok=True)
    try:
        fd = os.open(RUNTIME / "generation.lock", os.O_CREAT | os.O_EXCL | os.O_RDWR, 0o600)
    except FileExistsError:
        with (RUNTIME / "generation.lock").open("rb") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            read_generation()
        return
    with os.fdopen(fd, "w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if (RUNTIME / "generation.json").exists():
            raise RuntimeError("generation lock lost; refusing reset")
        save_generation({"generation": 0, "phase": "new"})


def read_generation():
    state = json.loads((RUNTIME / "generation.json").read_text())
    if (set(state) != {"generation", "phase"} or type(state["generation"]) is not int
            or state["generation"] < 0 or state["phase"] not in {"new", "executing", "done"}):
        raise ValueError("invalid generation state")
    return state


def stopped():
    raw = subprocess.check_output(["systemctl", "show", "--property=ActiveState",
                                   "--property=Job", "--property=ControlGroup",
                                   UNIT_NAME], timeout=15, text=True)
    state = dict(line.split("=", 1) for line in raw.splitlines())
    if set(state) != {"ActiveState", "Job", "ControlGroup"} or state["ActiveState"] not in {"inactive", "failed"} or state["Job"]:
        raise RuntimeError("bootstrap unit not quiescent")
    group = state["ControlGroup"]
    if group:
        if not group.startswith("/") or ".." in Path(group).parts:
            raise ValueError("invalid cgroup")
        root = Path("/sys/fs/cgroup") / group.lstrip("/")
        if root.exists():
            for path in [root / "cgroup.procs", *root.glob("**/cgroup.procs")]:
                if path.read_text().strip():
                    raise RuntimeError("old bootstrap descendants remain")
    if state["ActiveState"] == "failed":
        # Killing an uncooperative descendant can leave Result=timeout even
        # after stop completed. Normalize only after proving the cgroup empty.
        command("systemctl", "reset-failed", UNIT_NAME)
        stopped()


def bootstrap(request):
    generation = request.get("generation")
    if type(generation) is not int or not 0 < generation < 2**63:
        raise ValueError("invalid generation")
    # flock also serializes a replacement agent against any surviving process.
    with (RUNTIME / "generation.lock").open("rb") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        state = read_generation()
        if generation <= state["generation"]:
            raise ValueError("stale generation")
        save_generation({"generation": generation, "phase": "executing"})
        apply_bootstrap(request)
        save_generation({"generation": generation, "phase": "done"})


def apply_bootstrap(request):
    values = {"ORB_TOKEN": request["token"],
              "AMP_URL": request.get("amp_url") or "https://ampcode.com",
              "HTTPS_PROXY": request.get("proxy", ""),
              "HTTP_PROXY": request.get("proxy", ""),
              # Tailscale CIDRs only match IP literals; MagicDNS names need the suffix.
              "NO_PROXY": "localhost,127.0.0.1,::1,169.254.169.254,100.64.0.0/10,.ts.net"}
    if not values["ORB_TOKEN"] or any(not isinstance(v, str) or
            any(c in v for c in "\r\n\x00") for v in values.values()):
        raise ValueError("invalid credentials")
    RUNTIME.mkdir(mode=0o700, exist_ok=True)
    os.chmod(RUNTIME, 0o700)
    credential = RUNTIME / "bootstrap.env"
    # Install the cgroup-wide stop policy before closing an old or orphaned
    # systemd job. Do not replace credentials until old descendants are gone.
    UNIT.write_text(BOOTSTRAP_UNIT)
    command("systemctl", "daemon-reload")
    command("systemctl", "stop", "--job-mode=replace", UNIT_NAME)
    stopped()
    # /run is tmpfs. Type=exec allows credential unlink after exec, not after
    # Amp registration. The generation acknowledgment makes only that claim.
    try:
        fd = os.open(credential, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "w") as stream:
            os.fchmod(stream.fileno(), 0o600)
            for key, value in values.items():
                value = value.replace("\\", "\\\\").replace('"', '\\"')
                stream.write(f'{key}="{value}"\n')
        command("systemctl", "start", UNIT_NAME)
        command("systemctl", "is-active", "--quiet", UNIT_NAME)
    finally:
        credential.unlink(missing_ok=True)


def handle(conn, peer_cid):
    if peer_cid != 2:
        return
    try:
        deadline = time.monotonic() + 10
        line = b""
        while b"\n" not in line and len(line) <= MAX_MESSAGE:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("request deadline")
            conn.settimeout(remaining)
            chunk = conn.recv(min(4096, MAX_MESSAGE + 1 - len(line)))
            if not chunk:
                break
            line += chunk
        if len(line) > MAX_MESSAGE or not line.endswith(b"\n"):
            raise ValueError("invalid frame")
        request = json.loads(line)
        if not isinstance(request, dict):
            raise ValueError("invalid request")
        action = request.get("action")
        if action == "ping" and set(request) == {"action"}:
            pass
        elif action == "bootstrap" and set(request) <= {"action", "token", "amp_url", "proxy", "generation"}:
            bootstrap(request)
        else:
            raise ValueError("unsupported action")
        response = (json.dumps({"ok": True, "generation": request["generation"]}).encode() + b'\n'
                    if action == "bootstrap" else b'{"ok":true}\n')
    except Exception:
        # Never return exception text or request data (including tokens).
        response = b'{"ok":false}\n'
    try:
        conn.settimeout(2)
        conn.sendall(response)
    except OSError:
        pass


def network():
    routes = json.loads(subprocess.check_output(["ip", "-j", "-4", "route", "show", "default"]))
    iface = next(route["dev"] for route in routes if "dev" in route)
    nameservers = [line.split()[1] for line in Path("/proc/net/pnp").read_text().splitlines()
                   if line.startswith("nameserver ")]
    if not nameservers:
        raise RuntimeError("kernel supplied no DNS servers")
    command("resolvectl", "dns", iface, *nameservers)
    command("resolvectl", "default-route", iface, "yes")
    command("ip", "route", "replace", "169.254.169.254/32", "dev", iface)


def main():
    os.umask(0o077)
    if sys.argv[1:] == ["--initialize"]:
        initialize_generation()
        return
    if sys.argv[1:] == ["--network"]:
        network()
        return
    read_generation()
    with socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM) as listener:
        listener.bind((socket.VMADDR_CID_ANY, PORT))
        listener.listen(8)
        while True:
            conn, peer = listener.accept()
            with conn:
                handle(conn, peer[0])


if __name__ == "__main__":
    main()
