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

On Windows (amd64 and arm64), in PowerShell:

```powershell
irm https://lmget.org/install.ps1 | iex
```

For macOS and Linux, amd64 and arm64. The [script](site/install.sh) downloads the latest release, checks the archive against the release's `checksums.txt` and, when the GitHub CLI (`gh`) is installed and signed in, its build provenance attestation, then installs `lmget` into `~/.local/bin` (`LMGET_INSTALL_DIR` changes it, `LMGET_VERSION=v0.1.0` picks a release). It uses no sudo and writes nothing else. The archives are also on the [releases page](https://github.com/fabriziosalmi/lmget/releases), and `go install github.com/fabriziosalmi/lmget/cmd/lmget@latest` builds it from source (a source build reports its version as `dev`). The [PowerShell script](site/install.ps1) does the same on Windows and installs `lmget.exe` into `%LOCALAPPDATA%\Programs\lmget` (`LMGET_INSTALL_DIR` changes it); it does not change your PATH, it says how. Both scripts take `LMGET_RELEASE_URL` to download from a mirror (with `LMGET_VERSION`); `checksums.txt` then comes from the mirror too, so only the attestation check proves where the archive was built. The maintainer tools `lmget-index` and `lmget-node` are not in the archives: build them from source. lmget also builds on FreeBSD, OpenBSD and NetBSD, with no release.

With Scoop, from this repository, which is its own bucket (`bucket/lmget.json`, checked by CI with Scoop on Windows Server 2025):

```powershell
scoop bucket add lmget https://github.com/fabriziosalmi/lmget
scoop install lmget
```

With pip or uv, from the releases after v0.3.2: each release carries Python wheels for macOS, Linux (glibc and musl) and Windows, amd64 and arm64, that hold the release binary unchanged and no Python code, with their build provenance attested. Once lmget is on PyPI (published when a release is, by `pypi.yml`):

```sh
uv tool install lmget     # or: pipx install lmget, pip install lmget
```

In a container, from the releases after v0.3.2: `ghcr.io/fabriziosalmi/lmget`, for linux/amd64 and linux/arm64, the release binary on a distroless base (CA certificates, a nonroot user, no shell), with its build provenance attested. The store is `/home/nonroot/.lmget`; mount a volume there to keep it.

```sh
docker run --rm -v lmget:/home/nonroot/.lmget ghcr.io/fabriziosalmi/lmget info Qwen/Qwen3-0.6B
```

In GitHub Actions, the [setup action](setup/action.yml) installs a release, checked against `checksums.txt` and its build provenance (it refuses to install when either fails), and keeps the store between runs, keyed by the hash of `lmget.lock`:

```yaml
- uses: fabriziosalmi/lmget/setup@<commit or tag>
  with:
    version: v0.3.2     # the lmget release to install, or latest
    sync: 'true'        # run lmget sync on ./lmget.lock (lockfile: changes the path)
```

The action is in the repository from the commit after v0.3.2, so `@v0.3.2` does not have it: until the next release, pin a commit of `main` by its SHA. The `version` input is independent of that ref.

On Windows, lmget is tested by CI on Windows Server 2025 (the whole test suite, and `install.ps1` against a good and a bad checksum), not yet on a Windows desktop. There: a model is hard-linked where it is exported when the store and the target are on one drive, and copied otherwise, since symlinks need Developer Mode; lmget checks the free space of that drive before copying, and knows its copies by their bytes: `rm` removes them, an update replaces them, and the Hugging Face snapshot is not copied again. Keeping the store (`LMGET_HOME`) on the drive of your caches avoids the copies. In the Hugging Face cache the snapshot holds the files themselves, as `huggingface_hub` does on Windows without symlinks. Read-only is an attribute shared by all hard links of a file there, and removing one clears it: lmget sets it again on a blob each time it checks it. File paths that Windows would not store as written are refused: device names (`NUL`, `COM1`, ...), `:`, `<>"|?*`, a trailing dot or space, and 8.3 short names such as `GIT~1`.

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

- **Plan first.** The revision is resolved to a commit, and every file is listed with its size and published hash before a byte is downloaded. A Hugging Face repository typed in another case (`qwen/qwen3-0.6b`) is stored under its own spelling (`Qwen/Qwen3-0.6B`), where transformers looks for it.
- **Verified.** Large files (stored with Git LFS) are checked against their sha256; small files against their git blob id (the hash git gives a file), and their sha256 is recorded. A mismatch discards the partial file and exits with code 5.
- **Resumable.** Interrupt at any time; the same command resumes from the last completed chunk. A server that ignores range requests (some direct URLs and plain file servers used as origins) sends the whole file again.
- **Content-addressed.** Files are stored once by sha256 in `~/.lmget` (or `$LMGET_HOME`) and hardlinked into the target, so nothing is copied twice (across filesystems, where a hard link is impossible, the target gets a symlink to the store). An exported file is a hard link to its blob, so a tool that rewrites it in place rewrites the blob: the store keeps each blob's size and modification time, and when that time moves lmget hashes the blob again and moves it to `quarantine/` if its bytes changed, so the next pull fetches it again.
- **Checked by content, not only by hash.** Before a file is stored or exported, its header is checked the way the reference loader reads it, and pickle content that runs code is refused under any other name (see [Content checks](#content-checks)). Refused files exit with code 5.
- **Enough disk first.** A download that would leave less than 512 MiB free on the store's disk does not start (exit code 7).
- **Safer defaults.** Pickle weights (`.bin`, `.pt`, `.pth`, `.ckpt`, `.ot`, `.joblib`, `.pkl`, `.pickle`) are skipped when the repository lists any `.safetensors` file, even one that `--include` or `--exclude` leaves out (`--allow-pickle` keeps them). A `.bin` next to a `.xml` of the same name is OpenVINO weights, not a pickle. File paths that would leave the model directory, that contain control characters, that contain invisible code points (which some filesystems drop, so two names could land on one file), or that pass through `.git` or a name a filesystem folds onto it, are refused, whatever the source. Two files whose names differ only in case or Unicode form are refused before downloading when the target's filesystem treats them as one (macOS and Windows by default). Sizes above 16 TiB are refused.

Flags and environment:

- `--to hf` links into the Hugging Face cache (the default for Hugging Face models); `--to ollama` into Ollama's models directory (the default for Ollama models, and for Ollama models only); `--to omlx`, `--to lmstudio`, `--to jan`, `--to localai`, `--to exo`, `--to colibri` and `--to comfyui` into those apps' models folders (see [Apps](#apps)); `--to dir:PATH` into a plain directory; `--to none` keeps the files in the store only.
- The Hugging Face cache is `$HF_HUB_CACHE`, else `$HF_HOME/hub`, else `$XDG_CACHE_HOME/huggingface/hub`, else `~/.cache/huggingface/hub`.
- `--dry-run` stops after the plan, as `lmget info` does. For a Hugging Face model with a transformers `config.json`, the plan also says whether the model fits this machine: the selected weights plus the KV cache (keys and values, 16-bit) for 8192 tokens (or the model's maximum, if lower), against the memory lmget lets a model use here, the same budget nicknames use (85% of an NVIDIA GPU's memory, 30% of Apple Silicon's unified memory, 50% of the RAM otherwise). Hybrid models count only their full-attention layers; a sliding window makes the figure an upper bound and says so; an architecture whose cache lmget cannot count (such as MLA) gets no figure. Activations, runtime overhead and speed are not estimated. It reads `config.json` only (a few kB, checked against the hash in the plan, never stored), from the store or the source, and not for a plan that a revoked file or the license policy refuses.
- `--conns N` sets the parallel connections per file (default 8).
- `--lockfile PATH` uses another lockfile than `./lmget.lock`.
- `--plain` writes plain log lines even on a terminal, and so does `TERM=dumb`; `--json` writes events (see below).
- `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` are honored for every request.
- `-y` is short for `--yes`; `lmget completion` prints shell completion scripts.
- `lmget --version` prints the version.

## Content checks

A matching hash proves the bytes are what the source publishes, not that they are what the name says. Before a file is stored or exported under its name, its header is walked the way the reference loader reads it, and refused where that loader refuses it: for `.safetensors`, the JSON header must describe every tensor with a known size and the tensors must tile the data exactly, with no gap, overlap or trailing bytes (as the `safetensors` library checks); for `.gguf` versions 2 and 3, counts, strings and arrays must fit in the file, at most 65,536 tensors and 65,536 keys, keys and tensor names must not repeat or contain a NUL byte, and the tensors must follow one another from the aligned data section within the file (as llama.cpp checks), up to the first tensor type lmget cannot size: the tensors after it are not placed. A `.gguf` file of another version is only checked for the GGUF magic. Header fields that point past the file are how a crafted model attacks the program that opens it. A dtype or tensor type newer than lmget is not refused; it is only not sized. Python pickle content that runs code when loaded is refused unless the file has a pickle name (listed below). Pickles are detected by walking their opcodes (protocols 0 to 5) for one that imports or calls code (a stream still well-formed after a million opcodes counts as a pickle, so padding cannot push the call out of reach), and by the protocol 2 to 5 header; a zip with a `.pkl` entry, as `torch.save` writes, counts as one, and so does a zip lmget cannot read or whose directory is larger than 16 MiB, or such a zip appended after another format's header. Text files (`.json`, `.txt`, `.md`, `.yaml`, `.yml`, `.jinja`, `.html`, `.csv`, `.tsv`, `.cfg`, `.ini`, `.toml`, `.py`) are not inspected for pickles, so a zip appended to a text file is not detected; only a text file that starts as a zip is checked. Refused files exit with code 5.

## Nicknames

```sh
lmget qwen                  # proposes a Qwen3.5 variant for this machine and asks
lmget qwen --yes            # takes the proposal without asking
lmget info qwen             # shows the proposal and its plan, downloads no weights
lmget qwen --for 4GB        # a variant whose weights fit in 4 GB
lmget qwen --format gguf    # or safetensors, mlx, ollama
lmget qwen --to ollama      # for that app
```

A lowercase name of letters, digits, `.`, `_` and `-`, without `/` or `:`, is a nickname (the names of lmget's commands excepted). lmget reads this machine (memory, Apple Silicon, an NVIDIA GPU through `nvidia-smi`, a container's memory limit) and the apps installed (oMLX, LM Studio, Ollama), then proposes one variant:

- The target, for a chat model, is the first installed app, in the order oMLX (Apple Silicon only), LM Studio, Ollama, for which the family has a variant that fits; else the Hugging Face cache. Other kinds (transcription, embeddings, reranking, image embeddings) go to the Hugging Face cache, where transformers and sentence-transformers read them. `--to` sets it.
- The format is the best one the target loads that has a variant that fits, in this order: oMLX: MLX, then safetensors; LM Studio: MLX then GGUF on Apple Silicon, GGUF elsewhere; Ollama: the registry's models; the Hugging Face cache: safetensors, then MLX (Apple Silicon only), then GGUF. `--format` sets it.
- The size is the largest variant whose weights fit the memory budget, and the disk: 30% of the memory on Apple Silicon (on a 16 GB Mac, 5.15 GB: an 8B model at 4 bits, 4.35 GB, fits, and a 9B one at 5.98 GB, which does not run there with its context, does not), 85% of an NVIDIA GPU's memory for safetensors and GGUF, 50% of the memory otherwise. `--for` sets the budget.
- The choice depends on the machine, not on what is free at the moment, so it is the same every time. When the memory free now is below the model's weights, lmget says so. It also names the next size up.

Nothing is downloaded without a yes: on a terminal lmget asks, `--yes` answers in advance, and without a terminal (a script, CI, or `--json`) it stops with exit code 2 and prints the concrete command, because a script should name its model. Answering no also exits with code 2. `--for`, `--format` and `--yes` with a model that is not a nickname exit with code 2. `lmget.lock` records the concrete model and commit, never the nickname, so `lmget sync` reproduces the same files on any machine.

The project's nicknames are `aliases.json` next to the index, signed by the same keys; `lmget index` fetches it, and a nickname refreshes it once a day, using the cached copy when that fails. It is refused when the signature is not valid, when it is older than one already accepted, and when it has expired. Every Hugging Face variant in it was resolved at its source when the list was built: not gated, a license on the allowlist, no pickle selected, no code from the repository needed to load it (no `auto_map` in `config.json`), and files that match its format. An Ollama variant was resolved on the registry and carries the license the maintainer declares, since the registry gives none. Variants name a repository, not a commit: a nickname downloads what the repository holds today, verified as any download, and the lock pins the commit it got. The list is built from [`aliases.toml`](aliases.toml) in this repository.

Your own nicknames go in `aliases.toml` in the store (`~/.lmget/aliases.toml`, or `$LMGET_HOME/aliases.toml`), and win over the project's:

```toml
[aliases]
mine = "hf:mlx-community/Qwen3.5-4B-4bit"
asr  = "openai/whisper-small"
```

## lmget run

```sh
lmget run qwen "what model are you?"            # a nickname: proposal, yes, download, ask
lmget run ollama:smollm2:135m "hello"
lmget run mlx-community/Qwen3.5-0.8B-4bit "hi" --to omlx
lmget run Qwen/Qwen3-0.6B "hi" --endpoint http://127.0.0.1:8000   # vLLM, llama-server, ...
echo "summarize this" | lmget run qwen --yes
```

`run` does what `lmget get` does, links the model where an app loads it, and sends the prompt to that running app; lmget itself runs no model. The answer goes to stdout, the reasoning of a model that thinks aloud to stderr, and with `--json` both come as one `answer` event (`app`, `url`, `model`, `text`, `first_token_s`, `total_s`). `--max-tokens` caps the answer (default 512).

- The app: for a nickname, the one the proposal names; for an Ollama model, Ollama; for a Hugging Face model, the first installed app among oMLX and LM Studio that can load the files. `--to` names it. `--endpoint` asks an OpenAI-compatible server instead, and puts the model in the Hugging Face cache, where such servers read it.
- The address: oMLX's `server.host` and `server.port` in its settings (only those keys are read), else `127.0.0.1:8000`, or `LMGET_OMLX_URL`; LM Studio at `127.0.0.1:1234`, or `LMGET_LMSTUDIO_URL`; Ollama at `OLLAMA_HOST`, else `127.0.0.1:11434`. An app is recognised by how it answers, not by its port: a server on that port that is another app is refused. When the address is not this machine, lmget says that the prompt goes there.
- oMLX is recognised by its `/health` answer, without any key; the key you give for LM Studio goes only when LM Studio asks for one; the key for `--endpoint` goes to the address you gave. oMLX discovers models when it starts. After linking a model, lmget asks oMLX to look again (`/admin/api/reload`), which oMLX allows with its API key, given to lmget in `LMGET_OMLX_API_KEY`, or on loopback with its key verification turned off; otherwise lmget says to restart oMLX. lmget never reads another app's keys. `LMGET_LMSTUDIO_API_KEY` and `LMGET_RUN_API_KEY` (for `--endpoint`) are sent when set.
- When the app has not enough memory, lmget says so in the app's words and records it in the history; for 7 days a nickname then proposes a variant smaller than the one that did not fit, and says why. That cap only lowers the machine's budget, and it is not recorded as your choice.
- The prompt and the answer are never written to the history.

Checked live on 2026-11-17 with oMLX 0.8.0rc1 on an M4 Pro with 16 GB: the model was linked, oMLX reloaded and listed it, and its memory guard refused the prompt (it needed about 1.17 GB, the machine had 0.07 GB free); lmget reported it as a memory error. Ollama and LM Studio are covered by tests against simulated servers only.

## Apps

```sh
lmget mlx-community/Qwen3.5-0.8B-4bit --to omlx
lmget unsloth/Qwen3.5-4B-GGUF --include '*Q4_0*' --to lmstudio
lmget unsloth/Qwen3.5-4B-GGUF --include '*Q4_K_M*' --to jan
lmget unsloth/Qwen3.5-4B-GGUF --include '*Q4_K_M*' --to localai
lmget mlx-community/Qwen3-0.6B-4bit --to exo
lmget Justvugg/Qwen3-Coder-30B-A3B-colibri-int4 --to colibri
lmget Comfy-Org/Z-Image-Turbo --include z_image_turbo_bf16.safetensors --include qwen_3_4b.safetensors --include ae.safetensors --to comfyui
lmget Comfy-Org/Flux-1-Schnell --include flux1-schnell.safetensors --to comfyui:diffusion_models
```

lmget links the files into the app's models folder (hard links on the same disk, symlinks into the store otherwise), in the layout the app reads. Only Hugging Face models go there, and a target that cannot hold the selection is refused before anything is downloaded. Each folder can be set with a variable; a path must be absolute, `~/` is expanded.

| `--to` | Folder | Layout | Loads |
|---|---|---|---|
| `omlx` | `$LMGET_OMLX_DIR`, else `model.model_dir` in `~/.omlx/settings.json` (only that key is read), else `~/.omlx/models` | `<org>/<name>/` | `config.json` and `*.safetensors` |
| `lmstudio` | `$LMGET_LMSTUDIO_DIR`, else `~/.lmstudio/models` | `<org>/<name>/` | GGUF; MLX on Apple Silicon |
| `jan` | `$LMGET_JAN_DIR`, else Jan's data folder: `~/Library/Application Support/Jan/data` (macOS), `%APPDATA%\Jan\data` (Windows), `$XDG_DATA_HOME/Jan/data` or `~/.local/share/Jan/data` (Linux) | `llamacpp/models/<org>/<name>/` with a `model.yml` | one GGUF, and at most one `mmproj` file |
| `localai` | `$LMGET_LOCALAI_DIR`, else `$LOCALAI_MODELS_PATH`, else `$MODELS_PATH`, else `~/.localai/models` (the LocalAI launcher's folder) | flat: `<file>.gguf` | GGUF |
| `exo` | `$LMGET_EXO_DIR`, else `$EXO_DEFAULT_MODELS_DIR`, else `~/$EXO_HOME/models`, else `~/.exo/models` (macOS, Windows), `$XDG_DATA_HOME/exo/models` or `~/.local/share/exo/models` (Linux) | `<org>--<name>/` with `.exo-download.json` | `config.json` and `*.safetensors` |
| `colibri` | `$LMGET_COLIBRI_DIR`, else `~/colibri-models` (the folder colibri's setup downloads into without `--dir`) | `<org>--<name>/` with `.colibri-download.json` | `config.json` and `*.safetensors`: a colibri container |
| `comfyui` | `$LMGET_COMFYUI_DIR`, else `<basePath>/models` from ComfyUI Desktop's `config.json` (`~/Library/Application Support/ComfyUI`, `%APPDATA%\ComfyUI`; only that key is read), else comfy-cli's default workspace `~/Documents/comfy/ComfyUI/models` (macOS, Windows) or `~/comfy/ComfyUI/models` (Linux), whichever exists | flat, a folder per kind: `<folder>/<file>` | `*.safetensors` and ComfyUI's other weight files |

- oMLX: checked with oMLX 0.8.0rc1 on 2026-11-17: it found a model lmget exported and listed it by its directory name (`Qwen3.5-0.8B-4bit`); its log also showed it listing MLX models from the Hugging Face cache as `<org>--<name>`.
- LM Studio: a folder moved in the app is kept in no documented file, so set `LMGET_LMSTUDIO_DIR` then.
- Jan: lmget writes the `model.yml` Jan writes when it imports a GGUF (`model_path` relative to the data folder, `name`, `size_bytes`, `model_sha256`, and the `mmproj_` fields), plus the key `lmget_export: true`. Jan rewrites `model.yml` with keys of its own and keeps that one, so lmget still knows the file as its own. A `model.yml` without that key is Jan's, and lmget does not overwrite it.
- LocalAI: the file name is the model name. The `local-ai` command reads its models from `./models` in the folder where it starts, unless `--models-path` or one of the two variables says otherwise: point `LMGET_LOCALAI_DIR` at the same folder.
- exo: `.exo-download.json` is the manifest exo writes after it has verified a download (repository, commit, and each file's path, size and hash); exo takes a folder with a complete manifest as a downloaded model. exo runs the models it has a model card for (`mlx-community/Qwen3-0.6B-4bit` has one), so a repository outside its list is stored but not offered.
- colibri: `.colibri-download.json` is the manifest colibri's setup writes when a download is complete (repository, revision, and each file's path and size); colibri takes a folder with a complete manifest whose files are all there, at their sizes, as a downloaded model. lmget adds the key `lmget_export: true`, which colibri does not read, so it knows its own: a folder colibri downloaded itself is never overwritten nor removed. Start it with `./start-here.sh --model-dir ~/colibri-models/<org>--<name>`, or `COLI_MODEL=~/colibri-models/<org>--<name> ./coli chat`; its setup names the folders of its own catalog by id and does not look for this one. A GGUF goes to `--to dir:PATH`, then `./coli import`, which writes colibri's model folder beside it. A dry run to colibri gives no memory figure: colibri keeps only a model's dense part in memory and streams its experts from the disk, so the whole weights say nothing about whether it runs.
- ComfyUI: workflows name a model by its file name in a folder (`diffusion_models`, `text_encoders`, `vae`, `loras`, `checkpoints`, ...), so each selected weight file is linked flat into its folder, and the rest of the repository stays in the store. The folder comes from `--to comfyui:FOLDER` when given; else from the file's path, where a directory is named as a ComfyUI folder, as in the Comfy-Org repositories (`split_files/vae/ae.safetensors` goes to `vae/`); else from its tensors: a safetensors file with `model.diffusion_model.*` tensors is a full checkpoint (`checkpoints/`), as ComfyUI's checkpoint loader reads one. That is read from the stored file after its hash is verified. Any other file is not guessed: lmget asks for the folder, and the command it suggests downloads nothing again. Two files that would have one name in one folder are refused before the download, and so is a file inside a repository in the diffusers layout (`model_index.json`, a folder per component): ComfyUI loads single files, and that layout's `vae/` is not ComfyUI's. lmget does not find a ComfyUI installed somewhere else, nor the newer Desktop's installations: set `LMGET_COMFYUI_DIR` to its `models` folder.
- The layouts of LM Studio, Jan, LocalAI, exo, colibri and ComfyUI follow their documentation and source code; they have not been checked against the running apps yet. `lmget run` asks oMLX, LM Studio and Ollama; for Jan, LocalAI, exo, colibri and ComfyUI, start the app and ask it.

A model in an app folder has one revision at a time: updating it replaces the links lmget made and removes the ones the new revision no longer has, so the app never loads two revisions mixed. Files that lmget did not create are left alone, and one in the way stops the export. `lmget rm` removes only lmget's links and the `model.yml`, `.exo-download.json` or `.colibri-download.json` it wrote.

Text Generation WebUI keeps its models in `user_data/models` inside its install folder, where it finds a GGUF at any depth and a safetensors model as one folder per model; the plain-directory target fills it (here installed in `~/textgen`):

```sh
lmget unsloth/Qwen3.5-4B-GGUF --include '*Q4_K_M*' --to dir:$HOME/textgen/user_data/models/Qwen3.5-4B-GGUF
```

### Works with

What an app reads, and the lmget target that fills it. Read from each project's source or documentation on 2026-11-18.

- **The Hugging Face cache** (`--to hf`, the default): transformers, sentence-transformers, llama.cpp (`-hf` downloads into it and reads it), Goose's local inference, Lemonade.
- **Ollama** (`--to ollama`): Ollama, RamaLama's `ollama://` models, and the apps that ask Ollama for a model: Open WebUI, Page Assist, Hollama, Oterm, among others.
- **The apps above**: oMLX, LM Studio, Jan, LocalAI, exo, colibri, ComfyUI.
- **A plain directory** (`--to dir:PATH`): Text Generation WebUI (above), and apps given a file path: KoboldCpp (`--model`), llamafile (`-m`), llama-server (`-m`).
- **Through a server**: chat and agent apps that keep no model files of their own (LobeChat, LibreChat, Cherry Studio, Chatbox, SillyTavern, Dify, Langflow, and others) use whatever model the local server they are pointed at (Ollama, LM Studio, oMLX, LocalAI, Jan, a vLLM or llama-server) has, and lmget fills that server's folder.

## Ollama

```sh
lmget ollama:qwen3:0.6b                        # name[:tag], tag latest by default
lmget ollama:huihui_ai/qwen3-abliterated:0.6b  # namespace/name:tag
lmget https://ollama.com/library/qwen3:0.6b    # URLs work too
lmget ollama:qwen3:0.6b --to dir:./models/qwen3
lmget hf.co/unsloth/Qwen3-0.6B-GGUF:Q4_K_M     # a GGUF repository on Hugging Face, for Ollama
```

lmget reads the manifest of the tag from `registry.ollama.ai` and identifies the model by the sha256 of that manifest, which is the commit shown in the plan and pinned in `lmget.lock`. Every layer is a file of the plan, named after its type (`model.gguf`, `template`, `system`, `params.json`, `license`, ...), and verified against the sha256 digest the manifest gives for it; `manifest.json` itself is always part of the download. The model layer must be a GGUF file.

Ollama models are exported by default to Ollama's models directory (`$OLLAMA_MODELS`, else `~/.ollama/models`): the layers are linked as `blobs/sha256-<digest>` (hard links on the same disk) and the manifest is written as the tag file, as a copy: a tool that rewrites the tag file cannot change the store. The Ollama export needs every layer, so `--include` and `--exclude` work only with `--to dir:` or `--to none`; otherwise lmget stops with exit code 2 before downloading. `lmget rm ollama:<name>:<tag>` removes only what lmget wrote: blobs and tag files that Ollama downloaded itself are left in place, and so is a layer that another tag still names, whoever wrote that tag.

The registry serves manifests by tag only. `sync` therefore rebuilds a locked Ollama model from the manifest an earlier pull verified for that same name, and fetches missing layers by digest even after the tag has moved; without such a manifest in the store, the tag must still point at the locked one, or `sync` stops with exit code 5. Only public models are supported from the Ollama registry, and no token is sent to it.

### GGUF repositories on Hugging Face

Hugging Face serves its GGUF repositories to Ollama under the name `hf.co/<org>/<name>:<quantization>`, the one its "Use this model: Ollama" button gives (also accepted as `ollama:hf.co/...`). For that name lmget asks Hugging Face's registry (`huggingface.co/v2/...`) for the manifest Hugging Face builds: the GGUF file as the model layer, with the chat template and the parameters. The model layer's digest is the sha256 Hugging Face publishes for the GGUF file, so a GGUF already in the store is not downloaded again. The model lands in Ollama as `hf.co/<org>/<name>:<quantization>`, where Ollama's own pull of that name puts it, ready for `ollama run`. The Hugging Face token goes to Hugging Face, so gated repositories work once their terms are accepted.

`lmget <org>/<name> --to ollama` on a GGUF repository stops before downloading and gives the `hf.co/...` name to use instead.

Checked on 2026-11-18 with Ollama 0.40.16 on a Mac mini: lmget fetched `hf.co/unsloth/Qwen3-0.6B-GGUF:Q4_K_M` (397 MB, four layers, every digest verified); with its network cut, Ollama listed the model, read the chat template from the GGUF, and answered a chat.

## Signed index and nodes

The project keeps a signed index of models it mirrors: for each, the reference, the commit and every file with its size and sha256, and the nodes that serve those files by hash. `lmget index` fetches it, verifies its minisign signature against the index keys in force (the built-in project key until a key list is accepted, then the keys that list names, see below), refuses an index older than the highest one it ever accepted (even after that one expired), an index past its expiry date, and an index valid for more than 60 days, and keeps it in the store. A lock pinned on a tag or branch (`Org/Name@v1.0`) uses the index too when its commit is listed. The index lists Hugging Face models only, so the nodes stand in for Hugging Face, not for Ollama, OCI or URL sources. It is published at `https://lmget.org/index.json`; `LMGET_INDEX_URL` points lmget at another index (https only), and `LMGET_INDEX_KEY` adds one index key, trusted next to the others and never in place of them. A store keeps one record of the highest accepted sequence, whatever the URL the index came from.

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

The lock also records the license the source declared, and `none` for a Hugging Face model that declares none (a lock written before records nothing there, and that is not compared). If a later `get` or `sync --update` finds another license (or none, or one where there was none), lmget stops before downloading, with exit code 5 and the two licenses side by side. Read the new terms, then run the same command with `--accept-license-change` to go on and record the new license. The index builder refuses a changed license too, with exit code 2 and no override: lock the model again after reading the new license.

### License policy

A project can name the licenses its models may come under, in its lockfile; `LMGET_LICENSES` does the same for a machine, a CI job or an agent (names separated by commas or spaces):

```toml
version = 1

[policy]
licenses = ["apache-2.0", "mit", "none"]
```

The names are the ones the source uses (Hugging Face's license tag: `apache-2.0`, `mit`, `llama3.2`, ...), and `none` stands for a model that declares no license. When both policies are set, a model must be allowed by both. lmget checks the license on the plan, so `get`, `sync` and `info` (and the MCP server, which knows only `LMGET_LICENSES`) refuse a model the policy does not allow before anything is downloaded, with exit code 5 and the policy that refused it. lmget keeps the `[policy]` table as it is when it rewrites the lock; an lmget older than the policy stops on the unknown key rather than ignore it. A policy only restricts: it never lets a model through that lmget would otherwise refuse.

### Bill of materials

```sh
lmget sbom > sbom.cdx.json     # lmget.lock as a CycloneDX 1.6 document (--lockfile picks another)
```

Each model becomes a `machine-learning-model` component: name, commit, license (an SPDX identifier when the source's name maps to one, the name as written otherwise), where it comes from, a package URL for Hugging Face models (`pkg:huggingface/<org>/<name>@<commit>`), and one `file` component per file with its sha256. A signed URL appears without its query. The same lockfile always gives the same bytes: no timestamp, no random serial number. CI checks the output against the official CycloneDX 1.6 schema.

A lockfile usually arrives with a cloned repository, so lmget treats it as untrusted input:

- A pinned `sync` (or a plan from the signed index) fills the Hugging Face cache's `refs/main` only when it is empty, so offline loading finds the snapshot, and never moves it: a cloned lock cannot turn another project's `main` back to an old commit. Only a pull that resolves the revision now moves it.
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

## History

lmget keeps a local record of what it did, `history.jsonl` in the store (`~/.lmget/history.jsonl`, mode 0600): for each pull, the time, the lmget version, the command, the model and commit, the nickname and how its proposal was answered, the target kind, the bytes, the bytes fetched by origin, the time taken and the exit code; and each `rm`. It holds no token, no prompt and no path outside the store, and lmget sends it nowhere. It is used in three ways, all visible:

- The index nodes are asked fastest first, by the median speed measured over pulls of at least 50 MiB that one node served alone.
- A nickname reuses the `--for` and `--format` of its latest accepted, successful pull, and says so ("as you chose on ..."); passing either flag overrides it, and a remembered choice that no longer fits is dropped.
- After `lmget get`, one line points at models last pulled more than 30 days ago when they hold at least 1 GB together: at most once a day, and no more after three showings with no `lmget rm` in between.

`lmget history` lists the last 20 entries and the node speeds measured (`--json` writes all of them as one object of type `history`, with `path`, `entries` and `unreadable`), `lmget history clear` deletes it, and `LMGET_HISTORY=off` (or `0`, `false`, `no`) turns it off: nothing is written or read then. Past 1 MiB the file becomes `history.jsonl.1`, and the one before that is dropped. A line that cannot be read is skipped and counted, never fatal.

## Direct URLs

Only https is accepted, and URLs with credentials (`user:pass@`) are refused. The query string, where signed URLs carry their credentials, is used for the download only: logs, `--json` events, `lmget ls`, the history and the store's records show its values as `REDACTED`, and `lmget rm` finds the model by the full URL. The expected sha256 comes from, in order: a `#sha256=` fragment you add, a `Repr-Digest` or `Digest` header from the server, or, when neither exists, the first download, shown as "pinned on first download" and recorded in `lmget.lock` when there is one (`--lock`, or a lockfile in the directory); without a lock, the next run downloads and records it again. A fragment that disagrees with the server's header stops the run with exit code 5. A partial download is bound to the server's strong `ETag`, else to its `Last-Modified`, and every ranged request carries it in `If-Range`: if the file changes on the server during a download, the run stops with exit code 6 and the next run starts over, instead of joining two versions. A URL with a query string (signed URLs carry credentials there) is never written to `lmget.lock`: with `--lock`, or with a lockfile in the directory, lmget refuses it with exit code 2 before downloading. lmget sends no token to hosts other than the Hugging Face endpoint, and a redirect from https to http is refused.

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

Private and gated models use `HF_TOKEN`, else the one token file `huggingface_hub` reads: `$HF_TOKEN_PATH`, else `$HF_HOME/token`, where `HF_HOME` defaults to `$XDG_CACHE_HOME/huggingface`, then `~/.cache/huggingface` (where `huggingface-cli login` saves it). One file, not a chain: a project or a container that sets its own `HF_HOME` does not fall back to your personal token. `HF_ENDPOINT` selects a mirror. The token is sent only to the configured endpoint, and is dropped on a redirect to any other host or port.

## Your own origins

```sh
lmget sync --origin https://cache.example.lan     # or LMGET_ORIGINS="https://a,http://b"; --origin replaces the variable
```

An origin is a server that holds blobs at `<origin>/blobs/sha256/<sha256>`: a LAN cache, a company mirror, an `lmget-node`, or any static file server over another machine's `LMGET_HOME`, whose store uses the same paths (`python3 -m http.server -d ~/.lmget` is enough on a trusted network). lmget asks the origins, in order, for every file whose sha256 the plan knows, before the source; files the source publishes with a git id only (small, non-LFS files) are not asked for, and with a lockfile every file has its sha256. What an origin serves is verified like any download; when no origin serves the right bytes, the file comes from the source, which never resumes an origin's partial chunks. An origin that cannot be reached, answers with an error (any status but 401, 403, 404 and 410) or serves other bytes is skipped for the rest of the run, up to 10 minutes; one that lacks a file (404) is still asked for the others. Origins are separated by commas or spaces. One that refuses the connection costs one request; one that drops packets can cost up to the 15 s connect timeout. A working origin is retried once per chunk (2 attempts). The Hugging Face token is never sent to an origin or a node, and an origin's redirect is followed within its own host and port only: an origin is someone else's server, and must not send lmget to this machine's own services. `http` is accepted because integrity rests on the hash, not on TLS; use `https` when the names of the models you fetch are not for the network to see. At most 16 origins are accepted. The log and the `file_done` events name an origin as `origin:` followed by its URL without the scheme. The plan still comes from the source (or the signed index).
### A private bucket

```sh
export LMGET_S3_ENDPOINT=https://<account>.r2.cloudflarestorage.com   # or https://s3.<region>.amazonaws.com
export LMGET_S3_ACCESS_KEY_ID=... LMGET_S3_SECRET_ACCESS_KEY=...      # a read-only key is enough
lmget sync --origin s3://models/lmget                                 # or in LMGET_ORIGINS, with other origins
```

An origin `s3://bucket` or `s3://bucket/prefix` is a private bucket on Amazon S3, Cloudflare R2 or another S3 server, holding blobs at `<prefix>/blobs/sha256/<sha256>`: a copy of a store's `blobs/` folder (`aws s3 sync ~/.lmget/blobs s3://bucket/prefix/blobs`, or the same with rclone). lmget signs its requests with AWS Signature Version 4, path-style (`<endpoint>/<bucket>/<key>`), checked against AWS's published test vectors. The endpoint and the keys come from lmget's own variables only, `LMGET_S3_ENDPOINT`, `LMGET_S3_ACCESS_KEY_ID`, `LMGET_S3_SECRET_ACCESS_KEY`, `LMGET_S3_SESSION_TOKEN` for temporary keys, and `LMGET_S3_REGION` (default `auto`, as R2 wants; the bucket's region on S3), never from `AWS_*` or `~/.aws`, which belong to other tools. The endpoint must be `https` (plain `http` only to this machine, for a local test server); a signed request follows no redirect, so the keys and the session token reach the endpoint only. What the bucket serves is verified like any origin's bytes. A missing blob (404, or 403 `AccessDenied`, which S3 answers when the key may not list the bucket) comes from the source; a bucket that refuses the keys themselves (`SignatureDoesNotMatch`, `InvalidAccessKeyId`, a wrong region, a skewed clock, an expired token) stops the pull with that reason, instead of downloading everything from the source unnoticed.

## Sharing on the local network

```sh
lmget serve                          # on one machine: share this store's public models
LMGET_LAN=on lmget Qwen/Qwen3-0.6B   # on the others: find such servers and ask them first
```

`lmget serve` shares, read only, the blobs of the models in its store that are public at their source: a Hugging Face repository that Hugging Face reports as neither private nor gated, or a model of the Ollama registry. Gated and private models, direct URLs, OCI images and GGUF repositories fetched for Ollama are never shared: a colleague gets them under their own access. A model pulled with lmget 0.4.0 or earlier is shared once it is pulled again (nothing is downloaded: the plan now records that it is public). It listens on port 7878 (`--listen` changes the address) at `/blobs/sha256/<sha256>`, and announces itself by mDNS as an `_lmget._tcp` service: on macOS through the system's responder (`dns-sd`), on Linux through Avahi when it is installed, and otherwise by answering the queries itself (`--no-mdns` turns the announcement off). It answers clients on this machine's local networks (and this machine) only, whatever address it listens on: a VPN peer or a host on the internet gets 403, unless `--open`. It serves at most 64 connections at once, and drops a client that has not read anything for 30 seconds. A blob the signed index revoked is never served, and when the store's revocation record cannot be read, nothing is.

With `LMGET_LAN=on`, lmget asks the local network for such servers once per run (for 0.6 s, on every network interface, since a VPN often holds the default route) and uses what it finds as origins, after the ones `LMGET_ORIGINS` names: every byte is verified against the sha256 the plan knows, and the source is asked when a server lacks a file. Discovery is off by default: the query tells the network that this machine is about to download, and the requests that follow tell a server which models. An answer comes from anyone on the network, so lmget keeps at most 4 servers, only at addresses inside this machine's own networks (never loopback, link-local such as the cloud metadata address, a broadcast address or the internet), gives each 3 seconds to answer and a file 30 seconds plus one second per MiB, then asks the next one or the source. lmget's own mDNS responder (Linux without Avahi, other systems) answers queriers on this machine's networks only, at most once a second each.

Checked on 2026-11-18 between a MacBook and a Mac mini on the same LAN, in both directions: the client found the other machine's server by mDNS and took the model's weights (`prajjwal1/bert-tiny`, 17.8 MB) from it, the small files that the source hashes with git only from Hugging Face.

## Output for scripts and integrations

When stdout and stderr are both terminals lmget draws a live view. Otherwise it writes stable `lmget: ...` log lines to stderr and the resulting path to stdout; `lmget info` and `--dry-run` print one `size<TAB>hash<TAB>path` row per selected file to stdout, and `ls`, `rm`, `doctor` and `index` print tab-separated rows:

```sh
dir=$(lmget Qwen/Qwen3-0.6B --to dir:./models/qwen3)
```

`--json` writes one JSON event per line to stdout. Every event has `v` (schema version, currently 1), `type` and `time`. An invalid flag is reported as text on stderr even with `--json`. The top-level numeric fields of an event are omitted when zero, and so is `cached` when false; the numeric fields inside `plan` (`bytes`, `to_fetch`, `disk_free`, and `size` of each file) are always present.

| type | fields |
|---|---|
| `answer` | `lmget run` only: `app`, `url`, `model`, `text`, `first_token_s`, `total_s` |
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

`sync --json` emits the same events once per model. `ls`, `rm`, `doctor`, `index` and `history` with `--json` write a single object with `v`, `type` and `time`, plus: for `ls` (type `list`), `models` and `store`; for `rm` (type `removed`), `result`; for `doctor` (type `doctor`), `checks`; for `index` (type `index`), `index`, `signed_by` and `keys`, where `index` is the verified index, `signed_by` the key number that signed it, and `keys` holds `list` (the key list in force, absent when none is), `keys` (the key numbers trusted for the index) and `revoked` (how many key numbers the store refuses).

## Agents (MCP)

```sh
claude mcp add lmget -- lmget mcp
```

`lmget mcp` speaks the Model Context Protocol on stdin and stdout, for agents such as Claude Code, Cursor or Zed. It offers three tools:

- `info`: the plan of a model, without downloading: commit, license, files with sizes and hashes, what is already stored, the pickle and gated checks, the free disk, and whether it fits this machine's memory when lmget can tell. For a nickname, the variant chosen for this machine and why. It only reads.
- `list`: the models in the store and where each one is linked. It only reads.
- `get`: downloads, verifies and links a model, only when called with `confirm: true`; without it, it returns the plan. It links into the apps and the Hugging Face cache (`to`: `hf`, `ollama`, `omlx`, `lmstudio`, `jan`, `localai`, `exo`, `colibri`, `comfyui`) or keeps the files in the store (`none`), never into a directory: the file names come from the repository, so a folder is chosen on the command line, by a person. A direct URL or an OCI image stays in the store unless `to` names an app. A direct URL or an OCI image is fetched from a public address only: lmget checks the address it connects to (so a name that resolves elsewhere later cannot get around it) and refuses this machine, its networks, link-local addresses and a proxy, which it cannot check through. A message longer than 4 MiB gets a protocol error, and the session goes on.

Arguments lmget does not know are refused. Downloads report progress when the client asks for it, and stop when the client cancels them. Pulls are recorded in the history as `mcp`; the prompt the agent was given is not.

## Exit codes

| code | meaning |
|---|---|
| 0 | success |
| 1 | other failure; for `doctor`, at least one check failed |
| 2 | usage: bad reference, flag, lockfile or nickname, nothing selected, a target that cannot hold the model, a `dir:` target outside the lockfile's directory, a nickname not confirmed |
| 3 | repository, revision or file not found, or a repository with no files at that revision |
| 4 | private or gated: token missing or not granted |
| 5 | verification failed: received bytes do not match the published or locked hash, the content is not what the file name says, a blob the index revoked, the source no longer matches the lock, the license differs from the one in the lockfile, or a signed index, key list or nickname list that does not verify or is older than one accepted |
| 6 | network: source unreachable or failing after retries |
| 7 | disk: not enough space, or a write failed |
| 130 | interrupted; run the same command to resume |

## Library

The CLI is a renderer over the Go package `github.com/fabriziosalmi/lmget`: `Client.Pull` emits the same events (except `nickname`, `lock` and `error`, which the CLI writes; `Pull` returns the error), with `progress` up to 10 times per second per file, where the CLI's `--json` keeps at most 4 per second per file.

## License

Apache-2.0. See [LICENSE](LICENSE).
