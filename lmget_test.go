package lmget

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

type fakeFile struct {
	path    string
	data    []byte
	lfs     bool
	corrupt bool // serve different bytes than published
}

type fakeHF struct {
	files  []fakeFile
	byRepo map[string][]fakeFile // optional: files for a given "org/name"
	served atomic.Int64          // bytes served by resolve
	onByte atomic.Pointer[func(total int64)]
	tree   func() []map[string]any // override
}

func gitID(b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func (f *fakeHF) handler() http.Handler {
	mux := http.NewServeMux()
	// every repository serves the same files (models share blobs, like real
	// repos that ship the same tokenizer), unless byRepo says otherwise
	filesOf := func(r *http.Request) []fakeFile {
		if fs, ok := f.byRepo[r.PathValue("org")+"/"+r.PathValue("name")]; ok {
			return fs
		}
		return f.files
	}
	mux.HandleFunc("GET /api/models/{org}/{name}/revision/{rev...}", func(w http.ResponseWriter, r *http.Request) {
		if rev := r.PathValue("rev"); rev != "main" && rev != commit {
			http.NotFound(w, r) // like HF: branches, tags and commits resolve, anything else 404
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("org") + "/" + r.PathValue("name"), "sha": commit, "gated": false, "cardData": map[string]any{"license": "apache-2.0"}})
	})
	mux.HandleFunc("GET /api/models/{org}/{name}/tree/"+commit, func(w http.ResponseWriter, r *http.Request) {
		var entries []map[string]any
		if f.tree != nil {
			entries = f.tree()
		} else {
			for _, ff := range filesOf(r) {
				e := map[string]any{"type": "file", "path": ff.path, "size": len(ff.data), "oid": gitID(ff.data)}
				if ff.lfs {
					e["lfs"] = map[string]any{"oid": sha(ff.data), "size": len(ff.data)}
				}
				entries = append(entries, e)
			}
			entries = append(entries, map[string]any{"type": "directory", "path": "sub", "oid": "x"})
		}
		// paginate: one entry per page, as HF does with Link rel=next
		page := 0
		fmt.Sscan(r.URL.Query().Get("page"), &page)
		if page+1 < len(entries) {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?recursive=true&page=%d>; rel="next"`, r.Host, r.URL.Path, page+1))
		}
		json.NewEncoder(w).Encode(entries[page : page+1])
	})
	files := http.NewServeMux()
	files.HandleFunc("GET /{org}/{name}/resolve/"+commit+"/{path...}", func(w http.ResponseWriter, r *http.Request) {
		p := r.PathValue("path")
		for _, ff := range filesOf(r) {
			if ff.path == p {
				data := ff.data
				if ff.corrupt {
					data = append([]byte(nil), data...)
					data[len(data)/2] ^= 1
				}
				http.ServeContent(&countingWriter{ResponseWriter: w, f: f}, r, p, time.Time{}, bytes.NewReader(data))
				return
			}
		}
		http.NotFound(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			mux.ServeHTTP(w, r)
		} else {
			files.ServeHTTP(w, r)
		}
	})
}

type countingWriter struct {
	http.ResponseWriter
	f *fakeHF
}

func (c *countingWriter) Write(b []byte) (int, error) {
	// small writes so cancellation can land mid-chunk
	var n int
	for len(b) > 0 {
		k := min(len(b), 32<<10)
		m, err := c.ResponseWriter.Write(b[:k])
		n += m
		t := c.f.served.Add(int64(m))
		if fn := c.f.onByte.Load(); fn != nil {
			(*fn)(t)
		}
		if err != nil {
			return n, err
		}
		b = b[k:]
	}
	return n, nil
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func newTestClient(t *testing.T, srv *httptest.Server, home string) *Client {
	t.Helper()
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HF_TOKEN_PATH", filepath.Join(t.TempDir(), "none"))
	t.Setenv("HF_HOME", t.TempDir())
	c, err := New(Options{Home: home, HFEndpoint: srv.URL, Conns: 4, ChunkSize: 256 << 10})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fakeSafetensors is a well-formed safetensors file with n payload bytes.
func fakeSafetensors(n int) []byte {
	header := []byte(fmt.Sprintf(`{"w":{"dtype":"U8","shape":[%d],"data_offsets":[0,%d]}}`, n, n))
	b := make([]byte, 8, 8+len(header)+n)
	binary.LittleEndian.PutUint64(b, uint64(len(header)))
	return append(append(b, header...), randBytes(n)...)
}

func modelFiles() []fakeFile {
	return []fakeFile{
		{path: "model.safetensors", data: fakeSafetensors(3<<20 + 12345), lfs: true},
		{path: "config.json", data: []byte(`{"a":1}`)},
		{path: "sub/tokenizer.json", data: randBytes(70_000)},
	}
}

func TestParseRef(t *testing.T) {
	ok := map[string]Ref{
		"Qwen/Qwen3-0.6B":                                              {"hf", "Qwen/Qwen3-0.6B", "main", "", ""},
		"hf:Qwen/Qwen3-0.6B@v1.0":                                      {"hf", "Qwen/Qwen3-0.6B", "v1.0", "", ""},
		"Qwen/Qwen3-0.6B@refs/pr/1":                                    {"hf", "Qwen/Qwen3-0.6B", "refs/pr/1", "", ""},
		"https://huggingface.co/Qwen/Q3":                               {"hf", "Qwen/Q3", "main", "", ""},
		"https://hf.co/Qwen/Q3/tree/dev":                               {"hf", "Qwen/Q3", "dev", "", ""},
		"https://huggingface.co/a/b/blob/c1/x":                         {"hf", "a/b", "c1", "x", ""},
		"https://huggingface.co/a/b/resolve/main/sub/m.gguf":           {"hf", "a/b", "main", "sub/m.gguf", ""},
		"https://example.com/m/model.gguf":                             {"url", "https://example.com/m/model.gguf", "", "", ""},
		"https://example.com/m.gguf#sha256=" + strings.Repeat("A", 64): {"url", "https://example.com/m.gguf", "", "", strings.Repeat("a", 64)},
	}
	for in, want := range ok {
		got, err := ParseRef(in)
		if err != nil || got != want {
			t.Errorf("ParseRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	bad := []string{"", "../etc", "a/../b", "a/b/c", "Qwen", "a/b@", "a/b@../x", "civitai:123",
		"https://huggingface.co/datasets/a/b", "a /b", "-a/b", "x:y/z",
		"http://example.com/m.gguf", "https://user:pw@example.com/m.gguf", "https://example.com/m.gguf#md5=00",
		"https://example.com/m.gguf#sha256=abc", "https://huggingface.co/a/b/resolve/main/../../etc"}
	for _, in := range bad {
		if _, err := ParseRef(in); err == nil {
			t.Errorf("ParseRef(%q) accepted", in)
		} else if AsError(err).Code != ExitUsage {
			t.Errorf("ParseRef(%q) code %d, want %d", in, AsError(err).Code, ExitUsage)
		}
	}
}

func TestPullVerifiesAndExportsHFCache(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	hfcache := t.TempDir()
	t.Setenv("HF_HUB_CACHE", hfcache)
	c := newTestClient(t, srv, t.TempDir())

	var types []EventType
	out, err := c.Pull(context.Background(), Request{Ref: Ref{"hf", "org/m", "main", "", ""}, Target: Target{Kind: "hf"}},
		func(e Event) { types = append(types, e.Type) })
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Plan.Files) != 3 {
		t.Fatalf("pagination: got %d files, want 3", len(out.Plan.Files))
	}
	snap := filepath.Join(hfcache, "models--org--m", "snapshots", commit)
	if out.Path != snap {
		t.Fatalf("path %s, want %s", out.Path, snap)
	}
	for _, ff := range f.files {
		got, err := os.ReadFile(filepath.Join(snap, ff.path))
		if err != nil || !bytes.Equal(got, ff.data) {
			t.Fatalf("%s: content mismatch through the hf cache (%v)", ff.path, err)
		}
		etag := gitID(ff.data)
		if ff.lfs {
			etag = sha(ff.data)
		}
		if _, err := os.Stat(filepath.Join(hfcache, "models--org--m", "blobs", etag)); err != nil {
			t.Fatalf("%s: blob not named by its huggingface_hub etag %s", ff.path, etag)
		}
	}
	if ref, _ := os.ReadFile(filepath.Join(hfcache, "models--org--m", "refs", "main")); string(ref) != commit {
		t.Fatalf("refs/main = %q", ref)
	}
	if types[0] != EvResolve || types[1] != EvPlan || types[len(types)-1] != EvDone {
		t.Fatalf("event order: %v", types)
	}

	// second pull: everything cached, git-only files through the index
	served := f.served.Load()
	out, err = c.Pull(context.Background(), Request{Ref: Ref{"hf", "org/m", "main", "", ""}, Target: Target{Kind: "none"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Fetched != 0 || f.served.Load() != served || out.Plan.ToFetch != 0 {
		t.Fatalf("second pull fetched %d bytes (server %d), to_fetch %d", out.Fetched, f.served.Load()-served, out.Plan.ToFetch)
	}
}

// Break-test: the server publishes one hash and serves other bytes.
func TestPullRejectsCorruptBytes(t *testing.T) {
	for _, lfs := range []bool{true, false} {
		files := modelFiles()
		files[0].corrupt, files[0].lfs = true, lfs
		f := &fakeHF{files: files}
		srv := httptest.NewServer(f.handler())
		home := t.TempDir()
		c := newTestClient(t, srv, home)
		_, err := c.Pull(context.Background(), Request{Ref: Ref{"hf", "org/m", "main", "", ""}, Target: Target{Kind: "none"}}, nil)
		srv.Close()
		if e := AsError(err); e == nil || e.Code != ExitVerify {
			t.Fatalf("lfs=%v: got %v, want exit %d", lfs, err, ExitVerify)
		}
		if c.store.Has(sha(files[0].data), int64(len(files[0].data))) {
			t.Fatal("corrupt file reached the store")
		}
		left, _ := filepath.Glob(filepath.Join(home, "tmp", "*.part"))
		state, _ := filepath.Glob(filepath.Join(home, "tmp", "*.state"))
		left = append(left, state...)
		if len(left) != 0 {
			t.Fatalf("partial files left behind: %v", left)
		}
	}
}

// Interrupt mid-download, resume, and count what the server sent.
func TestPullResumesWithoutRefetching(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	t.Setenv("HF_HUB_CACHE", t.TempDir())
	home := t.TempDir()
	c := newTestClient(t, srv, home)
	ctx, cancel := context.WithCancel(context.Background())
	stopAt := func(total int64) {
		if total > 1500<<10 {
			cancel()
		}
	}
	f.onByte.Store(&stopAt)
	_, err := c.Pull(ctx, Request{Ref: Ref{"hf", "org/m", "main", "", ""}, Target: Target{Kind: "none"}}, nil)
	if e := AsError(err); e.Code != ExitInterrupted {
		t.Fatalf("got %v, want interrupted", err)
	}
	first := f.served.Load()
	f.onByte.Store(nil)
	out, err := c.Pull(context.Background(), Request{Ref: Ref{"hf", "org/m", "main", "", ""}, Target: Target{Kind: "none"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, ff := range f.files {
		total += int64(len(ff.data))
	}
	waste := first + f.served.Load() - first - total // bytes sent more than once
	maxWaste := int64(c.opt.Conns) * c.opt.ChunkSize
	t.Logf("first run %d B, second run %d B (client fetched %d), total %d, re-sent %d (bound %d)",
		first, f.served.Load()-first, out.Fetched, total, waste, maxWaste)
	if waste > maxWaste {
		t.Fatalf("re-sent %d bytes, bound is %d", waste, maxWaste)
	}
}

func TestResolveRejectsUnsafePaths(t *testing.T) {
	for _, p := range []string{"../evil", "/abs", "a/../../b", "a//b", "a\\b"} {
		f := &fakeHF{tree: func() []map[string]any {
			return []map[string]any{{"type": "file", "path": p, "size": 1, "oid": strings.Repeat("a", 40)}}
		}}
		srv := httptest.NewServer(f.handler())
		c := newTestClient(t, srv, t.TempDir())
		_, err := c.Resolve(context.Background(), Ref{"hf", "org/m", "main", "", ""}, Selection{})
		srv.Close()
		if err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
}

func TestSelectionSkipsPickleWhenSafetensorsExist(t *testing.T) {
	files := append(modelFiles(),
		fakeFile{path: "pytorch_model.bin", data: randBytes(1000), lfs: true},
		fakeFile{path: "openvino/openvino_model.bin", data: randBytes(1000), lfs: true}, // OpenVINO IR, not pickle
		fakeFile{path: "openvino/openvino_model.xml", data: []byte("<net/>")})
	f := &fakeHF{files: files}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	p, err := c.Resolve(context.Background(), Ref{"hf", "org/m", "main", "", ""}, Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for _, fl := range p.Files {
		if fl.Path == "pytorch_model.bin" && fl.Selected() {
			t.Fatal("pickle selected although safetensors exist")
		}
		if fl.Path == "openvino/openvino_model.bin" && !fl.Selected() {
			t.Fatal("OpenVINO weights treated as pickle")
		}
	}
	p, _ = c.Resolve(context.Background(), Ref{"hf", "org/m", "main", "", ""}, Selection{AllowPickle: true, Include: []string{"pytorch_model.bin"}})
	if sel := p.Selected(); len(sel) != 1 || sel[0].Path != "pytorch_model.bin" || len(p.Pickle) != 1 {
		t.Fatalf("include/allow-pickle: %+v", sel)
	}
	if _, err := c.Resolve(context.Background(), Ref{"hf", "org/m", "main", "", ""}, Selection{Include: []string{"["}}); AsError(err).Code != ExitUsage {
		t.Fatalf("bad glob: %v", err)
	}
}

func TestErrorsMapToExitCodes(t *testing.T) {
	cases := map[int]ExitCode{401: ExitAuth, 403: ExitAuth, 404: ExitNotFound, 429: ExitNetwork, 500: ExitNetwork}
	for status, want := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		c := newTestClient(t, srv, t.TempDir())
		_, err := c.Resolve(context.Background(), Ref{"hf", "org/m", "main", "", ""}, Selection{})
		srv.Close()
		if got := AsError(err).Code; got != want {
			t.Errorf("HTTP %d: exit %d, want %d", status, got, want)
		}
	}
	if AsError(context.Canceled).Code != ExitInterrupted {
		t.Error("context.Canceled must map to interrupted")
	}
	if e := newErr(ExitDisk, "disk", "cannot write").wrap(errors.New("no space left on device")); e.Cause != "no space left on device" {
		t.Errorf("the cause is not carried: %+v", e)
	}
}

// Identical bytes already in the target directory are accepted, different
// bytes are not overwritten.
func TestExportDirAcceptsIdenticalFilesOnly(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	dir := t.TempDir()
	req := Request{Ref: Ref{"hf", "org/m", "main", "", ""}, Target: Target{Kind: "dir", Path: dir}}
	if _, err := newTestClient(t, srv, t.TempDir()).Pull(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	// a second, empty store exporting into the same directory
	if _, err := newTestClient(t, srv, t.TempDir()).Pull(context.Background(), req, nil); err != nil {
		t.Fatalf("identical files rejected: %v", err)
	}
	p := filepath.Join(dir, "config.json")
	os.Remove(p)
	os.WriteFile(p, []byte(`{"a":2}`), 0o644) // same size, other bytes
	_, err := newTestClient(t, srv, t.TempDir()).Pull(context.Background(), req, nil)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("different bytes were accepted or overwritten: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != `{"a":2}` {
		t.Fatal("user file was overwritten")
	}
}
