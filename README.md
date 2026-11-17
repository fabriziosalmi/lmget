<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="site/lmget-dark.svg">
    <img alt="lmget" src="site/lmget-light.svg" width="212" height="64">
  </picture>
</p>

<p align="center">Get open models from their sources, verified.<br><a href="https://lmget.org">lmget.org</a></p>

lmget resolves a model, shows what it is about to download, fetches it with resumable parallel requests, checks every file against the hash the source publishes (for a direct URL that publishes none, the hash you give or the one recorded at the first download), and links it where your tools already look for it.

Status: early development. Sources in this build: Hugging Face (with ModelScope as a second origin), the Ollama registry, OCI registries (Docker Hub's `ai/` models, ghcr.io, ...), and direct https URLs.

## Install

```sh
curl -fsSL https://lmget.org/install.sh | sh
```

For macOS and Linux, amd64 and arm64. The [script](site/install.sh) downloads the latest release, checks the archive against the release's `checksums.txt` and, when the GitHub CLI (`gh`) is installed and signed in, its build provenance attestation, then installs `lmget` into `~/.local/bin` (`LMGET_INSTALL_DIR` changes it, `LMGET_VERSION=v0.1.0` picks a release). It uses no sudo and writes nothing else. The archives are also on the [releases page](https://github.com/fabriziosalmi/lmget/releases), and `go install github.com/fabriziosalmi/lmget/cmd/lmget@latest` builds it from source (a source build reports its version as `dev`). The maintainer tools `lmget-index` and `lmget-node` are not in the archives: build them from source.

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
lmget qwen                                     # a nickname: the Qwen3.5 variant that fits this machine (see below)
```

`lmget get <model>` is the same as `lmget <model>`. When the current directory holds an `lmget.lock`, `lmget get` adds the model to it or updates its entry, even without `--lock` (see [Lockfile](#lockfile)).

What happens on every run:

- **Plan first.** The revision is resolved to a commit, and every file is listed with its size and published hash before a byte is downloaded.
- **Verified.** Large files (stored with Git LFS) are checked against their sha256; small files against their git blob id (the hash git gives a file), and their sha256 is recorded. A mismatch discards the partial file and exits with code 5.
- **Resumable.** Interrupt at any time; the same command resumes from the last completed chunk. A server that ignores range requests (some direct URLs and plain file servers used as origins) sends the whole file again.
- **Content-addressed.** Files are stored once by sha256 in `~/.lmget` (or `$LMGET_HOME`) and hardlinked into the target, so nothing is copied twice (across filesystems, where a hard link is impossible, the target gets a symlink to the store).
- **Checked by content, not only by hash.** Before a file is stored or exported, its header is checked the way the reference loader reads it, and pickle content that runs code is refused under any other name (see [Content checks](#content-checks)). Refused files exit with code 5.
- **Enough disk first.** A download that would leave less than 512 MiB free on the store's disk does not start (exit code 7).
- **Safer defaults.** Pickle weights (`.bin`, `.pt`, `.pth`, `.ckpt`, `.ot`, `.joblib`, `.pkl`, `.pickle`) are skipped when the repository lists any `.safetensors` file, even one that `--include` or `--exclude` leaves out (`--allow-pickle` keeps them). A `.bin` next to a `.xml` of the same name is OpenVINO weights, not a pickle. File paths that would leave the model directory, that contain control characters, that contain invisible code points (which some filesystems drop, so two names could land on one file), or that pass through `.git` or a name a filesystem folds onto it, are refused, whatever the source. Sizes above 16 TiB are refused.

Flags and environment:

- `--to hf` links into the Hugging Face cache (the default for Hugging Face models); `--to ollama` into Ollama's models directory (the default for Ollama models, and for Ollama models only); `--to omlx` and `--to lmstudio` into those apps' models directories (see below); `--to dir:PATH` into a plain directory; `--to none` keeps the files in the store only.
- The Hugging Face cache is `$HF_HUB_CACHE`, else `$HF_HOME/hub`, else `$XDG_CACHE_HOME/huggingface/hub`, else `~/.cache/huggingface/hub`.
- `--dry-run` stops after the plan, as `lmget info` does.
- `--conns N` sets the parallel connections per file (default 8).
- `--lockfile PATH` uses another lockfile than `./lmget.lock`.
- `--plain` writes plain log lines even on a terminal, and so does `TERM=dumb`; `--json` writes events (see below).
- `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` are honored for every request.
- `-y` is short for `--yes`; `lmget completion` prints shell completion scripts.
- `lmget --version` prints the version.

## Content checks

A matching hash proves the bytes are what the source publishes, not that they are what the name says. Before a file is stored or exported under its name, its header is walked the way the reference loader reads it, and refused where that loader refuses it: for `.safetensors`, the JSON header must describe every tensor with a known size and the tensors must tile the data exactly, with no gap, overlap or trailing bytes (as the `safetensors` library checks); for `.gguf` versions 2 and 3, counts, strings and arrays must fit in the file, at most 65,536 tensors and 65,536 keys, keys and tensor names must not repeat or contain a NUL byte, and the tensors must follow one another from the aligned data section within the file (as llama.cpp checks), up to the first tensor type lmget cannot size: the tensors after it are not placed. A `.gguf` file of another version is only checked for the GGUF magic. Header fields that point past the file are how a crafted model attacks the program that opens it. A dtype or tensor type newer than lmget is not refused; it is only not sized. Python pickle content that runs code when loaded is refused unless the file has a pickle name (listed below). Pickles are detected by walking their opcodes (protocols 0 to 5) for one that imports or calls code, and by the protocol 2 to 5 header; a zip with a `.pkl` entry, as `torch.save` writes, counts as one, and so does a zip lmget cannot read or whose directory is larger than 16 MiB, or such a zip appended after another format's header. Text files (`.json`, `.txt`, `.md`, `.yaml`, `.yml`, `.jinja`, `.html`, `.csv`, `.tsv`, `.cfg`, `.ini`, `.toml`, `.py`) are not inspected for pickles, so a zip appended to a text file is not detected; only a text file that starts as a zip is checked. Refused files exit with code 5.

## Nicknames

```sh
lmget qwen                  # proposes a Qwen3.5 variant for this machine and asks
lmget qwen --yes            # takes the proposal without asking
lmget info qwen             # shows the proposal and its plan, downloads nothing
lmget qwen --for 4GB        # a variant whose weights fit in 4 GB
lmget qwen --format gguf    # or safetensors, mlx, ollama
lmget qwen --to ollama      # for that app
```

A lowercase name of letters, digits, `.`, `_` and `-`, without `/` or `:`, is a nickname (the names of lmget's commands excepted). lmget reads this machine (memory, Apple Silicon, an NVIDIA GPU through `nvidia-smi`, a container's memory limit) and the apps installed (oMLX, LM Studio, Ollama), then proposes one variant:

- The target is the first installed app, in the order oMLX (Apple Silicon only), LM Studio, Ollama, for which the family has a variant that fits; else the Hugging Face cache. `--to` sets it.
- The format is the best one the target loads that has a variant that fits, in this order: oMLX: MLX, then safetensors; LM Studio: MLX then GGUF on Apple Silicon, GGUF elsewhere; Ollama: the registry's models; the Hugging Face cache: safetensors, then MLX (Apple Silicon only), then GGUF. `--format` sets it.
- The size is the largest variant whose weights fit the memory budget, and the disk: 60% of the memory on Apple Silicon, 85% of an NVIDIA GPU's memory for safetensors and GGUF, 50% of the memory otherwise. `--for` sets the budget.
- The choice depends on the machine, not on what is free at the moment, so it is the same every time. When the memory free now is below the model's weights, lmget says so. It also names the next size up.

Nothing is downloaded without a yes: on a terminal lmget asks, `--yes` answers in advance, and without a terminal (a script, CI, or `--json`) it stops with exit code 2 and prints the concrete command, because a script should name its model. Answering no also exits with code 2. `--for`, `--format` and `--yes` with a model that is not a nickname exit with code 2. `lmget.lock` records the concrete model and commit, never the nickname, so `lmget sync` reproduces the same files on any machine.

The project's nicknames are `aliases.json` next to the index, signed by the same keys; `lmget index` fetches it, and a nickname refreshes it once a day, using the cached copy when that fails. It is refused when the signature is not valid, when it is older than one already accepted, and when it has expired. Every Hugging Face variant in it was resolved at its source when the list was built: not gated, a license on the allowlist, no pickle selected, no code from the repository needed to load it (no `auto_map` in `config.json`), and files that match its format. An Ollama variant was resolved on the registry and carries the license the maintainer declares, since the registry gives none. Variants name a repository, not a commit: a nickname downloads what the repository holds today, verified as any download, and the lock pins the commit it got. The list is built from [`aliases.toml`](aliases.toml) in this repository.

Your own nicknames go in `aliases.toml` in the store (`~/.lmget/aliases.toml`, or `$LMGET_HOME/aliases.toml`), and win over the project's:

```toml
[aliases]
mine = "hf:mlx-community/Qwen3.5-4B-4bit"
asr  = "openai/whisper-small"
```

## oMLX and LM Studio

```sh
lmget mlx-community/Qwen3.5-0.8B-4bit --to omlx
lmget unsloth/Qwen3.5-4B-GGUF --include '*Q4_0*' --to lmstudio
```

Both apps keep models as `<models directory>/<org>/<name>`, and lmget links the files there (hard links on the same disk, symlinks into the store otherwise). Only Hugging Face models go there, and a target that cannot hold the selection is refused before anything is downloaded.

- oMLX: the directory is `$LMGET_OMLX_DIR`, else `model.model_dir` in `~/.omlx/settings.json` (only that key is read), else `~/.omlx/models`; both must be absolute, `~/` is expanded. oMLX finds a model by its `config.json` and `*.safetensors`, so a selection without them is refused. Checked with oMLX 0.8.0rc1 on 2026-11-17: it found a model lmget exported and listed it by its directory name (`Qwen3.5-0.8B-4bit`); its log also showed it listing MLX models from the Hugging Face cache as `<org>--<name>`.
- LM Studio: the directory is `$LMGET_LMSTUDIO_DIR` (absolute, `~/` expanded), else `~/.lmstudio/models`. A directory moved in the app is kept in no documented file, so set the variable then. The layout follows LM Studio's documentation; it has not been checked against the app yet.

A model in an app directory has one revision at a time: updating it replaces the links lmget made and removes the ones the new revision no longer has, so the app never loads two revisions mixed. Files that lmget did not create are left alone, and one in the way stops the export. `lmget rm` removes only lmget's links.

## Ollama

```sh
lmget ollama:qwen3:0.6b                        # name[:tag], tag latest by default
lmget ollama:huihui_ai/qwen3-abliterated:0.6b  # namespace/name:tag
lmget https://ollama.com/library/qwen3:0.6b    # URLs work too
lmget ollama:qwen3:0.6b --to dir:./models/qwen3
```

lmget reads the manifest of the tag from `registry.ollama.ai` and identifies the model by the sha256 of that manifest, which is the commit shown in the plan and pinned in `lmget.lock`. Every layer is a file of the plan, named after its type (`model.gguf`, `template`, `system`, `params.json`, `license`, ...), and verified against the sha256 digest the manifest gives for it; `manifest.json` itself is always part of the download. The model layer must be a GGUF file.

Ollama models are exported by default to Ollama's models directory (`$OLLAMA_MODELS`, else `~/.ollama/models`): the layers are linked as `blobs/sha256-<digest>` (hard links on the same disk) and the manifest is written as the tag file, as a copy: a tool that rewrites the tag file cannot change the store. The Ollama export needs every layer, so `--include` and `--exclude` work only with `--to dir:` or `--to none`; otherwise lmget stops with exit code 2 before downloading. `lmget rm ollama:<name>:<tag>` removes only what lmget wrote: blobs and tag files that Ollama downloaded itself are left in place, and so is a layer that another tag still names, whoever wrote that tag.

The registry serves manifests by tag only. `sync` therefore rebuilds a locked Ollama model from the manifest an earlier pull verified for that same name, and fetches missing layers by digest even after the tag has moved; without such a manifest in the store, the tag must still point at the locked one, or `sync` stops with exit code 5. Only public models are supported, and no token is sent to the registry.

## Signed index and nodes

The project keeps a signed index of models it mirrors: for each, the reference, the commit and every file with its size and sha256, and the nodes that serve those files by hash. `lmget index` fetches it, verifies its minisign signature against the index keys in force (the built-in project key until a key list is accepted, then the keys that list names, see below), refuses an index older than the highest one it ever accepted (even after that one expired), an index past its expiry date, and an index valid for more than 60 days, and keeps it in the store. The index lists Hugging Face models only, so the nodes stand in for Hugging Face, not for Ollama, OCI or URL sources. It is published at `https://lmget.org/index.json`; `LMGET_INDEX_URL` points lmget at another index (https only), and `LMGET_INDEX_KEY` adds one index key, trusted next to the others and never in place of them. A store keeps one record of the highest accepted sequence, whatever the URL the index came from.

Two kinds of keys sign it. The root key (key number `736BC89C91E98921`, built into lmget) stays offline and signs one file: `keys.json`, next to the index, which lists the keys allowed to sign the index and the keys that are revoked. While a valid key list is known, the index keys are exactly the ones it lists, plus a key given in `LMGET_INDEX_KEY`. Until a store has accepted a key list, lmget trusts the built-in project key `21C7BDA090E969F4`; once it has, the built-in key is not trusted again, so when the list in force expires `lmget index` must fetch a current one before any index verifies, except one signed by the `LMGET_INDEX_KEY` key. An index signed by the root key, by a revoked key or by a key the list does not name is refused, and the error says which key signed it. Revocations and the highest accepted key list are kept in the store for good: a revoked key is never trusted again there, even after the list expires, and an older list cannot bring it back. A key list is valid for at most 400 days, and a list older than the highest one accepted is never in force; one that cannot be reached leaves the keys in force, one with a bad signature or content stops `lmget index`. `LMGET_ROOT_KEY` adds a root key for a self-hosted index. `lmget index` shows the key that signed the index and the key list in force.

The index can also revoke blobs: content the project found malicious or mislabeled, listed by sha256 with a reason. lmget refuses a revoked blob from any origin, before downloading it when its sha256 is known (`lmget info` says so too) and before it enters the store otherwise, and refuses to export one already stored; `lmget doctor` reports revoked blobs in the store and the models that use them. Revocations are recorded in the store when their index is accepted: they hold after the index expires and after the key that signed it is rotated out or revoked, until a newer accepted index withdraws them. A record that cannot be read stops the pull. No model of the index may use a revoked blob, so nodes never serve one. Revocations reach a machine through `lmget index`, and through the fetch a failing source triggers: run `lmget index` now and then.

The nodes are the last origin of a file: after the source (and, for Hugging Face, ModelScope) failed. When that happens and the store holds no valid index (a new store, or an expired copy), lmget fetches the index then, once per run: from the index URL and, for the project's index only, from `node1.lmget.org` and `node2.lmget.org` (built into lmget), which serve the copy they verified; the signature decides, not the host. Fetching and verifying it took 0.49 to 0.65 s in three runs on 2026-11-17. When the source cannot be reached at all, or no longer has the model, the plan of an indexed model comes from the index, because its file list is signed. Access refusals (gated, private) are never routed to the nodes, and every byte is verified against the signed hash. A node that cannot be reached, answers with an error (any status but 401, 403, 404 and 410) or serves other bytes is skipped for the rest of the run, up to 10 minutes; a node that lacks one file (404) is still asked for the others.

Maintainer tools sit next to the CLI (build them from source: `go build ./cmd/lmget-index ./cmd/lmget-node`):

- `lmget-index build -lock lmget.lock -sequence N -node https://...` writes the index from a lockfile. Every model is resolved again at its source: it must not be gated, the license the source declares must be on the allowlist (`apache-2.0`, `mit`, `bsd-2-clause`, `bsd-3-clause`, `isc`, `cc0-1.0`, `cc-by-4.0`, `unlicense`, in any case; when the source declares several licenses, each must be on it), and the lock must list every file the source publishes at the locked commit, with the source's own sizes, sha256 and git ids (an index entry is a whole model, never a selection; pickle weights next to safetensors only with `allow_pickle`). The builder then downloads every file into its own store, pinned to the lock (so a lock cannot put a wrong sha256 under the signature), and computes each blob's torrent identity. Sign the output with `minisign -S`. `-revoked FILE` adds revoked blobs, one `<sha256> <reason>` per line. `-node` is repeatable, `-valid` sets how long the index stays valid (default `720h`; lmget refuses more than 60 days) and `-o` the output file (default stdout).
- `lmget-index keys -sequence N -index-key lmget-index.pub [-revoke NUMBER]` writes `keys.json`, checked as lmget will check it (a root key can be neither listed nor revoked, a listed key cannot be revoked). `-index-key` and `-revoke` are repeatable, `-valid` defaults to `8760h` (at most 400 days) and `-o` names the output file (default stdout). Sign it with the root key and publish it next to the index.
- `lmget-index aliases -in aliases.toml -sequence N` writes `aliases.json`, the project's nicknames (see [Nicknames](#nicknames)), resolving every variant at its source. `-valid` defaults to `720h` (at most 60 days) and `-o` names the output file (default stdout). Sign it with an index key and publish it next to the index.
- `lmget-node mirror` (no arguments) mirrors the models of the index, pinned to its commits and files (a source that drifted from the index is refused); `lmget-node serve` serves the index and only the blobs it names, with ranges, behind a TLS reverse proxy (`-listen`, default `127.0.0.1:8080`); `lmget-node seed` seeds the same blobs as BitTorrent v2 torrents and announces them on the Mainline DHT, re-reading the index every minute (`-port`, default any free port; `-no-dht` does not announce on the DHT). Both stop offering a blob once the index no longer names it, and all of them once the index expires; from 7 days before the expiry they say so in their log. [deploy/lmget-node](deploy/lmget-node) runs a node in a Proxmox container with no inbound port, behind a Cloudflare Tunnel.

Every blob is its own BitTorrent v2 torrent: its name and file are its sha256 and the piece length is fixed at 4 MiB, so its infohash depends only on its content. The index carries that infohash (`btih2`) for every file, so any BitTorrent v2 client can fetch a mirrored blob from nodes that run `lmget-node seed` and from other seeders. The `lmget` CLI itself does not include a torrent client: it downloads from the nodes over HTTP and stays small.

## Lockfile

```sh
lmget Qwen/Qwen3-0.6B --lock     # creates ./lmget.lock, or adds the model to it
lmget sync                       # reproduce it: same commits, every file checked against the locked sha256
lmget sync --update              # resolve the revisions again and rewrite the lock
lmget sync Qwen/Qwen3-0.6B       # only the models named, which must be in the lock
```

`lmget.lock` pins each model to a commit (for Ollama and OCI models, the sha256 of the manifest) and every file to its size and sha256, together with the selection (`--include`, `--exclude`) and the target. Once a lockfile exists in the current directory, `lmget get` keeps it current. Concurrent lmget runs update it one at a time, and `sync` never writes over an entry that another run changed while it was downloading. `sync` stops with exit code 5 if the source no longer matches the lock. When an earlier pull on this machine verified every locked file of a model against the source, at the locked commit, and the blobs are still in the store, `sync` does not use the network for that model.

The lock also records the license the source declared. If a later `get` or `sync --update` finds another license (or none), lmget stops before downloading, with exit code 5 and the two licenses side by side. Read the new terms, then run the same command with `--accept-license-change` to go on and record the new license. The index builder refuses a changed license too, with exit code 2 and no override: lock the model again after reading the new license.

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
lmget rm Qwen/Qwen3-0.6B@<commit> # remove only that commit (at least 7 characters); without it, every stored commit goes
lmget doctor                     # store, Hugging Face cache, Hugging Face endpoint, token, revoked blobs, lockfile
lmget doctor --verify            # also re-hash every blob in the store
```

`rm` removes only the links lmget created that still point at its data; files the user replaced, downloads made by other tools, and shared files another model still uses (a blob another commit's snapshot links to, a file another model exported to the same directory) are left in place. `rm` then deletes every blob in the store that no stored model references, not only this model's. The space reported as freed is measured from the blobs' link counts, so space still held by other hard links is reported separately. `doctor --verify` moves a corrupt blob to `quarantine/` in the store and removes lmget's hard links to it, so the next `get` or `sync` downloads it again. `doctor` never prints the token, only where it was found.

## Direct URLs

Only https is accepted, and URLs with credentials (`user:pass@`) are refused. The expected sha256 comes from, in order: a `#sha256=` fragment you add, a `Repr-Digest` or `Digest` header from the server, or, when neither exists, the first download, shown as "pinned on first download" and recorded in `lmget.lock` when there is one (`--lock`, or a lockfile in the directory); without a lock, the next run downloads and records it again. A fragment that disagrees with the server's header stops the run with exit code 5. A partial download is bound to the server's strong `ETag`, else to its `Last-Modified`, and every ranged request carries it in `If-Range`: if the file changes on the server during a download, the run stops with exit code 6 and the next run starts over, instead of joining two versions. A URL with a query string (signed URLs carry credentials there) is never written to `lmget.lock`: with `--lock`, or with a lockfile in the directory, lmget refuses it with exit code 2 before downloading. lmget sends no token to hosts other than the Hugging Face endpoint, and a redirect from https to http is refused.

## OCI registries

```sh
lmget oci://docker.io/ai/smollm2               # Docker Hub's ai/ models, tag latest by default
lmget oci://docker.io/ai/smollm2:360M-Q4_K_M
lmget oci://ghcr.io/org/model@sha256:<64 hex>  # pinned by manifest digest
```

lmget reads the model's manifest (OCI or Docker), checks it against the registry's `Docker-Content-Digest` when the registry sends one, and pins its digest as the commit; `sync` asks for that exact digest, so it reproduces the model even after the tag moved. Every layer is verified against its sha256 digest. Files are named after the manifest's `org.cncf.model.filepath` annotation (CNCF model spec), else `org.opencontainers.image.title`, else, for Docker's AI artifacts, after their type (`model.gguf`, `mmproj.gguf`, `model.safetensors`, `license`, ...); a layer of a type lmget does not know is kept as `layer-<first 12 hex of its digest>`. `config.json` and `manifest.json` are always part of the download. `docker.io/<name>` means `docker.io/library/<name>`. The model is exported to `dir:./<name>` by default. Anonymous pull tokens are renewed while a download runs; only public images are supported, and multi-platform indexes are refused (pin a manifest with `@sha256:`).

## When Hugging Face fails

If a file of a Hugging Face model cannot be downloaded (network errors, 5xx or 429 after the retries, or the file missing), lmget looks for the same repository on ModelScope (`www.modelscope.ai`) and takes that file from there, but only if ModelScope lists the same path with the same size and, for files whose sha256 Hugging Face publishes (LFS), the same sha256. The bytes are verified against the hash Hugging Face publishes (the sha256, or the git id for the other files), so ModelScope is never trusted: a copy that differs is refused with exit code 5. The log says which files came from ModelScope (`from=modelscope` in plain output and in `file_done` events).

ModelScope is asked nothing while Hugging Face works, and never for a file Hugging Face refuses (401, 403): a gated model stays gated. `--no-mirror` turns the fallback off. The plan itself comes from Hugging Face: if Hugging Face cannot be reached, only a model in the signed index can still be planned, from the index.

Private and gated models use `HF_TOKEN`, else the token file at `$HF_TOKEN_PATH`, `$HF_HOME/token` or `~/.cache/huggingface/token` (where `huggingface-cli login` saves it). `HF_ENDPOINT` selects a mirror. The token is sent only to the configured endpoint, and is dropped on a redirect to any other host or port.

## Your own origins

```sh
lmget sync --origin https://cache.example.lan     # or LMGET_ORIGINS="https://a,http://b"; --origin replaces the variable
```

An origin is a server that holds blobs at `<origin>/blobs/sha256/<sha256>`: a LAN cache, a company mirror, an `lmget-node`, or any static file server over another machine's `LMGET_HOME`, whose store uses the same paths (`python3 -m http.server -d ~/.lmget` is enough on a trusted network). lmget asks the origins, in order, for every file whose sha256 the plan knows, before the source; files the source publishes with a git id only (small, non-LFS files) are not asked for, and with a lockfile every file has its sha256. What an origin serves is verified like any download; when no origin serves the right bytes, the file comes from the source, which never resumes an origin's partial chunks. An origin that cannot be reached, answers with an error (any status but 401, 403, 404 and 410) or serves other bytes is skipped for the rest of the run, up to 10 minutes; one that lacks a file (404) is still asked for the others. Origins are separated by commas or spaces. One that refuses the connection costs one request; one that drops packets can cost up to the 15 s connect timeout. A working origin is retried once per chunk (2 attempts). No token is ever sent to an origin or a node. `http` is accepted because integrity rests on the hash, not on TLS; use `https` when the names of the models you fetch are not for the network to see. At most 16 origins are accepted. The log and the `file_done` events name an origin as `origin:` followed by its URL without the scheme. The plan still comes from the source (or the signed index).

## Output for scripts and integrations

When stdout and stderr are both terminals lmget draws a live view. Otherwise it writes stable `lmget: ...` log lines to stderr and the resulting path to stdout; `lmget info` and `--dry-run` print one `size<TAB>hash<TAB>path` row per selected file to stdout, and `ls`, `rm`, `doctor` and `index` print tab-separated rows:

```sh
dir=$(lmget Qwen/Qwen3-0.6B --to dir:./models/qwen3)
```

`--json` writes one JSON event per line to stdout. Every event has `v` (schema version, currently 1), `type` and `time`. An invalid flag is reported as text on stderr even with `--json`. The top-level numeric fields of an event are omitted when zero, and so is `cached` when false; the numeric fields inside `plan` (`bytes`, `to_fetch`, `disk_free`, and `size` of each file) are always present.

| type | fields |
|---|---|
| `nickname` | `name`, then either `ref` and `from` (the user's own nickname and its file) or `kind`, `family`, `machine` (`os`, `arch`, `apple_silicon`, `total_mem`, `avail_mem`, `gpu`, `vram_total`, `vram_free`, `container_cap`), `apps`, `choice` (`variant`, `target`, `budget`, `why[]`, `bigger`, `tight`) and `note` |
| `resolve` | `ref` |
| `plan` | `plan`: `ref` (`source`: `hf`, `ollama`, `oci`, `url`; `repo`, `revision`, `file`, `sha256`), `commit`, `license`, `gated`, `hash_from` (URLs: `user`, `server`, `first`), `files[]` (`path`, `size`, `sha256`, `git_sha1`, `cached`, `skipped`), `bytes`, `to_fetch`, `pickle`, `disk_free`, `from_index` (the plan came from the signed index) |
| `file_start` | `file`, `total` |
| `progress` | `file`, `bytes`, `total` (at most 4 per second per file) |
| `file_done` | `file`, `sha256`, `total`, `bytes` (for a downloaded file), `cached`, `from` (`origin:<origin URL without the scheme>`, `modelscope` or `node:<host>` when the bytes did not come from the source) |
| `export` | `target`, `path` |
| `done` | `total`, `fetched`, `elapsed_s`, `path` |
| `lock` | `path` of the lockfile written after the pull |
| `error` | `error`: `code`, `kind`, `message`, `why`, `fixes[]` (`cmd`, `note`), `cause` (the underlying error) |

`sync --json` emits the same events once per model. `ls`, `rm`, `doctor` and `index` with `--json` write a single object with `v`, `type` and `time`, plus: for `ls` (type `list`), `models` and `store`; for `rm` (type `removed`), `result`; for `doctor` (type `doctor`), `checks`; for `index` (type `index`), `index`, `signed_by` and `keys`, where `index` is the verified index, `signed_by` the key number that signed it, and `keys` holds `list` (the key list in force, absent when none is), `keys` (the key numbers trusted for the index) and `revoked` (how many key numbers the store refuses).

## Exit codes

| code | meaning |
|---|---|
| 0 | success |
| 1 | other failure; for `doctor`, at least one check failed |
| 2 | usage: bad reference, flag, lockfile or nickname, nothing selected, a target that cannot hold the model, a `dir:` target outside the lockfile's directory, a nickname not confirmed |
| 3 | repository, revision or file not found |
| 4 | private or gated: token missing or not granted |
| 5 | verification failed: received bytes do not match the published or locked hash, the content is not what the file name says, a blob the index revoked, the source no longer matches the lock, the license differs from the one in the lockfile, or a signed index, key list or nickname list that does not verify or is older than one accepted |
| 6 | network: source unreachable or failing after retries |
| 7 | disk: not enough space, or a write failed |
| 130 | interrupted; run the same command to resume |

## Library

The CLI is a renderer over the Go package `github.com/fabriziosalmi/lmget`: `Client.Pull` emits the same events (except `nickname`, `lock` and `error`, which the CLI writes; `Pull` returns the error), with `progress` up to 10 times per second per file, where the CLI's `--json` keeps at most 4 per second per file.

## License

Apache-2.0. See [LICENSE](LICENSE).
