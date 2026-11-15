# lmget

Get open models from their sources, verified.

lmget resolves a model, shows what it is about to download, fetches it with resumable parallel requests, checks every file against the hash the source publishes, and links it where your tools already look for it.

Status: early development. Sources in this build: Hugging Face, and direct https URLs.

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
```

What happens on every run:

- **Plan first.** The revision is resolved to a commit, and every file is listed with its size and published hash before a byte is downloaded.
- **Verified.** Large files are checked against their sha256 (Git LFS); small files against their git blob id, and their sha256 is recorded. A mismatch discards the partial file and exits with code 5.
- **Resumable.** Interrupt at any time; the same command resumes from the last completed chunk.
- **Content-addressed.** Files are stored once by sha256 in `~/.lmget` (or `$LMGET_HOME`) and hardlinked into the target, so nothing is copied twice.
- **Checked by content, not only by hash.** A matching hash proves the bytes are what the source publishes, not that they are what the name says. Before a file enters the store, `.safetensors` must have a valid header, `.gguf` must start with the GGUF magic, and a file whose content is a Python pickle (raw, or the zip `torch.save` writes) is refused unless its name says so. Refused files exit with code 5.
- **Safer defaults.** Pickle weights (`.bin`, `.pt`, `.ckpt`, ...) are skipped when the repository also has safetensors (`--allow-pickle` keeps them). File paths that would leave the model directory are refused.

## Lockfile

```sh
lmget Qwen/Qwen3-0.6B --lock     # creates ./lmget.lock, or adds the model to it
lmget sync                       # reproduce it: same commits, every file checked against the locked sha256
lmget sync --update              # resolve the revisions again and rewrite the lock
```

`lmget.lock` pins each model to a commit and every file to its size and sha256, together with the selection (`--include`, `--exclude`) and the target. Once a lockfile exists in the current directory, `lmget get` keeps it current. `sync` stops with exit code 5 if the source no longer matches the lock. When every locked file is already in the store, `sync` does not use the network.

## Managing the store

```sh
lmget ls                         # models, sizes, where they are exported
lmget rm Qwen/Qwen3-0.6B         # remove it and free the blobs no other model uses
lmget doctor                     # store, export targets, network, token, lockfile
lmget doctor --verify            # also re-hash every blob in the store
```

`rm` removes only the links lmget created that still point at its data; files the user replaced, and downloads made by other tools, are left in place. The space reported as freed is measured from the blobs' link counts, so space still held by other hard links is reported separately. `doctor --verify` moves a corrupt blob to `quarantine/` in the store, together with lmget's hard links to it, so the next `get` or `sync` downloads it again. `doctor` never prints the token, only where it was found.

## Direct URLs

Only https is accepted, and URLs with credentials (`user:pass@`) are refused. The expected sha256 comes from, in order: a `#sha256=` fragment you add, a `Repr-Digest` or `Digest` header from the server, or, when neither exists, the first download, which is then pinned in `lmget.lock` and shown as "pinned on first download". A partial download is bound to the server's `ETag` (or `Last-Modified`) and every ranged request carries `If-Range`: if the file changes on the server, lmget starts over instead of joining two versions. URLs with a query string (signed URLs carry credentials there) are downloaded but never written to `lmget.lock`. lmget sends no token to hosts other than the Hugging Face endpoint.

Private and gated models use `HF_TOKEN` or the token saved by `huggingface-cli login`. `HF_ENDPOINT` selects a mirror. The token is sent only to the configured endpoint.

## Output for scripts and integrations

On a terminal lmget draws a live view. When stdout is not a terminal it writes stable `lmget: ...` log lines to stderr and the resulting path to stdout:

```sh
dir=$(lmget Qwen/Qwen3-0.6B --to dir:./models/qwen3)
```

`--json` writes one JSON event per line to stdout. Every event has `v` (schema version, currently 1), `type` and `time`. Numeric fields are omitted when zero.

| type | fields |
|---|---|
| `resolve` | `ref` |
| `plan` | `plan`: `ref`, `commit`, `license`, `gated`, `hash_from` (URLs: `user`, `server`, `first`), `files[]` (`path`, `size`, `sha256`, `git_sha1`, `cached`, `skipped`), `bytes`, `to_fetch`, `pickle`, `disk_free` |
| `file_start` | `file`, `total` |
| `progress` | `file`, `bytes`, `total` (at most 4 per second per file) |
| `file_done` | `file`, `sha256`, `total`, `cached` |
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
