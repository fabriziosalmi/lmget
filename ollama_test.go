package lmget

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOllama is a registry: manifests by tag, blobs by digest behind a 307
// to a "CDN" path that serves ranges, as registry.ollama.ai does with R2.
type fakeOllama struct {
	mu        sync.Mutex
	tags      map[string][]byte // "library/m:tag" -> manifest
	blobs     map[string][]byte // sha256 -> bytes
	corrupt   map[string]bool   // serve other bytes for these digests
	manifests atomic.Int64      // manifest requests
	blobHits  atomic.Int64      // blob requests at the registry
	auth      atomic.Bool       // an Authorization header arrived
}

func newFakeOllama() *fakeOllama {
	return &fakeOllama{tags: map[string][]byte{}, blobs: map[string][]byte{}, corrupt: map[string]bool{}}
}

type layer struct {
	typ  string
	data []byte
}

// publish builds a manifest the way Ollama's registry serves it (with the
// "from" field real manifests carry) and returns its bytes.
func (f *fakeOllama) publish(name, tag string, layers ...layer) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	desc := func(mt string, b []byte) map[string]any {
		f.blobs[sha(b)] = b
		return map[string]any{"mediaType": mt, "digest": "sha256:" + sha(b), "size": len(b), "from": "/Users/x/.ollama/models/blobs/sha256-" + sha(b)}
	}
	cfg := []byte(`{"model_format":"gguf","model_family":"llama","file_type":"Q8_0"}`)
	var ls []map[string]any
	for _, l := range layers {
		ls = append(ls, desc("application/vnd.ollama.image."+l.typ, l.data))
	}
	m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": ollamaManifestType,
		"config": desc("application/vnd.docker.container.image.v1+json", cfg), "layers": ls})
	f.tags[name+":"+tag] = m
	return m
}

func (f *fakeOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" {
		f.auth.Store(true)
	}
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/v2/") && strings.Contains(p, "/manifests/"):
		f.manifests.Add(1)
		i := strings.Index(p, "/manifests/")
		f.mu.Lock()
		m, ok := f.tags[p[4:i]+":"+p[i+len("/manifests/"):]]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`))
			return
		}
		w.Header().Set("Content-Type", ollamaManifestType)
		w.Write(m)
	case strings.HasPrefix(p, "/v2/") && strings.Contains(p, "/blobs/sha256:"):
		f.blobHits.Add(1)
		http.Redirect(w, r, "/r2/"+p[strings.LastIndex(p, ":")+1:]+"?X-Amz-Signature=x", http.StatusTemporaryRedirect)
	case strings.HasPrefix(p, "/r2/"):
		d := strings.TrimPrefix(p, "/r2/")
		f.mu.Lock()
		b, ok := f.blobs[d]
		if f.corrupt[d] {
			b = bytes.Repeat([]byte{'x'}, len(b))
		}
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		http.ServeContent(w, r, "data", time.Unix(1700000000, 0), bytes.NewReader(b))
	default:
		w.WriteHeader(404)
	}
}

func fakeGGUF(n int) []byte { return append([]byte("GGUF\x03\x00\x00\x00"), randBytes(n)...) }

func smolLayers() []layer {
	return []layer{
		{"model", fakeGGUF(700 << 10)},
		{"system", []byte("You are a helpful AI assistant named SmolLM, trained by Hugging Face")},
		{"template", []byte("{{- if .Messages }}{{- range .Messages }}<|im_start|>{{ .Role }}\n{{ .Content }}<|im_end|>\n{{ end }}{{ end }}<|im_start|>assistant\n")},
		{"license", []byte("Apache License\nVersion 2.0, January 2004\nhttp://www.apache.org/licenses/\n")},
		{"params", []byte(`{"stop":["<|im_start|>","<|im_end|>"]}`)},
	}
}

func newOllamaClient(t *testing.T, srv *httptest.Server, home string) *Client {
	t.Helper()
	t.Setenv("HF_TOKEN_PATH", filepath.Join(t.TempDir(), "none"))
	t.Setenv("HF_HOME", t.TempDir())
	c, err := New(Options{Home: home, HFEndpoint: "https://hf.invalid", HFToken: "hf_secret_for_hf_only",
		OllamaRegistry: srv.URL, HTTP: srv.Client(), Conns: 4, ChunkSize: 128 << 10})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ollamaRef(t *testing.T, s string) Ref {
	t.Helper()
	r, err := ParseRef(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestParseOllamaRefs(t *testing.T) {
	ok := map[string]struct{ repo, tag, canon string }{
		"ollama:smollm2":                             {"library/smollm2", "latest", "ollama:smollm2"},
		"ollama:smollm2:135m":                        {"library/smollm2", "135m", "ollama:smollm2:135m"},
		"OLLAMA:qwen3:0.6b":                          {"library/qwen3", "0.6b", "ollama:qwen3:0.6b"},
		"ollama:huihui_ai/qwen3-abliterated:0.6b":    {"huihui_ai/qwen3-abliterated", "0.6b", "ollama:huihui_ai/qwen3-abliterated:0.6b"},
		"https://ollama.com/library/smollm2:135m":    {"library/smollm2", "135m", "ollama:smollm2:135m"},
		"https://www.ollama.com/huihui_ai/qwen3-abl": {"huihui_ai/qwen3-abl", "latest", "ollama:huihui_ai/qwen3-abl"},
		"ollama:library/smollm2:latest":              {"library/smollm2", "latest", "ollama:smollm2"},
	}
	for in, want := range ok {
		r, err := ParseRef(in)
		if err != nil || r.Source != "ollama" || r.Repo != want.repo || r.Revision != want.tag || r.String() != want.canon {
			t.Errorf("ParseRef(%q) = %+v %q, %v; want %+v", in, r, r.String(), err, want)
			continue
		}
		if back, err := ParseRef(r.String()); err != nil || back != r {
			t.Errorf("%q does not round-trip: %+v, %v", r.String(), back, err)
		}
	}
	for _, in := range []string{"ollama:", "ollama::x", "ollama:a/b/c", "ollama:../x", "ollama:m:", "ollama:m:../x",
		"ollama:m:-x", "ollama:m@sha256:" + strings.Repeat("a", 64), "https://ollama.com/smollm2",
		"https://ollama.com/library/smollm2/tags", "https://ollama.com/library/m?x=1", "ollama:m:" + strings.Repeat("t", 129)} {
		if _, err := ParseRef(in); err == nil || AsError(err).Code != ExitUsage {
			t.Errorf("ParseRef(%q) accepted or wrong code: %v", in, err)
		}
	}
}

func TestOllamaPullExportsWhatOllamaReads(t *testing.T) {
	f := newFakeOllama()
	ls := smolLayers()
	man := f.publish("library/smollm2", "135m", ls...)
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	models := t.TempDir()
	t.Setenv("OLLAMA_MODELS", models)
	c := newOllamaClient(t, srv, t.TempDir())
	out, err := c.Pull(context.Background(), Request{Ref: ollamaRef(t, "ollama:smollm2:135m"), Target: Target{Kind: "ollama"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Plan.Commit != sha(man) {
		t.Fatalf("commit %s, want the sha256 of the manifest %s", out.Plan.Commit, sha(man))
	}
	var names []string
	for _, fl := range out.Plan.Files {
		names = append(names, fl.Path)
	}
	if got := strings.Join(names, ","); got != "config.json,license,manifest.json,model.gguf,params.json,system,template" {
		t.Fatalf("files %s", got)
	}
	// every blob Ollama will look for, with the right bytes
	var m ollamaManifest
	json.Unmarshal(man, &m)
	for _, d := range append([]ollamaDescriptor{m.Config}, m.Layers...) {
		h := strings.TrimPrefix(d.Digest, "sha256:")
		b, err := os.ReadFile(filepath.Join(models, "blobs", "sha256-"+h))
		if err != nil || sha(b) != h {
			t.Fatalf("blob %s missing or wrong: %v", h[:12], err)
		}
	}
	tag := filepath.Join(models, "manifests", hostOf(srv.URL), "library", "smollm2", "135m")
	b, err := os.ReadFile(tag)
	if err != nil || !bytes.Equal(b, man) {
		t.Fatalf("tag file missing or not the manifest bytes: %v", err)
	}
	if fi, _ := os.Stat(tag); fi.Mode().Perm() != 0o644 {
		t.Fatalf("tag file mode %v, want 0644: Ollama rewrites it", fi.Mode().Perm())
	}
	tfi, _ := os.Lstat(tag)
	sfi, _ := os.Stat(c.store.BlobPath(out.Plan.Commit))
	if tfi.Mode()&os.ModeSymlink != 0 || os.SameFile(tfi, sfi) {
		t.Fatal("the tag file is a link to the store: Ollama rewriting it would change the store")
	}
	if sfi.Mode().Perm() != 0o444 {
		t.Fatalf("the stored manifest is %v, want read-only", sfi.Mode().Perm())
	}
	if out.RunAs != hostOf(srv.URL)+"/library/smollm2:135m" {
		t.Fatalf("RunAs %q", out.RunAs)
	}
	if f.auth.Load() {
		t.Fatal("the HF token was sent to the Ollama registry")
	}
	if f.blobHits.Load() < 7 { // the 700 KiB model arrives in ranged chunks, each through the registry
		t.Fatalf("only %d blob requests", f.blobHits.Load())
	}
	// a second pull downloads nothing and leaves the export as is
	before := f.blobHits.Load()
	if _, err := c.Pull(context.Background(), Request{Ref: ollamaRef(t, "ollama:smollm2:135m"), Target: Target{Kind: "ollama"}}, nil); err != nil {
		t.Fatal(err)
	}
	if f.blobHits.Load() != before {
		t.Fatal("a second pull fetched blobs again")
	}
	if c.OllamaName(ollamaRef(t, "ollama:smollm2:135m")) == "smollm2:135m" {
		t.Fatal("a custom registry must keep its host in the name")
	}
	c2, _ := New(Options{Home: t.TempDir()})
	if got := c2.OllamaName(ollamaRef(t, "ollama:smollm2:135m")); got != "smollm2:135m" {
		t.Fatalf("default registry name %q", got)
	}
}

func TestOllamaCorruptLayerIsRefused(t *testing.T) {
	f := newFakeOllama()
	ls := smolLayers()
	f.publish("library/smollm2", "135m", ls...)
	f.corrupt[sha(ls[0].data)] = true
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	models := t.TempDir()
	t.Setenv("OLLAMA_MODELS", models)
	c := newOllamaClient(t, srv, t.TempDir())
	_, err := c.Pull(context.Background(), Request{Ref: ollamaRef(t, "ollama:smollm2:135m"), Target: Target{Kind: "ollama"}}, nil)
	if AsError(err).Code != ExitVerify {
		t.Fatalf("got %v, want exit %d", err, ExitVerify)
	}
	if ents, _ := os.ReadDir(models); len(ents) != 0 {
		t.Fatal("something was exported after a failed verification")
	}
}

func TestOllamaModelLayerMustBeGGUF(t *testing.T) {
	f := newFakeOllama()
	pickle := []byte("\x80\x04\x95\x10\x00\x00\x00\x00\x00\x00\x00\x8c\x02os\x8c\x06system\x93.")
	f.publish("library/evil", "latest", layer{"model", pickle})
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	c := newOllamaClient(t, srv, t.TempDir())
	_, err := c.Pull(context.Background(), Request{Ref: ollamaRef(t, "ollama:evil"), Target: Target{Kind: "ollama"}}, nil)
	if AsError(err).Code != ExitVerify {
		t.Fatalf("got %v, want a pickle as model.gguf refused (exit %d)", err, ExitVerify)
	}
}

func TestOllamaManifestValidation(t *testing.T) {
	c, err := New(Options{Home: t.TempDir(), OllamaRegistry: "https://registry.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	ref := ollamaRef(t, "ollama:m")
	d := func(mt, dg string, size int64) string {
		return fmt.Sprintf(`{"mediaType":%q,"digest":%q,"size":%d}`, mt, dg, size)
	}
	good := "sha256:" + strings.Repeat("a", 64)
	cfg := d("application/vnd.docker.container.image.v1+json", good, 10)
	man := func(layers ...string) []byte {
		return []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":%s,"layers":[%s]}`, ollamaManifestType, cfg, strings.Join(layers, ",")))
	}
	bad := map[string][]byte{
		"digest":       man(d("application/vnd.ollama.image.model", "sha256:../../x", 1)),
		"md5":          man(d("application/vnd.ollama.image.model", "md5:"+strings.Repeat("a", 32), 1)),
		"size":         man(d("application/vnd.ollama.image.model", good, -1)),
		"huge":         man(d("application/vnd.ollama.image.model", good, MaxFileSize+1)),
		"type":         man(d("application/vnd.ollama.image.../../x", good, 1)),
		"foreign type": man(d("application/octet-stream", good, 1)),
		"no layers":    man(),
		"schema":       []byte(strings.Replace(string(man(d("application/vnd.ollama.image.model", good, 1))), `"schemaVersion":2`, `"schemaVersion":1`, 1)),
		"oci index":    []byte(strings.Replace(string(man(d("application/vnd.ollama.image.model", good, 1))), ollamaManifestType, "application/vnd.oci.image.index.v1+json", 1)),
		"json":         []byte(`{"schemaVersion":2,`),
	}
	for name, b := range bad {
		if _, err := c.ollamaPlan(ref, b, sha(b), Selection{}); err == nil {
			t.Errorf("%s: manifest accepted", name)
		}
	}
	// two licenses: two files, no collision
	p, err := c.ollamaPlan(ref, man(d("application/vnd.ollama.image.model", good, 1),
		d("application/vnd.ollama.image.license", "sha256:"+strings.Repeat("b", 64), 1),
		d("application/vnd.ollama.image.license", "sha256:"+strings.Repeat("c", 64), 1)), "x", Selection{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range p.Files {
		names = append(names, f.Path)
	}
	if got := strings.Join(names, ","); got != "config.json,license,license-2,manifest.json,model.gguf" {
		t.Fatalf("files %s", got)
	}
}

func TestOllamaSelectionKeepsTheManifest(t *testing.T) {
	f := newFakeOllama()
	f.publish("library/smollm2", "135m", smolLayers()...)
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	c := newOllamaClient(t, srv, t.TempDir())
	ref := ollamaRef(t, "ollama:smollm2:135m")
	sel := Selection{Include: []string{"model.gguf"}}
	if _, err := c.Pull(context.Background(), Request{Ref: ref, Selection: sel, Target: Target{Kind: "ollama"}}, nil); AsError(err).Code != ExitUsage {
		t.Fatalf("a partial Ollama export: %v", err)
	}
	dir := t.TempDir()
	out, err := c.Pull(context.Background(), Request{Ref: ref, Selection: sel, Target: Target{Kind: "dir", Path: dir}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, ","); got != "manifest.json,model.gguf" {
		t.Fatalf("dir has %s", got)
	}
	if len(out.Plan.Selected()) != 2 {
		t.Fatalf("%d selected", len(out.Plan.Selected()))
	}
}

func TestOllamaLockSyncsOfflineAndAfterTheTagMoves(t *testing.T) {
	f := newFakeOllama()
	ls := smolLayers()
	f.publish("library/smollm2", "135m", ls...)
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	models := t.TempDir()
	t.Setenv("OLLAMA_MODELS", models)
	home := t.TempDir()
	c := newOllamaClient(t, srv, home)
	ref := ollamaRef(t, "ollama:smollm2:135m")
	out, err := c.Pull(context.Background(), Request{Ref: ref, Target: Target{Kind: "ollama"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLock(filepath.Join(t.TempDir(), DefaultLockName))
	if err := l.Upsert(out.Plan, Selection{}, Target{Kind: "ollama"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Write(); err != nil {
		t.Fatal(err)
	}
	l2, err := ReadLock(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if l2.Models[0].Ref != "ollama:smollm2:135m" || l2.Models[0].To != "ollama" || l2.Models[0].Commit != out.Plan.Commit {
		t.Fatalf("lock entry %+v", l2.Models[0])
	}
	sync := func() error {
		req, err := l2.Request(&l2.Models[0], false)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Pull(context.Background(), req, nil)
		return err
	}
	// 1. everything stored: no request at all, and the export comes back
	os.RemoveAll(filepath.Join(models, "manifests"))
	m0, b0 := f.manifests.Load(), f.blobHits.Load()
	if err := sync(); err != nil {
		t.Fatal(err)
	}
	if f.manifests.Load() != m0 || f.blobHits.Load() != b0 {
		t.Fatal("an offline sync used the network")
	}
	if _, err := os.Stat(filepath.Join(models, "manifests", hostOf(srv.URL), "library", "smollm2", "135m")); err != nil {
		t.Fatal("sync did not restore the tag file")
	}
	// 2. the tag moves on and a layer is missing from the store: the verified
	// manifest in the store still names it, and the blob is fetched by digest
	f.publish("library/smollm2", "135m", layer{"model", fakeGGUF(1000)})
	os.Remove(c.store.BlobPath(sha(ls[1].data)))
	if err := sync(); err != nil {
		t.Fatalf("sync after the tag moved: %v", err)
	}
	if !c.store.Has(sha(ls[1].data), int64(len(ls[1].data))) {
		t.Fatal("the missing layer was not fetched again")
	}
	// 3. a fresh store has no verified manifest: the tag must still match
	c3 := newOllamaClient(t, srv, t.TempDir())
	req, _ := l2.Request(&l2.Models[0], false)
	if _, err := c3.Pull(context.Background(), req, nil); AsError(err).Code != ExitVerify ||
		!strings.Contains(AsError(err).Why, "the tag now points at manifest") {
		t.Fatalf("got %v, want drift", err)
	}
}

func TestOllamaLockNeedsItsManifest(t *testing.T) {
	commit := strings.Repeat("a", 64)
	entry := func(c string, files string) string {
		return fmt.Sprintf("version = 1\n[[model]]\nref = \"ollama:m:1b\"\ncommit = %q\nto = \"ollama\"\n%s", c, files)
	}
	file := func(p, s string) string {
		return fmt.Sprintf("[[model.file]]\npath = %q\nsize = 1\nsha256 = %q\n", p, s)
	}
	cases := map[string]string{
		"no manifest":    entry(commit, file("model.gguf", strings.Repeat("b", 64))),
		"other manifest": entry(commit, file("manifest.json", strings.Repeat("b", 64))),
		"git commit":     entry(strings.Repeat("a", 40), file("manifest.json", strings.Repeat("a", 40)+strings.Repeat("0", 24))),
		"short commit":   entry("abc", file("manifest.json", "abc")),
	}
	for name, text := range cases {
		p := filepath.Join(t.TempDir(), DefaultLockName)
		os.WriteFile(p, []byte(text), 0o644)
		if _, err := ReadLock(p); err == nil {
			t.Errorf("%s: lock accepted", name)
		}
	}
	p := filepath.Join(t.TempDir(), DefaultLockName)
	os.WriteFile(p, []byte(entry(commit, file("manifest.json", commit))), 0o644)
	if _, err := ReadLock(p); err != nil {
		t.Fatalf("a valid entry was refused: %v", err)
	}
}

func TestOllamaRemoveLeavesOllamasOwnFiles(t *testing.T) {
	f := newFakeOllama()
	man := f.publish("library/smollm2", "135m", smolLayers()...)
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	models := t.TempDir()
	t.Setenv("OLLAMA_MODELS", models)
	// Ollama already has another model, and its own pull of this tag
	other := filepath.Join(models, "blobs", "sha256-"+strings.Repeat("e", 64))
	os.MkdirAll(filepath.Dir(other), 0o755)
	os.WriteFile(other, []byte("ollama's"), 0o644)
	tag := filepath.Join(models, "manifests", hostOf(srv.URL), "library", "smollm2", "135m")
	os.MkdirAll(filepath.Dir(tag), 0o755)
	os.WriteFile(tag, man, 0o644)
	c := newOllamaClient(t, srv, t.TempDir())
	ref := ollamaRef(t, "ollama:smollm2:135m")
	if _, err := c.Pull(context.Background(), Request{Ref: ref, Target: Target{Kind: "ollama"}}, nil); err != nil {
		t.Fatal(err)
	}
	res, err := c.Remove(ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Models) != 1 || !strings.HasPrefix(res.Models[0], "ollama:smollm2:135m@") {
		t.Fatalf("removed %v", res.Models)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("rm deleted a blob of another Ollama model")
	}
	if b, err := os.ReadFile(tag); err != nil || !bytes.Equal(b, man) {
		t.Fatal("rm deleted the tag file Ollama wrote itself")
	}
	ents, _ := os.ReadDir(filepath.Join(models, "blobs"))
	if len(ents) != 1 {
		t.Fatalf("%d blobs left, want only Ollama's own", len(ents))
	}

	// without Ollama's own tag, rm removes the tag lmget wrote, and keeps the root
	models2 := t.TempDir()
	t.Setenv("OLLAMA_MODELS", models2)
	if _, err := c.Pull(context.Background(), Request{Ref: ref, Target: Target{Kind: "ollama"}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Remove(ref); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(models2); len(ents) != 0 {
		t.Fatalf("rm left %d entries in the models directory", len(ents))
	}
	if _, err := os.Stat(models2); err != nil {
		t.Fatal("rm removed the models directory itself")
	}
}

// A lock cannot make the manifest of one model, verified for that model,
// stand for another name: ollama run trusted would then run evil.
func TestOllamaLockCannotRelabelAnotherModelsManifest(t *testing.T) {
	f := newFakeOllama()
	evil := f.publish("someone/evil", "latest", layer{"model", fakeGGUF(1000)})
	f.publish("library/trusted", "latest", layer{"model", fakeGGUF(1000)})
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	models := t.TempDir()
	t.Setenv("OLLAMA_MODELS", models)
	c := newOllamaClient(t, srv, t.TempDir())
	out, err := c.Pull(context.Background(), Request{Ref: ollamaRef(t, "ollama:someone/evil"), Target: Target{Kind: "none"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLock(filepath.Join(t.TempDir(), DefaultLockName))
	if err := l.Upsert(out.Plan, Selection{}, Target{Kind: "ollama"}); err != nil {
		t.Fatal(err)
	}
	l.Models[0].Ref = "ollama:trusted"
	if err := l.Write(); err != nil {
		t.Fatal(err)
	}
	l2, err := ReadLock(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	req, _ := l2.Request(&l2.Models[0], false)
	_, err = c.Pull(context.Background(), req, nil)
	if AsError(err).Code != ExitVerify {
		t.Fatalf("got %v, want the relabelled lock refused (exit %d)", err, ExitVerify)
	}
	tag := filepath.Join(models, "manifests", hostOf(srv.URL), "library", "trusted", "latest")
	if b, err := os.ReadFile(tag); err == nil && bytes.Equal(b, evil) {
		t.Fatal("ollama:trusted now runs someone/evil")
	}
}
