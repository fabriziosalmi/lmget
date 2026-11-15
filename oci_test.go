package lmget

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// fakeOCI is a registry with Docker Hub's token flow: /v2/ answers 401 with a
// Bearer challenge, /token hands out tokens that stop working after a few
// requests (as Docker Hub's 300 s tokens do over a long download), and blobs
// are redirected to a CDN path that answers ranges.
type fakeOCI struct {
	mu         sync.Mutex
	srv        *httptest.Server
	cdn        *httptest.Server  // another host, as production.cloudfront.docker.com is
	manifests  map[string][]byte // "repo:tag" and "repo@sha256:..." -> body
	blobs      map[string][]byte // "sha256:..." -> bytes
	lieDigest  bool              // Docker-Content-Digest that does not match
	swap       []byte            // served for any request by digest, with no digest header
	uses       map[string]int    // token -> requests served with it
	maxUses    int
	issued     atomic.Int64
	cdnAuth    atomic.Bool // the CDN saw an Authorization header
	hfTokenHit atomic.Bool // the HF token reached the registry
}

func newFakeOCI(t *testing.T) *fakeOCI {
	f := &fakeOCI{manifests: map[string][]byte{}, blobs: map[string][]byte{}, uses: map[string]int{}, maxUses: 1 << 30}
	f.srv = httptest.NewTLSServer(f)
	f.cdn = httptest.NewTLSServer(f) // same handler, /cdn/ paths only
	t.Cleanup(f.srv.Close)
	t.Cleanup(f.cdn.Close)
	return f
}

func (f *fakeOCI) host() string { return strings.TrimPrefix(f.srv.URL, "https://") }

func (f *fakeOCI) blob(b []byte) string {
	d := "sha256:" + sha(b)
	f.blobs[d] = b
	return d
}

// push stores a manifest under repo:tag and repo@digest, and returns its digest.
func (f *fakeOCI) push(repo, tag string, m map[string]any) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := json.Marshal(m)
	s := sha256.Sum256(body)
	d := "sha256:" + hex.EncodeToString(s[:])
	f.manifests[repo+":"+tag] = body
	f.manifests[repo+"@"+d] = body
	return d
}

func (f *fakeOCI) dockerAI(repo, tag string, model, license []byte) string {
	cfg := []byte(`{"config":{"format":"gguf"}}`)
	return f.push(repo, tag, map[string]any{"schemaVersion": 2, "mediaType": ociManifestType,
		"config": map[string]any{"mediaType": "application/vnd.docker.ai.model.config.v0.1+json", "digest": f.blob(cfg), "size": len(cfg)},
		"layers": []map[string]any{
			{"mediaType": "application/vnd.docker.ai.gguf.v3", "digest": f.blob(model), "size": len(model)},
			{"mediaType": "application/vnd.docker.ai.license", "digest": f.blob(license), "size": len(license)},
		}})
}

func (f *fakeOCI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if strings.HasPrefix(p, "/cdn/") {
		if r.Header.Get("Authorization") != "" {
			f.cdnAuth.Store(true)
		}
		http.ServeContent(w, r, "b", time.Time{}, bytes.NewReader(f.blobs[strings.TrimPrefix(p, "/cdn/")]))
		return
	}
	if p == "/token" {
		n := f.issued.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"token": fmt.Sprintf("tok-%d", n), "expires_in": 300})
		return
	}
	auth := r.Header.Get("Authorization")
	if strings.Contains(auth, "hf_secret") {
		f.hfTokenHit.Store(true)
	}
	tok := strings.TrimPrefix(auth, "Bearer ")
	f.mu.Lock()
	ok := tok != "" && strings.HasPrefix(tok, "tok-") && f.uses[tok] < f.maxUses
	if ok {
		f.uses[tok]++
	}
	f.mu.Unlock()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+f.srv.URL+`/token",service="fake"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case p == "/v2/":
		w.WriteHeader(200)
	case strings.Contains(p, "/manifests/"):
		i := strings.Index(p, "/manifests/")
		repo, rev := p[len("/v2/"):i], p[i+len("/manifests/"):]
		key := repo + ":" + rev
		if strings.HasPrefix(rev, "sha256:") {
			key = repo + "@" + rev
		}
		f.mu.Lock()
		body, found := f.manifests[key]
		f.mu.Unlock()
		if !found {
			http.NotFound(w, r)
			return
		}
		d := sha(body)
		if f.swap != nil && strings.HasPrefix(rev, "sha256:") {
			w.Write(f.swap)
			return
		}
		if f.lieDigest {
			d = strings.Repeat("0", 64)
		}
		w.Header().Set("Docker-Content-Digest", "sha256:"+d)
		w.Header().Set("Content-Type", ociManifestType)
		w.Write(body)
	case strings.Contains(p, "/blobs/"):
		http.Redirect(w, r, f.cdn.URL+"/cdn/"+p[strings.LastIndex(p, "/")+1:], http.StatusTemporaryRedirect)
	default:
		http.NotFound(w, r)
	}
}

func newOCIClient(t *testing.T, f *fakeOCI, home string) *Client {
	t.Helper()
	t.Setenv("HF_TOKEN_PATH", filepath.Join(t.TempDir(), "none"))
	t.Setenv("HF_HOME", t.TempDir())
	c, err := New(Options{Home: home, HFEndpoint: "https://hf.invalid", HFToken: "hf_secret_for_hf_only",
		HTTP: f.srv.Client(), Conns: 4, ChunkSize: 128 << 10})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ociRef(t *testing.T, f *fakeOCI, rest string) Ref {
	t.Helper()
	r, err := ParseRef("oci://" + f.host() + "/" + rest)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestParseOCIRefs(t *testing.T) {
	ok := map[string]struct{ repo, rev, canon string }{
		"oci://docker.io/ai/smollm2":                               {"docker.io/ai/smollm2", "latest", "oci://docker.io/ai/smollm2"},
		"oci://docker.io/ai/smollm2:360M-Q4_K_M":                   {"docker.io/ai/smollm2", "360M-Q4_K_M", "oci://docker.io/ai/smollm2:360M-Q4_K_M"},
		"oci://ghcr.io/org/team/model:v1":                          {"ghcr.io/org/team/model", "v1", "oci://ghcr.io/org/team/model:v1"},
		"oci://docker.io/ubuntu":                                   {"docker.io/library/ubuntu", "latest", "oci://docker.io/library/ubuntu"},
		"oci://localhost:5000/m@sha256:" + strings.Repeat("a", 64): {"localhost:5000/m", "sha256:" + strings.Repeat("a", 64), "oci://localhost:5000/m@sha256:" + strings.Repeat("a", 64)},
	}
	for in, want := range ok {
		r, err := ParseRef(in)
		if err != nil || r.Source != "oci" || r.Repo != want.repo || r.Revision != want.rev || r.String() != want.canon {
			t.Errorf("ParseRef(%q) = %+v %q, %v", in, r, r.String(), err)
			continue
		}
		if back, err := ParseRef(r.String()); err != nil || back != r {
			t.Errorf("%q does not round-trip: %+v %v", r.String(), back, err)
		}
	}
	for _, in := range []string{"oci://", "oci://docker.io", "oci://Docker.io/ai/x", "oci://docker.io/AI/x", "oci://docker.io/ai/../x",
		"oci://docker.io/ai/x:", "oci://docker.io/ai/x@md5:00", "oci://docker.io/ai/x@sha256:abc", "oci://-bad/x",
		"oci://docker.io/ai/x:-tag", "oci://docker.io//x"} {
		if _, err := ParseRef(in); err == nil {
			t.Errorf("ParseRef(%q) accepted", in)
		}
	}
}

func TestOCIPullDockerAIModel(t *testing.T) {
	f := newFakeOCI(t)
	model, lic := fakeGGUF(600<<10), []byte("Apache License 2.0")
	digest := f.dockerAI("ai/smollm2", "latest", model, lic)
	f.maxUses = 3 // tokens die mid-download: the client must renew them
	c := newOCIClient(t, f, t.TempDir())
	dir := t.TempDir()
	out, err := c.Pull(context.Background(), Request{Ref: ociRef(t, f, "ai/smollm2"), Target: Target{Kind: "dir", Path: dir}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if "sha256:"+out.Plan.Commit != digest {
		t.Fatalf("commit %s, want the manifest digest %s", out.Plan.Commit, digest)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "model.gguf")); !bytes.Equal(b, model) {
		t.Fatal("model.gguf is not the model layer")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "license")); !bytes.Equal(b, lic) {
		t.Fatal("license is not the license layer")
	}
	if f.issued.Load() < 3 {
		t.Fatalf("only %d tokens issued: renewal on 401 was not exercised", f.issued.Load())
	}
	if f.cdnAuth.Load() {
		t.Fatal("the registry token was sent to the CDN")
	}
	if f.hfTokenHit.Load() {
		t.Fatal("the Hugging Face token was sent to the registry")
	}
}

func TestOCIPullCNCFModelNamesFilesFromTheManifest(t *testing.T) {
	f := newFakeOCI(t)
	w, mm := fakeGGUF(1000), fakeGGUF(500)
	cfg := []byte(`{"config":{"format":"gguf"}}`)
	f.push("ai/gemma3", "latest", map[string]any{"schemaVersion": 2, "mediaType": ociManifestType,
		"config": map[string]any{"mediaType": "application/vnd.cncf.model.config.v1+json", "digest": f.blob(cfg), "size": len(cfg)},
		"layers": []map[string]any{
			{"mediaType": "application/vnd.cncf.model.weight.v1.raw", "digest": f.blob(w), "size": len(w), "annotations": map[string]string{"org.cncf.model.filepath": "gemma-3-12b-it-Q4_K_M.gguf"}},
			{"mediaType": "application/vnd.cncf.model.weight.v1.raw", "digest": f.blob(mm), "size": len(mm), "annotations": map[string]string{"org.cncf.model.filepath": "mmproj-F16.gguf"}},
		}})
	c := newOCIClient(t, f, t.TempDir())
	p, err := c.Resolve(context.Background(), ociRef(t, f, "ai/gemma3"), Selection{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, fl := range p.Files {
		names = append(names, fl.Path)
	}
	if got := strings.Join(names, ","); got != "config.json,gemma-3-12b-it-Q4_K_M.gguf,manifest.json,mmproj-F16.gguf" {
		t.Fatalf("files %s", got)
	}
}

func TestOCIRefusesBadManifests(t *testing.T) {
	f := newFakeOCI(t)
	c := newOCIClient(t, f, t.TempDir())
	layer := func(ann map[string]string) map[string]any {
		b := fakeGGUF(10)
		return map[string]any{"schemaVersion": 2, "mediaType": ociManifestType,
			"config": map[string]any{"mediaType": "x", "digest": f.blob([]byte("{}")), "size": 2},
			"layers": []map[string]any{{"mediaType": "application/vnd.cncf.model.weight.v1.raw", "digest": f.blob(b), "size": len(b), "annotations": ann}}}
	}
	f.push("ai/escape", "latest", layer(map[string]string{"org.cncf.model.filepath": "../../.bashrc"}))
	f.push("ai/gitdir", "latest", layer(map[string]string{"org.cncf.model.filepath": ".git/config"}))
	f.push("ai/index", "latest", map[string]any{"schemaVersion": 2, "mediaType": ociIndexType, "manifests": []any{map[string]any{"digest": "sha256:" + strings.Repeat("a", 64)}}})
	for _, repo := range []string{"ai/escape", "ai/gitdir"} {
		if _, err := c.Resolve(context.Background(), ociRef(t, f, repo), Selection{}); err == nil {
			t.Errorf("%s was accepted", repo)
		}
	}
	// an index gets the error that says what to do, not a generic one
	if _, err := c.Resolve(context.Background(), ociRef(t, f, "ai/index"), Selection{}); AsError(err).Kind != "unsupported" ||
		!strings.Contains(AsError(err).Why, "@sha256:") {
		t.Errorf("an index: %v", err)
	}
	f.dockerAI("ai/lie", "latest", fakeGGUF(10), []byte("l"))
	f.lieDigest = true
	if _, err := c.Resolve(context.Background(), ociRef(t, f, "ai/lie"), Selection{}); AsError(err).Code != ExitVerify {
		t.Fatalf("a manifest that does not match Docker-Content-Digest: %v", err)
	}
}

func TestOCILockSyncsByDigestAfterTheTagMoves(t *testing.T) {
	f := newFakeOCI(t)
	model := fakeGGUF(4000)
	f.dockerAI("ai/m", "latest", model, []byte("license v1"))
	home := t.TempDir()
	c := newOCIClient(t, f, home)
	ref := ociRef(t, f, "ai/m")
	out, err := c.Pull(context.Background(), Request{Ref: ref, Target: Target{Kind: "none"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLock(filepath.Join(t.TempDir(), DefaultLockName))
	if err := l.Upsert(out.Plan, Selection{}, Target{Kind: "none"}); err != nil {
		t.Fatal(err)
	}
	l.Write()
	l2, err := ReadLock(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	f.dockerAI("ai/m", "latest", fakeGGUF(5000), []byte("license v2")) // the tag moves on
	c2 := newOCIClient(t, f, t.TempDir())                              // an empty store: online, by digest
	req, _ := l2.Request(&l2.Models[0], false)
	out2, err := c2.Pull(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("sync after the tag moved: %v", err)
	}
	if out2.Plan.Commit != out.Plan.Commit {
		t.Fatal("the sync did not reproduce the locked manifest")
	}
	if !c2.store.Has(sha(model), int64(len(model))) {
		t.Fatal("the locked model layer is not in the new store")
	}
	// rm by tag
	if _, err := c.Remove(ref); err != nil {
		t.Fatal(err)
	}
}

// A registry that answers a request by digest with another manifest (and no
// digest header) is caught: a pinned sync gets exactly the locked manifest.
func TestOCIRefusesAnotherManifestForADigest(t *testing.T) {
	f := newFakeOCI(t)
	f.dockerAI("ai/m", "latest", fakeGGUF(100), []byte("a"))
	c := newOCIClient(t, f, t.TempDir())
	p, err := c.Resolve(context.Background(), ociRef(t, f, "ai/m"), Selection{})
	if err != nil {
		t.Fatal(err)
	}
	f.dockerAI("ai/m", "other", fakeGGUF(200), []byte("b"))
	f.swap = f.manifests["ai/m:other"]
	at := p.Ref
	at.Revision = p.Commit
	if _, err := c.Resolve(context.Background(), at, Selection{}); AsError(err).Code != ExitVerify {
		t.Fatalf("got %v, want the substituted manifest refused", err)
	}
}
