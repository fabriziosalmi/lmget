package lmget

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// switchable puts a handler behind an on/off switch: off answers 503, as an
// unreachable or failing source does.
type switchable struct {
	h    http.Handler
	down atomic.Bool
}

func (s *switchable) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.down.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	s.h.ServeHTTP(w, r)
}

// lazy lets a server exist (and its URL be known) before its handler.
type lazy struct{ h atomic.Pointer[http.Handler] }

func (l *lazy) set(h http.Handler) { l.h.Store(&h) }
func (l *lazy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := l.h.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

type m3 struct {
	hf      *switchable
	hfURL   string
	files   []fakeFile
	signer  signer
	idx     *indexServer
	idxURL  string
	node    *httptest.Server
	nodeCli *Client
	lock    *Lock
	tlsHTTP *http.Client
}

func (m *m3) client(t *testing.T, home string) *Client {
	t.Helper()
	c, err := New(Options{Home: home, HFEndpoint: m.hfURL, HTTP: m.tlsHTTP, NoMirror: true,
		IndexURL: m.idxURL, IndexKeys: []string{m.signer.pub}, Conns: 4, ChunkSize: 256 << 10})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// setupM3: a source, a lock of org/m, a signed index built from it listing
// nodes, and one node that mirrored the index and serves it.
func setupM3(t *testing.T, extraNodes ...string) *m3 {
	t.Helper()
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HF_TOKEN_PATH", filepath.Join(t.TempDir(), "none"))
	t.Setenv("HF_HOME", t.TempDir())
	m := &m3{files: modelFiles(), signer: testSigner(t), idx: &indexServer{}}
	m.hf = &switchable{h: (&fakeHF{files: m.files}).handler()}
	hf := httptest.NewServer(m.hf)
	t.Cleanup(hf.Close)
	m.hfURL = hf.URL
	idx := httptest.NewTLSServer(m.idx)
	t.Cleanup(idx.Close)
	m.idxURL, m.tlsHTTP = idx.URL+"/index.json", idx.Client()
	nodeH := &lazy{}
	m.node = httptest.NewTLSServer(nodeH)
	t.Cleanup(m.node.Close)

	// a maintainer locks the model and builds the index from the lock
	b := m.client(t, t.TempDir())
	out, err := b.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.lock = NewLock(filepath.Join(t.TempDir(), DefaultLockName))
	if err := m.lock.Upsert(out.Plan, Selection{}, Target{Kind: "none"}); err != nil {
		t.Fatal(err)
	}
	ix, err := b.BuildIndex(context.Background(), m.lock, BuildOptions{Sequence: 1, Valid: 24 * time.Hour,
		Nodes: append(extraNodes, m.node.URL)})
	if err != nil {
		t.Fatal(err)
	}
	publish(m.idx, m.signer, *ix)
	// the node mirrors the index and serves it
	m.nodeCli = m.client(t, t.TempDir())
	if _, err := m.nodeCli.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := m.nodeCli.MirrorIndex(context.Background(), nil)
	if err != nil || len(res) != 1 || res[0].Err != "" {
		t.Fatalf("mirror: %+v %v", res, err)
	}
	nodeH.set(m.nodeCli.NodeHandler())
	return m
}

// M3 exit criterion, in miniature: with the source down, an indexed model
// arrives complete from the node, verified.
func TestIndexedModelComesFromTheNodeWhenTheSourceIsDown(t *testing.T) {
	m := setupM3(t)
	u := m.client(t, t.TempDir())
	if _, err := u.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.hf.down.Store(true)
	dir := t.TempDir()
	var from []string
	out, err := u.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "dir", Path: dir}}, func(e Event) {
		if e.Type == EvFileDone {
			from = append(from, e.From)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Plan.FromIndex {
		t.Fatal("the plan did not come from the signed index")
	}
	for _, f := range from {
		if !strings.HasPrefix(f, NodeOrigin) {
			t.Fatalf("a file came from %q, not from the node", f)
		}
	}
	for _, f := range m.files {
		if b, err := os.ReadFile(filepath.Join(dir, f.path)); err != nil || !bytes.Equal(b, f.data) {
			t.Fatalf("%s is missing or wrong in the export", f.path)
		}
	}
	// a pinned sync works the same way
	u2 := m.client(t, t.TempDir())
	if _, err := u2.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	req, err := m.lock.Request(&m.lock.Models[0], false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u2.Pull(context.Background(), req, nil); err != nil {
		t.Fatalf("pinned sync with the source down: %v", err)
	}
}

func TestNoIndexNoFallback(t *testing.T) {
	m := setupM3(t)
	u := m.client(t, t.TempDir()) // never fetched the index
	m.hf.down.Store(true)
	if _, err := u.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil); AsError(err).Code != ExitNetwork {
		t.Fatalf("got %v, want the source's network error", err)
	}
}

func TestRefusedAccessIsNotRoutedToTheNodes(t *testing.T) {
	m := setupM3(t)
	u := m.client(t, t.TempDir())
	u.UpdateIndex(context.Background())
	m.hf.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
	if _, err := u.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil); AsError(err).Code != ExitAuth {
		t.Fatalf("got %v: a gated model must stay gated", err)
	}
}

func TestANodeServingOtherBytesIsSkipped(t *testing.T) {
	// a node that serves the right sizes and ranges, with other bytes
	var good atomic.Pointer[string]
	bad := httptest.NewUnstartedServer(nil)
	bad.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequest(http.MethodGet, *good.Load()+r.URL.Path, nil)
		resp, err := bad.Client().Do(req)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		for i := range b {
			b[i] ^= 0x5a
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
	})
	bad.StartTLS()
	defer bad.Close()
	m := setupM3(t, bad.URL) // the bad node is listed first
	u0 := m.node.URL
	good.Store(&u0)
	u := m.client(t, t.TempDir())
	u.UpdateIndex(context.Background())
	m.hf.down.Store(true)
	var from []string
	if _, err := u.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, func(e Event) {
		if e.Type == EvFileDone {
			from = append(from, e.From)
		}
	}); err != nil {
		t.Fatal(err)
	}
	for _, f := range from {
		if f != NodeOrigin+hostOf(m.node.URL) {
			t.Fatalf("a file was taken from %q", f)
		}
	}
}

func TestNodeServesOnlyIndexedBlobs(t *testing.T) {
	m := setupM3(t)
	cl := m.node.Client()
	get := func(path string, hdr ...string) (int, []byte) {
		req, _ := http.NewRequest(http.MethodGet, m.node.URL+path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	model := m.files[0].data
	if code, b := get("/blobs/sha256/" + sha(model)); code != 200 || !bytes.Equal(b, model) {
		t.Fatalf("an indexed blob: %d", code)
	}
	if code, b := get("/blobs/sha256/"+sha(model), "Range", "bytes=10-19"); code != 206 || !bytes.Equal(b, model[10:20]) {
		t.Fatalf("a range: %d", code)
	}
	// a blob the node's store holds but the index does not name
	other := randBytes(100)
	os.WriteFile(m.nodeCli.store.BlobPath(sha(other)), other, 0o444)
	for _, p := range []string{"/blobs/sha256/" + sha(other), "/blobs/sha256/../../index.json", "/blobs/sha256/" + strings.ToUpper(sha(model)), "/", "/refs/"} {
		if code, _ := get(p); code == 200 {
			t.Errorf("%s served", p)
		}
	}
	if code, b := get("/index.json"); code != 200 || !json.Valid(b) {
		t.Fatalf("the index: %d", code)
	}
}

// the builder trusts the source, not the lock: license, gating and files
func TestBuildIndexChecksTheSource(t *testing.T) {
	m := setupM3(t)
	b := m.client(t, t.TempDir())
	rev := func(body string) {
		inner := (&fakeHF{files: m.files}).handler()
		m.hf.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/revision/") {
				w.Write([]byte(body))
				return
			}
			inner.ServeHTTP(w, r)
		})
	}
	build := func() error {
		_, err := b.BuildIndex(context.Background(), m.lock, BuildOptions{Sequence: 2, Valid: time.Hour, Nodes: []string{m.node.URL}})
		return err
	}
	rev(`{"sha":"` + commit + `","gated":false,"cardData":{"license":"llama3"}}`)
	if err := build(); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("a non-allowlisted license: %v", err)
	}
	rev(`{"sha":"` + commit + `","gated":"auto","cardData":{"license":"apache-2.0"}}`)
	if err := build(); err == nil || !strings.Contains(err.Error(), "gated") {
		t.Errorf("a gated model: %v", err)
	}
	m.hf.h = (&fakeHF{files: m.files}).handler()
	for i, name := range []string{"config.json (git id only at the source)", "model.safetensors (sha256 at the source)"} {
		saved := m.lock.Models[0].Files[i].SHA256
		m.lock.Models[0].Files[i].SHA256 = strings.Repeat("e", 64)
		if err := build(); err == nil {
			t.Errorf("a lock with a wrong sha256 for %s was indexed", name)
		}
		m.lock.Models[0].Files[i].SHA256 = saved
	}
	if err := build(); err != nil {
		t.Fatalf("the correct lock was refused: %v", err)
	}
}

// a node refuses to mirror a model whose source no longer matches the index
func TestMirrorRefusesDrift(t *testing.T) {
	m := setupM3(t)
	m.files[1].data = []byte(`{"a":2}`) // config.json changed at the source, same commit
	m.hf.h = (&fakeHF{files: m.files}).handler()
	n := m.client(t, t.TempDir())
	n.UpdateIndex(context.Background())
	res, err := n.MirrorIndex(context.Background(), nil)
	if err != nil || len(res) != 1 || res[0].Err == "" {
		t.Fatalf("drift was mirrored: %+v %v", res, err)
	}
}

func TestLicenseAllowlist(t *testing.T) {
	for l, want := range map[string]bool{"apache-2.0": true, "MIT": true, "apache-2.0, mit": true, "apache-2.0, llama3": false, "": false, "other": false, "cc-by-nc-4.0": false} {
		if LicenseAllowed(l) != want {
			t.Errorf("LicenseAllowed(%q) != %v", l, want)
		}
	}
}

// defence in depth: a node does not mirror a model whose license is not on the
// allowlist, even when a signed index lists it
func TestMirrorSkipsLicensesOffTheAllowlist(t *testing.T) {
	m := setupM3(t)
	ix := sampleIndex(2)
	ix.Nodes = []string{m.node.URL}
	ix.Models[0].Ref, ix.Models[0].Commit, ix.Models[0].License = "hf:org/m", commit, "llama3"
	ix.Models[0].Files = m.lock.Models[0].Files
	publish(m.idx, m.signer, ix)
	n := m.client(t, t.TempDir())
	if _, err := n.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := n.MirrorIndex(context.Background(), nil)
	if err != nil || len(res) != 1 || !strings.Contains(res[0].Err, "allowlist") {
		t.Fatalf("got %+v %v", res, err)
	}
	if st := n.Stats(); st.Blobs != 0 {
		t.Fatalf("%d blobs mirrored for a model off the allowlist", st.Blobs)
	}
}
