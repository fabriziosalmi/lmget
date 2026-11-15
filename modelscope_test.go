package lmget

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMS is ModelScope: a file listing, resolve URLs that serve small files
// with 200 and send large ones (302) to a CDN path that answers ranges.
type fakeMS struct {
	files    map[string][]byte // path -> bytes served
	listed   map[string]string // path -> sha256 in the listing (default: of the bytes)
	requests atomic.Int64
	auth     atomic.Bool
}

func (m *fakeMS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.requests.Add(1)
	if r.Header.Get("Authorization") != "" {
		m.auth.Store(true)
	}
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/api/v1/models/") && strings.HasSuffix(p, "/repo/files"):
		var fs []map[string]any
		for path, b := range m.files {
			s := sha(b)
			if v, ok := m.listed[path]; ok {
				s = v
			}
			fs = append(fs, map[string]any{"Type": "blob", "Path": path, "Size": len(b), "Sha256": s})
		}
		json.NewEncoder(w).Encode(map[string]any{"Code": 200, "Data": map[string]any{"Files": fs}})
	case strings.HasPrefix(p, "/models/") && strings.Contains(p, "/resolve/master/"):
		path := p[strings.Index(p, "/resolve/master/")+len("/resolve/master/"):]
		b, ok := m.files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if len(b) > 64<<10 {
			http.Redirect(w, r, "/cdn/"+path, http.StatusFound)
			return
		}
		w.Write(b) // ModelScope answers 200 with the whole small file
	case strings.HasPrefix(p, "/cdn/"):
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(m.files[strings.TrimPrefix(p, "/cdn/")]))
	default:
		http.NotFound(w, r)
	}
}

// hfFailing answers status for the resolve of the named files, and passes
// everything else to the fake Hub.
func hfFailing(f *fakeHF, status int, names ...string) http.Handler {
	h := f.handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, n := range names {
			if strings.Contains(r.URL.Path, "/resolve/") && strings.HasSuffix(r.URL.Path, "/"+n) {
				w.WriteHeader(status)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

func msSetup(t *testing.T, status int, failing ...string) (*Client, *fakeHF, *fakeMS, func()) {
	t.Helper()
	files := modelFiles()
	f := &fakeHF{files: files}
	ms := &fakeMS{files: map[string][]byte{}, listed: map[string]string{}}
	for _, ff := range files {
		ms.files[ff.path] = ff.data
	}
	hf := httptest.NewServer(hfFailing(f, status, failing...))
	msSrv := httptest.NewServer(ms)
	t.Setenv("HF_TOKEN_PATH", filepath.Join(t.TempDir(), "none"))
	t.Setenv("HF_HOME", t.TempDir())
	c, err := New(Options{Home: t.TempDir(), HFEndpoint: hf.URL, HFToken: "hf_secret_for_hf_only",
		ModelScopeEndpoint: msSrv.URL, Conns: 4, ChunkSize: 256 << 10})
	if err != nil {
		t.Fatal(err)
	}
	return c, f, ms, func() { hf.Close(); msSrv.Close() }
}

func pullCollect(c *Client) (*Outcome, []Event, error) {
	var evs []Event
	out, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, func(e Event) { evs = append(evs, e) })
	return out, evs, err
}

func TestModelScopeServesFilesHuggingFaceLost(t *testing.T) {
	c, _, ms, done := msSetup(t, http.StatusNotFound, "model.safetensors", "config.json")
	defer done()
	out, evs, err := pullCollect(c)
	if err != nil {
		t.Fatal(err)
	}
	from := map[string]string{}
	for _, e := range evs {
		if e.Type == EvFileDone {
			from[e.File] = e.From
		}
	}
	if from["model.safetensors"] != MirrorModelScope || from["config.json"] != MirrorModelScope || from["sub/tokenizer.json"] != "" {
		t.Fatalf("file_done from: %v", from)
	}
	for _, f := range out.Plan.Selected() {
		if !c.store.Has(f.SHA256, f.Size) {
			t.Fatalf("%s is not in the store", f.Path)
		}
	}
	if ms.auth.Load() {
		t.Fatal("the Hugging Face token was sent to ModelScope")
	}
}

func TestModelScopeIsNotAskedWhileHuggingFaceWorks(t *testing.T) {
	c, _, ms, done := msSetup(t, 0)
	defer done()
	if _, _, err := pullCollect(c); err != nil {
		t.Fatal(err)
	}
	if n := ms.requests.Load(); n != 0 {
		t.Fatalf("%d requests reached ModelScope while Hugging Face worked", n)
	}
}

func TestModelScopeBytesAreCheckedAgainstHuggingFace(t *testing.T) {
	c, f, ms, done := msSetup(t, http.StatusNotFound, "model.safetensors")
	defer done()
	var real []byte
	for _, ff := range f.files {
		if ff.path == "model.safetensors" {
			real = ff.data
		}
	}
	ms.files["model.safetensors"] = bytes.Repeat([]byte{'x'}, len(real)) // same size, other bytes
	ms.listed["model.safetensors"] = sha(real)                           // and a listing that lies
	_, _, err := pullCollect(c)
	e := AsError(err)
	if e.Code != ExitVerify || !strings.Contains(e.Error()+fixesText(e), "modelscope") {
		t.Fatalf("got %v, want a verification failure that names ModelScope", err)
	}
	if c.store.Has(sha(ms.files["model.safetensors"]), int64(len(real))) || c.store.Has(sha(real), int64(len(real))) {
		t.Fatal("bytes from ModelScope entered the store")
	}
}

func fixesText(e *Error) string {
	var b strings.Builder
	for _, f := range e.Fixes {
		b.WriteString(f.Cmd + " " + f.Note + "\n")
	}
	return b.String()
}

func TestModelScopeWithAnotherVersionIsNotTried(t *testing.T) {
	c, _, ms, done := msSetup(t, http.StatusNotFound, "model.safetensors")
	defer done()
	ms.listed["model.safetensors"] = strings.Repeat("0", 64) // ModelScope has another version
	_, _, err := pullCollect(c)
	if AsError(err).Code != ExitNotFound {
		t.Fatalf("got %v, want Hugging Face's not found", err)
	}
	if n := ms.requests.Load(); n != 1 {
		t.Fatalf("%d requests to ModelScope, want only the listing", n)
	}
}

func TestModelScopeIsNeverUsedForRefusedAccess(t *testing.T) {
	c, _, ms, done := msSetup(t, http.StatusForbidden, "model.safetensors")
	defer done()
	_, _, err := pullCollect(c)
	if AsError(err).Code != ExitAuth {
		t.Fatalf("got %v, want the access refusal", err)
	}
	if n := ms.requests.Load(); n != 0 {
		t.Fatalf("a gated file was looked up on ModelScope (%d requests)", n)
	}
}

func TestNoMirrorNeverAsksModelScope(t *testing.T) {
	c, _, ms, done := msSetup(t, http.StatusNotFound, "model.safetensors")
	defer done()
	c.opt.NoMirror = true
	if _, _, err := pullCollect(c); AsError(err).Code != ExitNotFound {
		t.Fatalf("got %v", err)
	}
	if ms.requests.Load() != 0 {
		t.Fatal("ModelScope was asked with NoMirror")
	}
}

// Hugging Face answering 503 to every attempt: the retries run out, then
// ModelScope serves the file (about 15 s of backoff).
func TestModelScopeAfterHuggingFaceKeepsFailing(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the retry backoff")
	}
	c, _, _, done := msSetup(t, http.StatusServiceUnavailable, "config.json")
	defer done()
	_, evs, err := pullCollect(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Type == EvFileDone && e.File == "config.json" && e.From != MirrorModelScope {
			t.Fatal("config.json did not come from ModelScope")
		}
	}
}
