#!/usr/bin/env python3
"""wheels.py TAG ARCHIVES OUTDIR: build the Python wheels of release TAG
(vX.Y.Z) from its archives in ARCHIVES (checked against the checksums.txt
there first) into OUTDIR.

A wheel holds no Python code: it carries the release binary as a script, as
ruff's wheels do, so "pip install lmget" or "uv tool install lmget" puts
lmget on the PATH. One wheel per platform:

    darwin amd64/arm64   macosx_12_0 (the oldest macOS Go supports)
    linux amd64/arm64    manylinux_2_17 and musllinux_1_2 (a static binary)
    windows amd64/arm64  win_amd64, win_arm64

The wheels are deterministic: fixed timestamps and order, so the same
archives give the same bytes.
"""
import base64
import hashlib
import pathlib
import re
import sys
import tarfile
import zipfile

PLATFORMS = {
    ("darwin", "amd64"): ["macosx_12_0_x86_64"],
    ("darwin", "arm64"): ["macosx_12_0_arm64"],
    ("linux", "amd64"): ["manylinux_2_17_x86_64", "manylinux2014_x86_64", "musllinux_1_2_x86_64"],
    ("linux", "arm64"): ["manylinux_2_17_aarch64", "manylinux2014_aarch64", "musllinux_1_2_aarch64"],
    ("windows", "amd64"): ["win_amd64"],
    ("windows", "arm64"): ["win_arm64"],
}
STAMP = (1980, 1, 1, 0, 0, 0)

DESCRIPTION = """\
lmget downloads open model weights from their sources (Hugging Face, the
Ollama registry, OCI registries, https URLs), checks every file against the
hash its source publishes, stores each file once, and links the model where
your tools look: the Hugging Face cache, Ollama, LM Studio, oMLX, Jan,
LocalAI, exo or a directory.

This package installs the lmget binary of the GitHub release with the same
version, unchanged; it holds no Python code. See https://lmget.org and
https://github.com/fabriziosalmi/lmget.
"""


def b64(digest: bytes) -> str:
    return base64.urlsafe_b64encode(digest).rstrip(b"=").decode()


def wheel(version: str, tags: list[str], exe: str, binary: bytes, license_text: bytes, out: pathlib.Path) -> pathlib.Path:
    dist = f"lmget-{version}.dist-info"
    data = f"lmget-{version}.data/scripts/{exe}"
    metadata = "\n".join([
        "Metadata-Version: 2.1",
        "Name: lmget",
        f"Version: {version}",
        "Summary: Get open models from their sources, verified",
        "Home-page: https://lmget.org",
        "Author: Fabrizio Salmi",
        "License: Apache-2.0",
        "Project-URL: Homepage, https://lmget.org",
        "Project-URL: Source, https://github.com/fabriziosalmi/lmget",
        "Project-URL: Releases, https://github.com/fabriziosalmi/lmget/releases",
        "Classifier: License :: OSI Approved :: Apache Software License",
        "Classifier: Environment :: Console",
        "Classifier: Topic :: Scientific/Engineering :: Artificial Intelligence",
        "Description-Content-Type: text/plain",
        "",
        DESCRIPTION,
    ]).encode()
    wheel_meta = "\n".join(["Wheel-Version: 1.0", "Generator: lmget scripts/wheels.py", "Root-Is-Purelib: false"] +
                           [f"Tag: py3-none-{t}" for t in tags]).encode() + b"\n"
    files = [
        (data, binary, 0o755),
        (f"{dist}/LICENSE", license_text, 0o644),
        (f"{dist}/METADATA", metadata, 0o644),
        (f"{dist}/WHEEL", wheel_meta, 0o644),
    ]
    record = "".join(f"{name},sha256={b64(hashlib.sha256(body).digest())},{len(body)}\n" for name, body, _ in files)
    record += f"{dist}/RECORD,,\n"
    files.append((f"{dist}/RECORD", record.encode(), 0o644))
    path = out / f"lmget-{version}-py3-none-{'.'.join(tags)}.whl"
    with zipfile.ZipFile(path, "w") as z:
        for name, body, mode in files:
            info = zipfile.ZipInfo(name, STAMP)
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = (0o100000 | mode) << 16  # a regular file with its mode: pip keeps the exec bit
            info.create_system = 3  # unix, so the mode above is read
            z.writestr(info, body, compresslevel=9)
    return path


def main() -> None:
    tag, archives, out = sys.argv[1], pathlib.Path(sys.argv[2]), pathlib.Path(sys.argv[3])
    if not re.fullmatch(r"v\d+\.\d+\.\d+", tag):
        sys.exit(f"not a release tag: {tag}")
    version = tag[1:]
    sums = {}
    for line in (archives / "checksums.txt").read_text().splitlines():
        h, name = line.split()
        sums[name] = h
    out.mkdir(parents=True, exist_ok=True)
    for (goos, goarch), tags in PLATFORMS.items():
        name = f"lmget_{version}_{goos}_{goarch}.tar.gz"
        raw = (archives / name).read_bytes()
        if hashlib.sha256(raw).hexdigest() != sums.get(name):
            sys.exit(f"{name} does not match checksums.txt")
        exe = "lmget.exe" if goos == "windows" else "lmget"
        top = f"lmget_{version}_{goos}_{goarch}"
        with tarfile.open(archives / name) as t:
            binary = t.extractfile(f"{top}/{exe}").read()
            license_text = t.extractfile(f"{top}/LICENSE").read()
        print(wheel(version, tags, exe, binary, license_text, out))


if __name__ == "__main__":
    main()
