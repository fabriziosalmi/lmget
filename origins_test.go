package lmget

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOrigin serves blobs by sha256, records what it was asked, and can lie
// or fail.
type fakeOrigin struct {
	mu    sync.Mutex
	blobs map[string][]byte
	asked []string
	auth  []string
	mode  string // "", "lie", "500", "half", "noranges"
	n     int
}

func (o *fakeOrigin) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.asked = append(o.asked, r.URL.Path)
		o.auth = append(o.auth, r.Header.Get("Authorization"))
		o.n++
		mode, n := o.mode, o.n
		o.mu.Unlock()
		b, ok := o.blobs[strings.TrimPrefix(r.URL.Path, "/blobs/sha256/")]
		switch {
		case !ok:
			http.NotFound(w, r)
			return
		case mode == "lie":
			b = append([]byte(nil), b...)
			b[len(b)/2] ^= 1
		case mode == "500", mode == "half" && n > 2: // the probe, then one chunk
			http.Error(w, "down", http.StatusInternalServerError)
			return
		case mode == "noranges": // like python3 -m http.server
			w.Write(b)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(string(b)))
	})
}

func originSetup(t *testing.T, mode string, withBlob bool) (*Client, *fakeHF, *fakeOrigin, []fakeFile) {
	t.Helper()
	t.Setenv("HF_HUB_CACHE", t.TempDir())
	files := modelFiles()
	f := &fakeHF{files: files}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	o := &fakeOrigin{blobs: map[string][]byte{}, mode: mode}
	if withBlob {
		for _, ff := range files {
			o.blobs[sha(ff.data)] = ff.data
		}
	}
	osrv := httptest.NewServer(o.handler())
	t.Cleanup(osrv.Close)
	newTestClient(t, srv, t.TempDir()) // sets the token environment
	c, err := New(Options{Home: t.TempDir(), HFEndpoint: srv.URL, Conns: 4, ChunkSize: 256 << 10,
		HFToken: "hf_secret", Origins: []string{osrv.URL + "/"}})
	if err != nil {
		t.Fatal(err)
	}
	return c, f, o, files
}

func originPull(t *testing.T, c *Client) (map[string]string, error) {
	t.Helper()
	from := map[string]string{}
	var mu sync.Mutex
	_, err := c.Pull(context.Background(), Request{Ref: Ref{"hf", "org/m", "main", "", ""}, Target: Target{Kind: "none"}}, func(e Event) {
		if e.Type == EvFileDone {
			mu.Lock()
			from[e.File] = e.From
			mu.Unlock()
		}
	})
	return from, err
}

// a file with a known sha256 comes from the origin and never from the
// source; the origin gets no token
func TestAnOriginServesWhatItHas(t *testing.T) {
	c, f, o, files := originSetup(t, "", true)
	from, err := originPull(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(from["model.safetensors"], UserOrigin) {
		t.Fatalf("model.safetensors came from %q", from["model.safetensors"])
	}
	if from["config.json"] != "" || from["sub/tokenizer.json"] != "" {
		t.Fatalf("files without a published sha256 were asked from the origin: %v", from)
	}
	if got := f.served.Load(); got >= int64(len(files[0].data)) {
		t.Fatalf("the source served %d bytes, the origin had the weights", got)
	}
	for _, a := range o.auth {
		if a != "" {
			t.Fatal("the origin received an Authorization header")
		}
	}
	for _, p := range o.asked {
		if p != "/blobs/sha256/"+sha(files[0].data) {
			t.Fatalf("the origin was asked for %s", p)
		}
	}
	for _, ff := range files {
		if !c.store.Has(sha(ff.data), int64(len(ff.data))) {
			t.Fatalf("%s is not in the store", ff.path)
		}
	}
}

// an origin that ignores ranges serves the whole file in one stream
func TestAnOriginWithoutRangesServesWholeFiles(t *testing.T) {
	c, f, _, files := originSetup(t, "noranges", true)
	from, err := originPull(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(from["model.safetensors"], UserOrigin) || f.served.Load() >= int64(len(files[0].data)) {
		t.Fatalf("model.safetensors came from %q, the source served %d bytes", from["model.safetensors"], f.served.Load())
	}
}

// an origin that lies, fails, misses the file or dies half way leaves the
// download to the source, which never resumes the origin's chunks
func TestAFailingOriginFallsBackToTheSource(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		withBlob bool
	}{{"lie", true}, {"500", true}, {"half", true}, {"", false}} {
		c, f, _, files := originSetup(t, tc.mode, tc.withBlob)
		from, err := originPull(t, c)
		if err != nil {
			t.Fatalf("%s: %v", tc.mode, err)
		}
		if from["model.safetensors"] != "" {
			t.Fatalf("%s/%v: model.safetensors came from %q", tc.mode, tc.withBlob, from["model.safetensors"])
		}
		w := files[0].data
		if !c.store.Has(sha(w), int64(len(w))) {
			t.Fatalf("%s: the weights are not in the store", tc.mode)
		}
		if got := f.served.Load(); got < int64(len(w)) {
			t.Fatalf("%s: the source served %d bytes of %d: it completed the origin's chunks", tc.mode, got, len(w))
		}
	}
}

// an origin that served other bytes is not asked again by the same client
func TestALyingOriginIsSkipped(t *testing.T) {
	c, _, o, _ := originSetup(t, "lie", true)
	if _, err := originPull(t, c); err != nil {
		t.Fatal(err)
	}
	if !c.nodeIsDown(UserOrigin + hostOf(c.opt.Origins[0])) {
		t.Fatal("the lying origin is not marked down")
	}
	o.mu.Lock()
	before := len(o.asked)
	o.mu.Unlock()
	if _, _, ok := c.fromOrigins(context.Background(), File{Path: "x", Size: 3, SHA256: strings.Repeat("a", 64)}, t.TempDir()+"/p", t.TempDir()+"/s", &counters{}); ok {
		t.Fatal("a skipped origin served a file")
	}
	if len(o.asked) != before {
		t.Fatal("the lying origin was asked again")
	}
}

func TestParseOrigins(t *testing.T) {
	got, err := parseOrigins([]string{"https://a.example/, http://10.0.0.5:8080", "https://b.example/base"})
	if err != nil || strings.Join(got, " ") != "https://a.example http://10.0.0.5:8080 https://b.example/base" {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range []string{"ftp://a.example", "https://u:p@a.example", "https://a.example/?x=1", "https://a.example/#f", "a.example", "https://", "mailto:x@y"} {
		if _, err := parseOrigins([]string{bad}); err == nil || AsError(err).Code != ExitUsage {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	if _, err := parseOrigins([]string{strings.Repeat("https://a.example ", maxOrigins+1)}); err == nil {
		t.Error("more than the maximum number of origins accepted")
	}
}
