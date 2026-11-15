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
- **Content-addressed.** Files are stored once by sha256 in `~/.lmget` (or `$LMGET_HOME`) and hardlinked into the target, so nothing is copied twice.
- **Checked by content, not only by hash.** A matching hash proves the bytes are what the source publishes, not that they are what the name says. Before a file enters the store, `.safetensors` must have a valid header, `.gguf` must start with the GGUF magic, and a file whose content is a Python pickle is refused unless its name says so. Pickles are detected by walking their opcodes (protocols 0 to 5); a zip with a `.pkl` entry, as `torch.save` writes, counts as one, and so does a zip lmget cannot read. Refused files exit with code 5.
- **Safer defaults.** Pickle weights (`.bin`, `.pt`, `.ckpt`, ...) are skipped when the repository also has safetensors (`--allow-pickle` keeps them). File paths that would leave the model directory, that contain control characters, or that contain invisible code points (which some filesystems drop, so two names could land on one file) are refused. Sizes above 16 TiB are refused.

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

The project keeps a signed index of models it mirrors: for each, the reference, the commit and every file with its size and sha256, and the nodes that serve those files by hash. `lmget index` fetches it, verifies its minisign signature against the project key built into lmget (not published yet; `LMGET_INDEX_URL` and `LMGET_INDEX_KEY` point lmget at another index), refuses an index older than the one it has or past its expiry date, and keeps it in the store.

With a verified index in the store, the nodes are the last origin of a file: after the source (and, for Hugging Face, ModelScope) failed. When the source cannot be reached at all, or no longer has the model, the plan of an indexed model comes from the index, because its file list is signed. Access refusals (gated, private) are never routed to the nodes, and every byte is verified against the signed hash.

Two maintainer tools sit next to the CLI:

- `lmget-index build -lock lmget.lock -sequence N -node https://...` writes the index from a lockfile. Every model is resolved again at its source: it must not be gated, the license the source declares must be on the allowlist (Apache-2.0, MIT, BSD, ISC, CC0, CC-BY-4.0, Unlicense), the source must publish exactly the locked files, and files the source names only by git id are downloaded to confirm their sha256. Sign the output with `minisign -S`.
- `lmget-node mirror` mirrors the models of the index, pinned to its commits and files (a source that drifted from the index is refused); `lmget-node serve` serves the index and only the blobs it names, with ranges, behind a TLS reverse proxy.

## Lockfile

```sh
lmget Qwen/Qwen3-0.6B --lock     # creates ./lmget.lock, or adds the model to it
lmget sync                       # reproduce it: same commits, every file checked against the locked sha256
lmget sync --update              # resolve the revisions again and rewrite the lock
```

`lmget.lock` pins each model to a commit (for Ollama, the sha256 of its manifest) and every file to its size and sha256, together with the selection (`--include`, `--exclude`) and the target. Once a lockfile exists in the current directory, `lmget get` keeps it current. Concurrent lmget runs update it one at a time, and `sync` never writes over an entry that another run changed while it was downloading. `sync` stops with exit code 5 if the source no longer matches the lock. When every locked file is already in the store, `sync` does not use the network.

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

## Output for scripts and integrations

On a terminal lmget draws a live view. When stdout is not a terminal it writes stable `lmget: ...` log lines to stderr and the resulting path to stdout:

```sh
dir=$(lmget Qwen/Qwen3-0.6B --to dir:./models/qwen3)
```

`--json` writes one JSON event per line to stdout. Every event has `v` (schema version, currently 1), `type` and `time`. Numeric fields are omitted when zero.

| type | fields |
|---|---|
| `resolve` | `ref` |
| `plan` | `plan`: `ref` (`source`: `hf`, `ollama`, `url`), `commit`, `license`, `gated`, `hash_from` (URLs: `user`, `server`, `first`), `files[]` (`path`, `size`, `sha256`, `git_sha1`, `cached`, `skipped`), `bytes`, `to_fetch`, `pickle`, `disk_free` |
| `file_start` | `file`, `total` |
| `progress` | `file`, `bytes`, `total` (at most 4 per second per file) |
| `file_done` | `file`, `sha256`, `total`, `cached`, `from` (`modelscope` when the bytes came from the second origin) |
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
| 5 | verification failed: received bytes do not match the published or locked hash, or the content is not what the file name says |
| 6 | network: source unreachable or failing after retries |
| 7 | disk: not enough space, or a write failed |
| 130 | interrupted; run the same command to resume |

## Library

The CLI is a renderer over the Go package `github.com/fabriziosalmi/lmget`: `Client.Pull` emits the same events, so an application gets exactly what the CLI shows.

## License

Apache-2.0. See [LICENSE](LICENSE).
