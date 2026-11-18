#!/usr/bin/env python3
"""scoop-manifest.py VERSION CHECKSUMS: write bucket/lmget.json, the Scoop
manifest of release VERSION (vX.Y.Z), with the hashes from its
checksums.txt. The repository is its own Scoop bucket:

    scoop bucket add lmget https://github.com/fabriziosalmi/lmget
    scoop install lmget

Run it for every release, after the archives are published.
"""
import json
import pathlib
import re
import sys

REPO = "fabriziosalmi/lmget"

tag, checksums = sys.argv[1], pathlib.Path(sys.argv[2])
if not re.fullmatch(r"v\d+\.\d+\.\d+", tag):
    sys.exit(f"not a release tag: {tag}")
version = tag[1:]
sums = {}
for line in checksums.read_text().splitlines():
    h, name = line.split()
    if not re.fullmatch(r"[0-9a-f]{64}", h):
        sys.exit(f"not a sha256: {h}")
    sums[name] = h

base = f"https://github.com/{REPO}/releases/download/v{version}"
arch = {}
for scoop, goarch in (("64bit", "amd64"), ("arm64", "arm64")):
    name = f"lmget_{version}_windows_{goarch}.tar.gz"
    if name not in sums:
        sys.exit(f"{name} is not in {checksums}")
    arch[scoop] = {"url": f"{base}/{name}", "hash": sums[name], "extract_dir": f"lmget_{version}_windows_{goarch}"}

manifest = {
    "version": version,
    "description": "Get open models from their sources, verified",
    "homepage": "https://lmget.org",
    "license": "Apache-2.0",
    "architecture": arch,
    "bin": "lmget.exe",
    "checkver": {"github": f"https://github.com/{REPO}"},
    "autoupdate": {
        "architecture": {
            scoop: {
                "url": f"https://github.com/{REPO}/releases/download/v$version/lmget_$version_windows_{goarch}.tar.gz",
                "extract_dir": f"lmget_$version_windows_{goarch}",
            }
            for scoop, goarch in (("64bit", "amd64"), ("arm64", "arm64"))
        },
        "hash": {"url": "$baseurl/checksums.txt"},
    },
}
out = pathlib.Path(__file__).resolve().parent.parent / "bucket" / "lmget.json"
out.write_text(json.dumps(manifest, indent=4) + "\n")
print(out)
