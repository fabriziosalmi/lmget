#!/usr/bin/env python3
"""site-stamp.py DIR: give the home page's CSS and JS links the hash of
their content (/site.css?v=<12 hex>), and the chat's files one hash of them
all (data-chat-v), in the copy about to be deployed.

Cloudflare's zone-level browser cache TTL keeps static files for hours
whatever _headers says; a URL that changes with the content cannot be
served stale. Run it on the deploy copy, never on site/ in the repo.
"""
import hashlib
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
page = root / "index.html"
html = page.read_text()
for name in ("site.css", "app.js", "intro.js"):
    digest = hashlib.sha256((root / name).read_bytes()).hexdigest()[:12]
    html, n = re.subn(r'"/%s(\?v=[0-9a-f]{12})?"' % re.escape(name), '"/%s?v=%s"' % (name, digest), html)
    if n != 1:
        sys.exit(f"{page}: expected one link to /{name}, found {n}")
    print(f"/{name}?v={digest}")
# the chat's files (site/chat/), loaded by app.js on demand: one version for
# all of them, on the form's data-chat-v
chat = hashlib.sha256()
for f in sorted((root / "chat").iterdir()):
    chat.update(f.name.encode() + b"\0" + f.read_bytes() + b"\0")
html, n = re.subn(r'data-chat-v="[0-9a-f]*"', 'data-chat-v="%s"' % chat.hexdigest()[:12], html)
if n != 1:
    sys.exit(f"{page}: expected one data-chat-v, found {n}")
print(f"/chat/*?v={chat.hexdigest()[:12]}")
page.write_text(html)
