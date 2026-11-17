# lmget

Get open models from their sources, verified.

lmget resolves a model, shows what it is about to download, fetches it with resumable parallel requests, checks every file against the hash the source publishes, and links it where your tools already look for it.

Status: early development. Sources in this build: Hugging Face (with ModelScope as a second origin), the Ollama registry, OCI registries (Docker Hub's `ai/` models, ghcr.io, ...), and direct https URLs.

## Usage

```sh
lmget Qwen/Qwen3-0.6B                          # download, verify, link into the Hugging Face cache
lmget info Qwen/Qwen3-0.6B                     # the plan only: files, sizes, hashes, license, checks
lmget Qwen/Qwen3-0.6B@<branch|tag|commit>
lmget https://huggingface.co/Qwen/Qwen3-0.6B   # URLs work too
lmget Qwen/Qwen3-0.6B --to dir:./models/qwen3  # plain directory instead of the HF cache
lmget unsloth/Qwen3-8B-GGUF --include '*Q4_K_M*'
lmget https://huggingface.co/unsloth/Qwen3-8B-GGUF/resolve/main/Qwen3-8B-Q4_K_M.gguf   # one file
lmget https://example.org/models/tiny.gguf     # any https URL, saved in the current directory
lmget 'https://example.org/models/tiny.gguf#sha256=<64 hex>'   # with the hash you expect
lmget ollama:smollm2:135m                      # into Ollama: ollama run smollm2:135m
```

What happens on every run:

- **Plan first.** The revision is resolved to a commit, and every file is listed with its size and published hash before a byte is downloaded.
- **Verified.** Large files are checked against their sha256 (Git LFS); small files against their git blob id, and their sha256 is recorded. A mismatch discards the partial file and exits with code 5.
- **Resumable.** Interrupt at any time; the same command resumes from the last completed chunk.
- **Content-addressed.** Files are stored once by sha256 in `~/.lmget` (or `$LMGET_HOME`) and hardlinked into the target, so nothing is copied twice (across filesystems, where a hard link is impossible, the target gets a symlink to the store).
- **Checked by content, not only by hash.** A matching hash proves the bytes are what the source publishes, not that they are what the name says. Before a file enters the store, its header is walked the way the reference loader reads it, and refused where that loader refuses it: for `.safetensors`, the JSON header must describe every tensor with a known size and the tensors must tile the data exactly, with no gap, overlap or trailing bytes (as the `safetensors` library checks); for `.gguf` versions 2 and 3, counts, strings and arrays must fit in the file, keys and tensor names must not repeat, and tensors must follow one another from the aligned data section within the file (as llama.cpp checks). Header fields that point past the file are how a crafted model attacks the program that opens it. A dtype or tensor type newer than lmget is not refused; it is only not sized. A file whose content is a Python pickle is refused unless its name says so. Pickles are detected by walking their opcodes (protocols 0 to 5); a zip with a `.pkl` entry, as `torch.save` writes, counts as one, and so does a zip lmget cannot read, or a zip with a `.pkl` entry appended after another format's header. Refused files exit with code 5.
- **Safer defaults.** Pickle weights (`.bin`, `.pt`, `.pth`, `.ckpt`, `.ot`, `.joblib`, `.pkl`) are skipped when the repository also has safetensors (`--allow-pickle` keeps them). File paths that would leave the model directory, that contain control characters, or that contain invisible code points (which some filesystems drop, so two names could land on one file) are refused. Sizes above 16 TiB are refused.

## Ollama

```sh
lmget ollama:qwen3:0.6b                        # name[:tag], tag latest by default
lmget ollama:huihui_ai/qwen3-abliterated:0.6b  # namespace/name:tag
lmget https://ollama.com/library/qwen3:0.6b    # URLs work too
lmget ollama:qwen3:0.6b --to dir:./models/qwen3
```

lmget reads the manifest of the tag from `registry.ollama.ai` and identifies the model by the sha256 of that manifest, which is the commit shown in the plan and pinned in `lmget.lock`. Every layer is a file of the plan, named after its type (`model.gguf`, `template`, `system`, `params.json`, `license`, ...), and verified against the sha256 digest the manifest gives for it; `manifest.json` itself is always part of the download. The model layer must be a GGUF file.

Ollama models are exported by default to Ollama's models directory (`$OLLAMA_MODELS`, else `~/.ollama/models`): the layers are linked as `blobs/sha256-<digest>` (hard links on the same disk) and the manifest is written as the tag file, as a copy: a tool that rewrites the tag file cannot change the store. The Ollama export needs every layer, so `--include` and `--exclude` work only with `--to dir:` or `--to none`. `lmget rm ollama:<name>:<tag>` removes only what lmget wrote: blobs and tag files that Ollama downloaded itself are left in place, and so is a layer that another tag still names, whoever wrote that tag.

The registry serves manifests by tag only. `sync` therefore rebuilds a locked Ollama model from the manifest an earlier pull verified for that same name, and fetches missing layers by digest even after the tag has moved; without such a manifest in the store, the tag must still point at the locked one, or `sync` stops with exit code 5. Only public models are supported, and no token is sent to the registry.

## Signed index and nodes

The project keeps a signed index of models it mirrors: for each, the reference, the commit and every file with its size and sha256, and the nodes that serve those files by hash. `lmget index` fetches it, verifies its minisign signature against the project key built into lmget (the index is published at `https://lmget.org/index.json`; `LMGET_INDEX_URL` and `LMGET_INDEX_KEY` point lmget at another index), refuses an index older than the highest one it ever accepted (even after that one expired), an index past its expiry date, and an index valid for more than 60 days, and keeps it in the store.

Two kinds of keys sign it. The root key (key number `736BC89C91E98921`, built into lmget) stays offline and signs one file: `keys.json`, next to the index, which lists the keys allowed to sign the index and the keys that are revoked. While a valid key list is known, the index keys are exactly the ones it lists; without one (none published, or expired), lmget trusts the built-in project key `21C7BDA090E969F4`. An index signed by the root key, by a revoked key or by a key the list does not name is refused, and the error says which key signed it. Revocations and the highest accepted key list are kept in the store for good: a revoked key is never trusted again there, even after the list expires, and an older list cannot bring it back. A key list is valid for at most 400 days; one that cannot be reached leaves the keys in force, one with a bad signature or content stops `lmget index`. `LMGET_ROOT_KEY` adds a root key for a self-hosted index. `lmget index` shows the key that signed the index and the key list in force.

With a verified index in the store, the nodes are the last origin of a file: after the source (and, for Hugging Face, ModelScope) failed. When the source cannot be reached at all, or no longer has the model, the plan of an indexed model comes from the index, because its file list is signed. Access refusals (gated, private) are never routed to the nodes, and every byte is verified against the signed hash. A node that fails or serves other bytes is skipped for the rest of the run.

Two maintainer tools sit next to the CLI:

- `lmget-index build -lock lmget.lock -sequence N -node https://...` writes the index from a lockfile. Every model is resolved again at its source: it must not be gated, the license the source declares must be on the allowlist (Apache-2.0, MIT, BSD, ISC, CC0, CC-BY-4.0, Unlicense), and the lock must list every file the source publishes at the locked commit, with the source's own sizes, sha256 and git ids (an index entry is a whole model, never a selection; pickle weights next to safetensors only with `allow_pickle`). The builder then downloads every file into its own store, pinned to the lock (so a lock cannot put a wrong sha256 under the signature), and computes each blob's torrent identity. Sign the output with `minisign -S`.
- `lmget-index keys -sequence N -index-key lmget-index.pub [-revoke NUMBER]` writes `keys.json`, checked as lmget will check it (a root key can be neither listed nor revoked, a listed key cannot be revoked). Sign it with the root key and publish it next to the index.
- `lmget-node mirror` mirrors the models of the index, pinned to its commits and files (a source that drifted from the index is refused); `lmget-node serve` serves the index and only the blobs it names, with ranges, behind a TLS reverse proxy; `lmget-node seed` seeds the same blobs as BitTorrent v2 torrents and announces them on the Mainline DHT, re-reading the index every minute. Both stop offering a blob once the index no longer names it, and all of them once the index expires; from 7 days before the expiry they say so in their log. [deploy/lmget-node](deploy/lmget-node) runs a node in a Proxmox container with no inbound port, behind a Cloudflare Tunnel.

Every blob is its own BitTorrent v2 torrent: its name and file are its sha256 and the piece length is fixed at 4 MiB, so its infohash depends only on its content. The index carries that infohash (`btih2`) for every file, so any BitTorrent v2 client can fetch a mirrored blob from the nodes and from other seeders. The `lmget` CLI itself does not include a torrent client: it downloads from the nodes over HTTP and stays small.

## Lockfile

```sh
lmget Qwen/Qwen3-0.6B --lock     # creates ./lmget.lock, or adds the model to it
lmget sync                       # reproduce it: same commits, every file checked against the locked sha256
lmget sync --update              # resolve the revisions again and rewrite the lock
```

`lmget.lock` pins each model to a commit (for Ollama, the sha256 of its manifest) and every file to its size and sha256, together with the selection (`--include`, `--exclude`) and the target. Once a lockfile exists in the current directory, `lmget get` keeps it current. Concurrent lmget runs update it one at a time, and `sync` never writes over an entry that another run changed while it was downloading. `sync` stops with exit code 5 if the source no longer matches the lock. When every locked file is already in the store, `sync` does not use the network.

The lock also records the license the source declared. If a later `get` or `sync --update` finds another license (or none), lmget stops before downloading, with exit code 5 and the two licenses side by side. Read the new terms, then run the same command with `--accept-license-change` to go on and record the new license. The index builder refuses such an entry the same way.

A lockfile usually arrives with a cloned repository, so lmget treats it as untrusted input:

- `sync` works offline only for files whose path and sha256 an earlier pull already checked against the source, and only with the git blob id that pull recorded for them; a lock cannot re-label bytes in the store as another model, nor name another blob of the Hugging Face cache.
- `dir:` targets must stay inside the lockfile's directory, every exported file is checked again with symlinks resolved, and nothing is ever written through a `.git` directory, nor through a name that a filesystem folds onto `.git` (such as `.git` with a zero-width character inside).
- A URL entry is bound to its own URL. A blob in the store stands for that URL only after a download from it verified that binding; otherwise `sync` downloads it again and checks it against the lock. Refs in a lock carry no `#sha256=` fragment or file path (the expected hash belongs in the file entry), and a lock that has one is refused.
- lmget only reads and writes a lockfile that is a regular file, not a symlink.
- A lock may list https URLs, and `sync` downloads from them: review the lock of a repository you did not write, as you would review its code.

## Managing the store

```sh
lmget ls                         # models, sizes, where they are exported
lmget rm Qwen/Qwen3-0.6B         # remove it and free the blobs no other model uses
lmget doctor                     # store, export targets, network, token, lockfile
lmget doctor --verify            # also re-hash every blob in the store
```

`rm` removes only the links lmget created that still point at its data; files the user replaced, downloads made by other tools, and shared files another model still uses (a blob another commit's snapshot links to, a file another model exported to the same directory) are left in place. The space reported as freed is measured from the blobs' link counts, so space still held by other hard links is reported separately. `doctor --verify` moves a corrupt blob to `quarantine/` in the store, together with lmget's hard links to it, so the next `get` or `sync` downloads it again. `doctor` never prints the token, only where it was found.

## Direct URLs

Only https is accepted, and URLs with credentials (`user:pass@`) are refused. The expected sha256 comes from, in order: a `#sha256=` fragment you add, a `Repr-Digest` or `Digest` header from the server, or, when neither exists, the first download, which is then pinned in `lmget.lock` and shown as "pinned on first download". A partial download is bound to the server's `ETag` (or `Last-Modified`) and every ranged request carries `If-Range`: if the file changes on the server, lmget starts over instead of joining two versions. URLs with a query string (signed URLs carry credentials there) are downloaded but never written to `lmget.lock`. lmget sends no token to hosts other than the Hugging Face endpoint, and a redirect from https to http is refused.

## OCI registries

```sh
lmget oci://docker.io/ai/smollm2               # Docker Hub's ai/ models, tag latest by default
lmget oci://docker.io/ai/smollm2:360M-Q4_K_M
lmget oci://ghcr.io/org/model@sha256:<64 hex>  # pinned by manifest digest
```

lmget reads the model's manifest (OCI or Docker), checks it against the registry's `Docker-Content-Digest`, and pins its digest as the commit; `sync` asks for that exact digest, so it reproduces the model even after the tag moved. Every layer is verified against its sha256 digest. Files are named after the manifest's `org.cncf.model.filepath` annotation (CNCF model spec) or, for Docker's AI artifacts, after their type (`model.gguf`, `license`). The model is exported to `dir:./<name>` by default. Anonymous pull tokens are renewed while a download runs; only public images are supported, and multi-platform indexes are refused (pin a manifest with `@sha256:`).

## When Hugging Face fails

If a file of a Hugging Face model cannot be downloaded (network errors, 5xx or 429 after the retries, or the file missing), lmget looks for the same repository on ModelScope (`www.modelscope.ai`) and takes that file from there, but only if ModelScope lists the same path with the same size and the same sha256. The bytes are verified against the hash Hugging Face publishes, so ModelScope is never trusted: a copy that differs is refused with exit code 5. The log says which files came from ModelScope (`from=modelscope` in plain output and in `file_done` events).

ModelScope is asked nothing while Hugging Face works, and never for a file Hugging Face refuses (401, 403): a gated model stays gated. `--no-mirror` turns the fallback off. The plan itself always comes from Hugging Face: if Hugging Face cannot be reached at all, there is no fallback.

Private and gated models use `HF_TOKEN` or the token saved by `huggingface-cli login`. `HF_ENDPOINT` selects a mirror. The token is sent only to the configured endpoint, and is dropped on a redirect to any other host or port.

## Your own origins

```sh
lmget sync --origin https://cache.example.lan     # or LMGET_ORIGINS="https://a,http://b"
```

An origin is a server that holds blobs at `<origin>/blobs/sha256/<sha256>`: a LAN cache, a company mirror, an `lmget-node`, or any static file server over another machine's `LMGET_HOME`, whose store uses the same paths (`python3 -m http.server -d ~/.lmget` is enough on a trusted network). lmget asks the origins, in order, for every file whose sha256 the plan knows, before the source; files the source publishes with a git id only (small, non-LFS files) are not asked for, and with a lockfile every file has its sha256. What an origin serves is verified like any download: an origin that serves other bytes is skipped for the rest of the run and the file comes from the source, which never resumes an origin's partial chunks. No token is ever sent to an origin. `http` is accepted because integrity rests on the hash, not on TLS; use `https` when the names of the models you fetch are not for the network to see. An origin is tried twice per chunk, so one that is down costs well under a second before the source takes over. The plan still comes from the source (or the signed index).

## Output for scripts and integrations

On a terminal lmget draws a live view. When stdout is not a terminal it writes stable `lmget: ...` log lines to stderr and the resulting path to stdout:

```sh
dir=$(lmget Qwen/Qwen3-0.6B --to dir:./models/qwen3)
```

`--json` writes one JSON event per line to stdout. Every event has `v` (schema version, currently 1), `type` and `time`. Numeric fields are omitted when zero.

| type | fields |
|---|---|
| `resolve` | `ref` |
| `plan` | `plan`: `ref` (`source`: `hf`, `ollama`, `oci`, `url`), `commit`, `license`, `gated`, `hash_from` (URLs: `user`, `server`, `first`), `files[]` (`path`, `size`, `sha256`, `git_sha1`, `cached`, `skipped`), `bytes`, `to_fetch`, `pickle`, `disk_free`, `from_index` (the plan came from the signed index) |
| `file_start` | `file`, `total` |
| `progress` | `file`, `bytes`, `total` (at most 4 per second per file) |
| `file_done` | `file`, `sha256`, `total`, `cached`, `from` (`origin:<host>`, `modelscope` or `node:<host>` when the bytes did not come from the source) |
| `export` | `target`, `path` |
| `done` | `total`, `fetched`, `elapsed_s`, `path` |
| `lock` | `path` of the lockfile written after the pull |
| `error` | `error`: `code`, `kind`, `message`, `why`, `fixes[]` (`cmd`, `note`) |

`sync --json` emits the same events once per model. `ls`, `rm` and `doctor` with `--json` write a single object with `v`, `type` (`list`, `removed`, `doctor`) and `models` plus `store`, `result`, or `checks`.

## Exit codes

| code | meaning |
|---|---|
| 0 | success |
| 1 | other failure; for `doctor`, at least one check failed |
| 2 | bad reference, flag or unsupported source |
| 3 | repository, revision or file not found |
| 4 | private or gated: token missing or not granted |
| 5 | verification failed: received bytes do not match the published or locked hash, the content is not what the file name says, or the license differs from the one in the lockfile |
| 6 | network: source unreachable or failing after retries |
| 7 | disk: not enough space, or a write failed |
| 130 | interrupted; run the same command to resume |

## Library

The CLI is a renderer over the Go package `github.com/fabriziosalmi/lmget`: `Client.Pull` emits the same events, so an application gets exactly what the CLI shows.

## License

Apache-2.0. See [LICENSE](LICENSE).
