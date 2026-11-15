"""archive.py EPOCH DIR OUT.tar.gz: a reproducible tarball of DIR.

Entries are sorted, owned by 0:0 with empty names, dated EPOCH, and the gzip
header carries no name or time, so the same input always gives the same bytes.
"""
import gzip
import os
import sys
import tarfile

epoch, src, out = int(sys.argv[1]), sys.argv[2], sys.argv[3]
base = os.path.basename(src.rstrip("/"))


def norm(ti):
    ti.uid = ti.gid = 0
    ti.uname = ti.gname = ""
    ti.mtime = epoch
    ti.mode = 0o755 if ti.isdir() or ti.name.endswith("/lmget") else 0o644
    return ti


with open(out, "wb") as raw:
    with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0, compresslevel=9) as gz:
        with tarfile.open(fileobj=gz, mode="w", format=tarfile.PAX_FORMAT) as tar:
            tar.add(src, arcname=base, recursive=False, filter=norm)
            for name in sorted(os.listdir(src)):
                tar.add(os.path.join(src, name), arcname=base + "/" + name, recursive=False, filter=norm)
