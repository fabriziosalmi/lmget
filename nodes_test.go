package lmget

import (
	"bytes"
	"context"
	"io"
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

// Found live on 2026-11-17: a store that never fetched the index got an
// exit 6 with the source down, though the nodes had the model. Now the
// first failure fetches the signed index, and the node serves the files.
func TestAFreshStoreFetchesTheIndexWhenTheSourceIsDown(t *testing.T) {
	m := setupM3(t)
	u := m.client(t, t.TempDir()) // never fetched the index
	m.hf.down.Store(true)
	out, err := u.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil)
	if err != nil {
		t.Fatalf("fresh store, source down: %v", err)
	}
	if !out.Plan.FromIndex {
		t.Fatal("the plan did not come from the signed index")
	}
	if _, err := u.Index(); err != nil {
		t.Fatalf("the index fetched on the way was not cached: %v", err)
	}
	// a pinned sync from a fresh store too
	req, err := m.lock.Request(&m.lock.Models[0], false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.client(t, t.TempDir()).Pull(context.Background(), req, nil); err != nil {
		t.Fatalf("pinned sync, fresh store, source down: %v", err)
	}
}

// With the index URL failing too, a mirror of the index (a node, which
// serves the copy it verified) stands in; its signature is checked as always.
func TestTheIndexComesFromAMirrorWhenItsURLFails(t *testing.T) {
	m := setupM3(t)
	m.idx.set([]byte("<html>down</html>"), "not a signature")
	c, err := New(Options{Home: t.TempDir(), HFEndpoint: m.hfURL, HTTP: m.tlsHTTP, NoMirror: true,
		IndexURL: m.idxURL, IndexMirrors: []string{m.node.URL + "/index.json"},
		IndexKeys: []string{m.signer.pub}, Conns: 4, ChunkSize: 256 << 10})
	if err != nil {
		t.Fatal(err)
	}
	m.hf.down.Store(true)
	out, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil)
	if err != nil || !out.Plan.FromIndex {
		t.Fatalf("index URL and source down, mirror up: %v", err)
	}

	// a mirror serving an index signed by another key is refused
	m2 := setupM3(t)
	m2.idx.set([]byte("down"), "x")
	other := testSigner(t)
	c2, err := New(Options{Home: t.TempDir(), HFEndpoint: m2.hfURL, HTTP: m2.tlsHTTP, NoMirror: true,
		IndexURL: m2.idxURL, IndexMirrors: []string{m2.node.URL + "/index.json"},
		IndexKeys: []string{other.pub}, Conns: 4, ChunkSize: 256 << 10})
	if err != nil {
		t.Fatal(err)
	}
	m2.hf.down.Store(true)
	if _, err := c2.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil); AsError(err).Code != ExitNetwork {
		t.Fatalf("an index signed by an untrusted key was used: %v", err)
	}
}

// No index anywhere: the source's own error, as before.
func TestNoIndexAnywhereNoFallback(t *testing.T) {
	m := setupM3(t)
	m.idx.set([]byte("down"), "x")
	u := m.client(t, t.TempDir()) // no mirrors: a custom index URL has none
	m.hf.down.Store(true)
	if _, err := u.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil); AsError(err).Code != ExitNetwork {
		t.Fatalf("got %v, want the source's network error", err)
	}
}

// Only the default index URL has mirrors: a custom one is never replaced by
// the project's nodes.
func TestOnlyTheDefaultIndexHasMirrors(t *testing.T) {
	t.Setenv("LMGET_INDEX_URL", "")
	c, err := New(Options{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.opt.IndexMirrors) != len(DefaultIndexMirrors) {
		t.Fatalf("default index: mirrors %v", c.opt.IndexMirrors)
	}
	c, err = New(Options{Home: t.TempDir(), IndexURL: "https://index.example.org/index.json"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.opt.IndexMirrors) != 0 {
		t.Fatalf("custom index: mirrors %v", c.opt.IndexMirrors)
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

// A node that fails is skipped for the rest of the run: measured on the
// real nodes, a dead first node cost every file its whole retry sequence
// (about 16 s each) before the next node was asked.
func TestADeadNodeIsAskedForOneFileOnly(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	dead := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[r.URL.Path]++
		mu.Unlock()
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer dead.Close()
	m := setupM3(t, dead.URL) // the index lists the dead node first
	u := m.client(t, t.TempDir())
	u.retryBase = time.Millisecond
	if _, err := u.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.hf.down.Store(true)
	var from []string
	out, err := u.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "dir", Path: t.TempDir()}}, func(e Event) {
		if e.Type == EvFileDone {
			from = append(from, e.From)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Plan.FromIndex || len(from) != len(m.files) {
		t.Fatalf("from_index=%v, %d files done; want %d from the index", out.Plan.FromIndex, len(from), len(m.files))
	}
	good := NodeOrigin + strings.TrimPrefix(m.node.URL, "https://")
	for _, f := range from {
		if f != good {
			t.Fatalf("a file came from %q, want %q", f, good)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 {
		t.Fatalf("the dead node was asked for %d different blobs (%v); want 1, then skipped", len(asked), asked)
	}
}
