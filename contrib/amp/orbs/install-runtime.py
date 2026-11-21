#!/usr/bin/python3
"""Install only the pinned official Amp executor/workload runtime into a guest root."""
import argparse
import hashlib
import io
import os
from pathlib import Path
import tarfile
import tempfile
import urllib.request

LAYER_SHA256 = "9571fe6d7a57c153f0dafa679edd453a60171d6f049d4b2c85764ea65f7aedd0"
LAYER_URL = ("https://us-central1-docker.pkg.dev/v2/amp-prod-0/amp-orb/amp-orb/blobs/sha256:"
             + LAYER_SHA256)
ASSETS = {
    **{f"etc/systemd/system/{name}": 0o644 for name in (
        "amp.slice", "amp-control.slice", "amp-workload.slice", "amp-headless.service",
        "amp-headless-env.path", "amp-headless-env.service")},
    **{f"usr/local/bin/{name}": 0o755 for name in (
        "amp-headless-update-binary", "amp-headless-apply-env", "amp-run-workload")},
    "etc/sudoers.d/amp-systemd-run": 0o440,
}


def install(root, layer):
    if hashlib.sha256(layer).hexdigest() != LAYER_SHA256:
        raise ValueError("Amp runtime layer checksum mismatch")
    with tarfile.open(fileobj=io.BytesIO(layer), mode="r:gz") as archive:
        assets = {}
        for name in ASSETS:
            member = archive.getmember(name)
            if not member.isfile():
                raise ValueError(f"Amp runtime asset is not a regular file: {name}")
            assets[name] = archive.extractfile(member).read()
    for name, data in assets.items():
        path = root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as stream:
            temporary = Path(stream.name)
            try:
                stream.write(data)
                stream.flush()
                temporary.chmod(ASSETS[name])
                os.replace(temporary, path)
            finally:
                temporary.unlink(missing_ok=True)
    wants = root / "etc/systemd/system/multi-user.target.wants"
    wants.mkdir(parents=True, exist_ok=True)
    link = wants / "amp-headless-env.path"
    link.unlink(missing_ok=True)
    link.symlink_to("../amp-headless-env.path")
    provenance = root / "usr/local/share/srv/amp-runtime-layer.sha256"
    provenance.parent.mkdir(parents=True, exist_ok=True)
    provenance.write_text(LAYER_SHA256 + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", type=Path, help="guest root, or an empty test directory")
    parser.add_argument("--layer", type=Path, help="offline copy of the same pinned layer")
    args = parser.parse_args()
    if args.layer:
        layer = args.layer.read_bytes()
    else:
        with urllib.request.urlopen(LAYER_URL, timeout=60) as response:
            layer = response.read()
    install(args.root, layer)


if __name__ == "__main__":
    main()
