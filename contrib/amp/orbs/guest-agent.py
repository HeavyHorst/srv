#!/usr/bin/python3
"""Narrow host-CID-only Orb bootstrap agent; no shell/exec RPC."""
import json
import os
import socket
import subprocess
import sys
import time
from pathlib import Path

PORT = 4050
MAX_MESSAGE = 65536
RUNTIME = Path("/run/srv-orb")
UNIT = Path("/run/systemd/system/srv-orb-bootstrap.service")
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
KillMode=process
TimeoutStopSec=10
LimitCORE=0
StandardOutput=null
StandardError=null
"""


def command(*args):
    subprocess.run(args, check=True, timeout=15, stdin=subprocess.DEVNULL,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def bootstrap(request):
    values = {"ORB_TOKEN": request["token"],
              "AMP_URL": request.get("amp_url") or "https://ampcode.com",
              "HTTPS_PROXY": request.get("proxy", ""),
              "HTTP_PROXY": request.get("proxy", ""),
              "NO_PROXY": "localhost,127.0.0.1,::1,169.254.169.254,100.64.0.0/10"}
    if not values["ORB_TOKEN"] or any(not isinstance(v, str) or
            any(c in v for c in "\r\n\x00") for v in values.values()):
        raise ValueError("invalid credentials")
    RUNTIME.mkdir(mode=0o700, exist_ok=True)
    os.chmod(RUNTIME, 0o700)
    credential = RUNTIME / "bootstrap.env"
    # /run is tmpfs, never the snapshot/root disk. systemd reads this as root
    # before exec; Type=exec lets us remove it as soon as restart returns.
    try:
        fd = os.open(credential, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "w") as stream:
            os.fchmod(stream.fileno(), 0o600)
            for key, value in values.items():
                value = value.replace("\\", "\\\\").replace('"', '\\"')
                stream.write(f'{key}="{value}"\n')
        UNIT.write_text(BOOTSTRAP_UNIT)
        command("systemctl", "daemon-reload")
        command("systemctl", "restart", "srv-orb-bootstrap.service")
        command("systemctl", "is-active", "--quiet", "srv-orb-bootstrap.service")
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
        elif action == "bootstrap" and set(request) <= {"action", "token", "amp_url", "proxy"}:
            bootstrap(request)
        else:
            raise ValueError("unsupported action")
        response = b'{"ok":true}\n'
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
    if sys.argv[1:] == ["--network"]:
        network()
        return
    with socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM) as listener:
        listener.bind((socket.VMADDR_CID_ANY, PORT))
        listener.listen(8)
        while True:
            conn, peer = listener.accept()
            with conn:
                handle(conn, peer[0])


if __name__ == "__main__":
    main()
