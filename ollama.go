package lmget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The Ollama registry speaks the Docker registry v2 protocol: a manifest per
// tag, and blobs addressed by their sha256 digest. A model's commit is the
// sha256 of its manifest, and every layer is a file of the plan named after
// its media type. The manifest itself is the file OllamaManifestFile, so the
// store keeps it, the lock pins it, and an export can be rebuilt offline.

// OllamaManifestFile is the plan file that holds the registry manifest.
const OllamaManifestFile = "manifest.json"

const (
	ollamaManifestType = "application/vnd.docker.distribution.manifest.v2+json"
	ollamaLayerPrefix  = "application/vnd.ollama.image."
	ollamaMaxManifest  = 4 << 20
	ollamaMaxLayers    = 1000
)

var (
	ollamaTagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	ollamaTypeRe   = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,39}$`)
	ollamaDigestRe = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)
	// file names for the layer types runtimes know; other types keep their name
	ollamaLayerNames = map[string]string{
		"model": "model.gguf", "projector": "projector.gguf", "adapter": "adapter.gguf",
		"params": "params.json", "messages": "messages.json",
	}
)

type ollamaDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type ollamaManifest struct {
	SchemaVersion int                `json:"schemaVersion"`
	MediaType     string             `json:"mediaType"`
	Config        ollamaDescriptor   `json:"config"`
	Layers        []ollamaDescriptor `json:"layers"`
}

// ollamaHF is the host Ollama names Hugging Face's registry by: Hugging
// Face serves its GGUF repositories to Ollama as hf.co/<org>/<name>:<quant>,
// with a manifest it builds (the GGUF file as the model layer, the chat
// template and parameters from the file's metadata).
const ollamaHF = "hf.co"

// hfOllamaRepo returns org/name for an Ollama model on Hugging Face.
func hfOllamaRepo(repo string) (string, bool) { return strings.CutPrefix(repo, ollamaHF+"/") }

// parseOllamaRef reads "name", "name:tag", "namespace/name[:tag]" and
// "hf.co/org/name[:quant]". Names without a namespace are in "library", as
// in Ollama.
func parseOllamaRef(in, orig string) (Ref, error) {
	name, tag := in, "latest"
	if i := strings.LastIndex(in, ":"); i >= 0 {
		name, tag = in[:i], in[i+1:]
	}
	if host, rest, ok := strings.Cut(name, "/"); ok && (strings.EqualFold(host, ollamaHF) || strings.EqualFold(host, "huggingface.co")) {
		r, err := validRef("ollama", rest, "main", orig)
		if err != nil {
			return Ref{}, newErr(ExitUsage, "usage", "not a Hugging Face repository for Ollama: "+quote(orig)).
				why("expected hf.co/Org/Name:QUANT").
				fix("lmget hf.co/unsloth/Qwen3-0.6B-GGUF:Q4_K_M", "")
		}
		if !ollamaTagRe.MatchString(tag) {
			return Ref{}, newErr(ExitUsage, "usage", "not a valid quantization tag: "+quote(tag)).
				fix("lmget hf.co/"+rest+":Q4_K_M", "")
		}
		r.Repo, r.Revision = ollamaHF+"/"+r.Repo, tag
		return r, nil
	}
	if !strings.Contains(name, "/") {
		name = "library/" + name
	}
	r, err := validRef("ollama", name, "main", orig)
	if err != nil {
		return Ref{}, newErr(ExitUsage, "usage", "not an Ollama model name: "+quote(orig)).
			why("expected name, name:tag or namespace/name:tag").
			fix("lmget ollama:qwen3:0.6b", "")
	}
	if !ollamaTagRe.MatchString(tag) {
		return Ref{}, newErr(ExitUsage, "usage", "not a valid Ollama tag: "+quote(tag)).
			fix("lmget ollama:"+strings.TrimPrefix(name, "library/")+":latest", "")
	}
	r.Revision = tag
	return r, nil
}

// parseOllamaURL reads https://ollama.com/<namespace>/<name>[:tag].
func parseOllamaURL(u *url.URL, raw string) (Ref, error) {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || u.RawQuery != "" || u.Fragment != "" {
		return Ref{}, newErr(ExitUsage, "usage", "the URL does not point to an Ollama model").
			fix("lmget https://ollama.com/library/qwen3:0.6b", "")
	}
	return parseOllamaRef(parts[0]+"/"+parts[1], raw)
}

// OllamaName is the name Ollama shows for the model of this plan
// (ollama run <name>).
func (c *Client) OllamaName(r Ref) string {
	_, repo, host := c.ollamaRegistry(r)
	name := repo + ":" + r.Revision
	if host == "registry.ollama.ai" {
		return strings.TrimPrefix(name, "library/")
	}
	return host + "/" + name
}

// ollamaRegistry returns where an Ollama model comes from: the registry's
// base URL, the model's path there, and the host Ollama files it under.
// hf.co/... is Hugging Face's registry, at the Hugging Face endpoint; every
// other name is in the Ollama registry.
func (c *Client) ollamaRegistry(r Ref) (base, repo, host string) {
	if rest, ok := hfOllamaRepo(r.Repo); ok {
		return c.opt.HFEndpoint, rest, ollamaHF
	}
	return c.opt.OllamaRegistry, r.Repo, hostOf(c.opt.OllamaRegistry)
}

func (c *Client) resolveOllama(ctx context.Context, ref Ref, sel Selection) (*Plan, error) {
	base, repo, host := c.ollamaRegistry(ref)
	onHF := host == ollamaHF
	u := fmt.Sprintf("%s/v2/%s/manifests/%s", base, repo, url.PathEscape(ref.Revision))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req) // the token goes only to the HF endpoint: Hugging Face's registry, never Ollama's
	req.Header.Set("Accept", ollamaManifestType)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, newErr(ExitNetwork, "network", "cannot reach "+hostOf(base)).
			wrap(err).fix("lmget doctor", "check connectivity")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
	case 404:
		if onHF {
			return nil, newErr(ExitNotFound, "not_found", ref.Display()+" was not found").
				why("no such repository on Hugging Face, or no GGUF file for that quantization in it").
				fix("lmget info "+repo+" --include '*.gguf'", "the GGUF files the repository has")
		}
		return nil, newErr(ExitNotFound, "not_found", ref.Display()+" was not found").
			why("no such model or tag in the Ollama registry").
			fix("", "check the name at https://ollama.com/"+ref.Repo)
	case 401, 403:
		if onHF {
			return nil, newErr(ExitAuth, "auth", ref.Display()+" needs access").
				why("the repository is gated or private").
				fix("", "accept its terms on huggingface.co and set HF_TOKEN")
		}
		return nil, newErr(ExitAuth, "auth", ref.Display()+" is not public").
			why("lmget downloads public Ollama models only")
	case 429:
		return nil, newErr(ExitNetwork, "network", "rate limited by "+hostOf(base)).
			fix("", "wait a minute and run again")
	default:
		return nil, newErr(ExitNetwork, "network", fmt.Sprintf("%s answered %s", hostOf(base), printable(resp.Status)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, ollamaMaxManifest+1))
	if err != nil {
		return nil, newErr(ExitNetwork, "network", "the manifest of "+ref.Display()+" did not arrive").wrap(err)
	}
	if len(body) > ollamaMaxManifest {
		return nil, newErr(ExitFailure, "source", "the manifest of "+ref.Display()+" is larger than 4 MiB")
	}
	sum := sha256.Sum256(body)
	return c.ollamaPlan(ref, body, hex.EncodeToString(sum[:]), sel)
}

// ollamaPlan builds the plan of a manifest whose sha256 is commit.
func (c *Client) ollamaPlan(ref Ref, body []byte, commit string, sel Selection) (*Plan, error) {
	base, repo, _ := c.ollamaRegistry(ref)
	bad := func(format string, a ...any) error {
		return newErr(ExitFailure, "source", "unexpected manifest for "+ref.Display()).why(format, a...)
	}
	var m ollamaManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, bad("not valid JSON: %v", err)
	}
	if m.SchemaVersion != 2 || m.MediaType != ollamaManifestType {
		return nil, bad("schema %d, media type %q", m.SchemaVersion, printable(m.MediaType))
	}
	if len(m.Layers) == 0 || len(m.Layers) > ollamaMaxLayers {
		return nil, bad("%d layers", len(m.Layers))
	}
	p := &Plan{Ref: ref, Commit: commit}
	if _, _, host := c.ollamaRegistry(ref); host == "registry.ollama.ai" {
		p.Access = "public" // lmget takes only public models there, with no token
	}
	p.Files = append(p.Files, File{Path: OllamaManifestFile, Size: int64(len(body)), SHA256: commit, data: body})
	used := map[string]int{OllamaManifestFile: 1}
	add := func(d ollamaDescriptor, name string) error {
		mm := ollamaDigestRe.FindStringSubmatch(d.Digest)
		if mm == nil {
			return bad("invalid digest %q", printable(d.Digest))
		}
		if d.Size < 0 || d.Size > MaxFileSize {
			return bad("invalid size %d for %s", d.Size, mm[1][:12])
		}
		name = uniqueName(used, name) // a second license is license-2, or the next free name
		p.Files = append(p.Files, File{Path: name, Size: d.Size, SHA256: mm[1],
			url: fmt.Sprintf("%s/v2/%s/blobs/sha256:%s", base, repo, mm[1])})
		return nil
	}
	if err := add(m.Config, "config.json"); err != nil {
		return nil, err
	}
	for _, l := range m.Layers {
		typ, ok := strings.CutPrefix(l.MediaType, ollamaLayerPrefix)
		if !ok || !ollamaTypeRe.MatchString(typ) {
			return nil, bad("unknown layer type %q", printable(l.MediaType))
		}
		name := typ
		if n, ok := ollamaLayerNames[typ]; ok {
			name = n
		}
		if err := add(l, name); err != nil {
			return nil, err
		}
	}
	if err := p.apply(sel, c.store, c.store.Root()); err != nil {
		return nil, err
	}
	// the manifest always comes along: it is what identifies the model
	for i := range p.Files {
		if p.Files[i].Path == OllamaManifestFile {
			p.Files[i].Skipped = ""
		}
	}
	p.recount(c.store)
	return p, nil
}

// resolveOllamaPinned builds the plan of a locked Ollama entry. The registry
// serves manifests by tag only, so a manifest a pull verified for this exact
// model is read from the store; otherwise the tag must still point at it.
func (c *Client) resolveOllamaPinned(ctx context.Context, ref Ref, commit string, sel Selection) (*Plan, error) {
	if m, err := c.store.loadManifest(ref, commit); err == nil && m.Verified[OllamaManifestFile] == commit {
		if b, err := os.ReadFile(c.store.BlobPath(commit)); err == nil {
			if s := sha256.Sum256(b); hex.EncodeToString(s[:]) == commit {
				return c.ollamaPlan(ref, b, commit, sel)
			}
		}
	}
	p, err := c.resolveOllama(ctx, ref, sel)
	if err != nil {
		return nil, err
	}
	if p.Commit != commit {
		return nil, newErr(ExitVerify, "lock", "the source does not match the lock for "+ref.Display()).
			why("the tag now points at manifest %s, the lock has %s", p.Commit[:12], commit[:12]).
			fix("lmget sync --update "+ref.Display(), "accept what the source publishes now")
	}
	return p, nil
}

// OllamaModelsDir follows Ollama: $OLLAMA_MODELS, else ~/.ollama/models.
func OllamaModelsDir() string {
	if p := os.Getenv("OLLAMA_MODELS"); p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".ollama", "models")
}

// exportOllama links the layers into Ollama's blobs directory and writes the
// manifest as the tag file, so ollama list and ollama run find the model
// without downloading anything.
func (c *Client) exportOllama(plan *Plan) (string, []string, error) {
	root := OllamaModelsDir()
	if plan.Ref.Source != "ollama" {
		return root, nil, newErr(ExitUsage, "usage", "the Ollama export holds Ollama models only").
			fix("--to dir:PATH", "a plain directory")
	}
	for _, f := range plan.Files {
		if !f.Selected() {
			return root, nil, newErr(ExitUsage, "usage", "the Ollama export needs every layer, "+f.Path+" is not selected").
				fix("", "drop --include and --exclude, or export to --to dir:PATH")
		}
	}
	body, err := os.ReadFile(c.store.BlobPath(plan.Commit))
	if err != nil {
		return root, nil, newErr(ExitFailure, "export", "the manifest is not in the store")
	}
	if s := sha256.Sum256(body); hex.EncodeToString(s[:]) != plan.Commit {
		return root, nil, newErr(ExitVerify, "verify", "the stored manifest does not match its hash").
			fix("lmget doctor --verify", "")
	}
	// a plan built from a lock holds only the locked files: the tag must not
	// name a layer this export does not link
	var m ollamaManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return root, nil, newErr(ExitFailure, "export", "the stored manifest is not valid JSON").wrap(err)
	}
	have := map[string]bool{}
	for _, f := range plan.Files {
		have[f.SHA256] = true
	}
	for _, d := range append([]ollamaDescriptor{m.Config}, m.Layers...) {
		if mm := ollamaDigestRe.FindStringSubmatch(d.Digest); mm == nil || !have[mm[1]] {
			return root, nil, newErr(ExitUsage, "usage", "the Ollama export needs every layer, "+printable(d.Digest)+" is not in the plan").
				fix("", "add the missing layers to the lock, or run lmget sync --update")
		}
	}
	var links []string
	for _, f := range plan.Files {
		if f.Path == OllamaManifestFile {
			continue
		}
		blob := filepath.Join(root, "blobs", "sha256-"+f.SHA256)
		if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
			return root, links, diskErr(err)
		}
		storeBlob := c.store.BlobPath(f.SHA256)
		fi, err := os.Lstat(blob)
		switch {
		case err != nil: // absent
			if err := linkOrSymlink(storeBlob, blob); err != nil {
				return root, links, diskErr(err)
			}
			links = append(links, blob)
		case same(storeBlob, blob): // ours from an earlier run
			links = append(links, blob)
		case fi.Mode().IsRegular() && hasContent(blob, f.Size, f.SHA256):
			// Ollama's own download, with the right bytes: leave it to Ollama
		default: // wrong bytes, wrong size or not a file: serve the verified ones
			os.Remove(blob)
			if err := linkOrSymlink(storeBlob, blob); err != nil {
				return root, links, diskErr(err)
			}
			links = append(links, blob)
		}
	}
	// a copy, not a link: whoever rewrites the tag file must not change the store
	_, repo, host := c.ollamaRegistry(plan.Ref)
	tag := filepath.Join(root, "manifests", host, filepath.FromSlash(repo), plan.Ref.Revision)
	if err := os.MkdirAll(filepath.Dir(tag), 0o755); err != nil {
		return root, links, diskErr(err)
	}
	// identical bytes already there are Ollama's own pull (or an earlier
	// export, already recorded): only a tag written now is added
	if !hasContent(tag, int64(len(body)), plan.Commit) {
		if err := writeFileAtomicMode(tag, body, 0o644); err != nil {
			return root, links, diskErr(err)
		}
		links = append(links, tag)
	}
	return root, links, nil
}
