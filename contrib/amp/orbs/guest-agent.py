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
NO_PROXY_BASE = "localhost,127.0.0.1,::1,169.254.169.254,100.64.0.0/10,.ts.net"
# Not ordered after network-online.target: the host only bootstraps after this
# agent answers, and the agent starts after its --network step configured the
# kernel-provided address, routes and DNS. Images may still pull in a
# wait-online that can stall far longer than the host's bootstrap exchange.
BOOTSTRAP_UNIT = """[Unit]
Description=Amp orb bootstrap
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
    # Guest-local destinations never use the gateway. proxy_bypass is the
    # provider's list of extra direct-connect destinations — hostnames, IPv4
    # literals, or leading-dot DNS suffixes, validated entry by entry in
    # handle() before any generation is consumed — and is only ever appended
    # to the built-in entries.
    extra = request.get("proxy_bypass", "")
    values = {"ORB_TOKEN": request["token"],
              "AMP_URL": request.get("amp_url") or "https://ampcode.com",
              "HTTPS_PROXY": request.get("proxy", ""),
              "HTTP_PROXY": request.get("proxy", ""),
              "NO_PROXY": NO_PROXY_BASE + ("," + extra if extra else "")}
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
        try:
            command("systemctl", "start", UNIT_NAME)
            command("systemctl", "is-active", "--quiet", UNIT_NAME)
        except BaseException:
            # A client timeout leaves PID1's start job queued or running; it
            # could still launch after a failure ack. Remove the credentials,
            # then cancel and prove quiescence. If that cannot be proven, this
            # raises instead and the generation stays "executing".
            credential.unlink(missing_ok=True)
            command("systemctl", "stop", "--job-mode=replace", UNIT_NAME)
            stopped()
            raise
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
        elif action == "bootstrap" and set(request) <= {"action", "token", "amp_url", "proxy", "generation", "proxy_bypass"}:
            # Validate before bootstrap(): an invalid destination must be
            # rejected like an unknown key, without consuming a generation.
            if not valid_proxy_bypass(request.get("proxy_bypass", "")):
                raise ValueError("invalid proxy bypass")
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


def valid_proxy_bypass(value):
    # The provider's optional destination list, re-validated here because
    # dispatcher validation is not the trust boundary: one to eight
    # comma-separated entries, at most 512 characters joined, no whitespace,
    # empty entries, duplicates, or built-in repeats. Each entry is a
    # hostname, IPv4 literal, or leading-dot DNS suffix with an optional
    # trailing dot kept verbatim — it only matches clients that compare the
    # same form. CIDR and IPv6 stay out: NO_PROXY handling differs between
    # HTTP clients. A matching contract, not a DNS linter or policy framework.
    if value == "":
        return True
    if (not isinstance(value, str) or len(value) > 512 or not value.isascii()
            or any(c.isspace() or c in "\x00'\"\\\\" for c in value)):
        return False
    seen = set()
    entries = value.split(",")
    if len(entries) > 8:
        return False
    for entry in entries:
        if (not entry or entry in NO_PROXY_BASE.split(",") or entry in seen
                or not valid_bypass_entry(entry)):
            return False
        seen.add(entry)
    return True


def valid_bypass_entry(entry):
    # One entry: hostname, IPv4 literal, or leading-dot DNS suffix; an
    # optional single trailing dot is the valid FQDN form, kept verbatim.
    # Junk without an alphanumeric is rejected; label characters are letters,
    # digits, hyphens and underscores.
    if len(entry) > 253:
        return False
    name = entry[1:] if entry.startswith(".") else entry
    if not name:
        return False
    labels = name.split(".")
    if labels[-1] == "":
        labels = labels[:-1]
    if not labels:
        return False
    alphanumeric = False
    for label in labels:
        if not label or len(label) > 63:
            return False
        for c in label:
            if c.isalnum():
                alphanumeric = True
            elif c not in "-_":
                return False
    return alphanumeric


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
