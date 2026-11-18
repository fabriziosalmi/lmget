package lmget

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeURL struct {
	data     atomic.Pointer[[]byte]
	digest   string // Repr-Digest value, if any
	noRanges bool
	served   atomic.Int64
	onByte   atomic.Pointer[func(int64)]
}

func (f *fakeURL) set(b []byte) { f.data.Store(&b) }

func (f *fakeURL) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b := *f.data.Load()
	sum := sha256.Sum256(b)
	w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sum[:8])) // strong: changes with content
	if f.digest != "" {
		w.Header().Set("Repr-Digest", f.digest)
	}
	cw := &urlCounter{ResponseWriter: w, f: f}
	if f.noRanges {
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		cw.Write(b)
		return
	}
	http.ServeContent(cw, r, "", time.Time{}, bytes.NewReader(b))
}

type urlCounter struct {
	http.ResponseWriter
	f *fakeURL
}

func (c *urlCounter) Write(b []byte) (int, error) {
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

func newURLTest(t *testing.T, f *fakeURL) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(f)
	t.Setenv("HF_TOKEN", "")
	c, err := New(Options{Home: t.TempDir(), HFEndpoint: "https://hf.invalid", Conns: 4, ChunkSize: 256 << 10, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func pullURL(c *Client, u string, dir string) (*Outcome, error) {
	ref, err := ParseRef(u)
	if err != nil {
		return nil, err
	}
	return c.Pull(context.Background(), Request{Ref: ref, Target: Target{Kind: "dir", Path: dir}}, nil)
}

func TestURLPinnedOnFirstDownloadThenSyncsOffline(t *testing.T) {
	f := &fakeURL{}
	data := randBytes(2<<20 + 77)
	f.set(data)
	c, srv := newURLTest(t, f)
	dir := t.TempDir()
	u := srv.URL + "/models/tiny.bin"
	out, err := pullURL(c, u, dir)
	if err != nil {
		t.Fatal(err)
	}
	if out.Plan.HashFrom != HashFromFirst {
		t.Fatalf("hash_from %q", out.Plan.HashFrom)
	}
	if out.Path != filepath.Join(dir, "tiny.bin") {
		t.Fatalf("path %s", out.Path)
	}
	if b, _ := os.ReadFile(out.Path); !bytes.Equal(b, data) {
		t.Fatal("content mismatch")
	}
	l := NewLock(filepath.Join(dir, DefaultLockName)) // the project directory, as in real use
	if err := l.Upsert(out.Plan, Selection{}, Target{Kind: "dir", Path: dir}); err != nil {
		t.Fatal(err)
	}
	if err := l.Write(); err != nil {
		t.Fatal(err)
	}
	l2, err := ReadLock(l.Path())
	if err != nil {
		t.Fatalf("lock with a URL entry does not read back: %v", err)
	}
	srv.Close()
	req, _ := l2.Request(&l2.Models[0], false)
	os.Remove(out.Path)
	if o, err := c.Pull(context.Background(), req, nil); err != nil || o.Fetched != 0 {
		t.Fatalf("offline sync of a URL: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "tiny.bin")); !bytes.Equal(b, data) {
		t.Fatal("offline sync did not restore the file")
	}
}

func TestURLHashSources(t *testing.T) {
	data := randBytes(600_000)
	sum := sha256.Sum256(data)
	good := fmt.Sprintf("%x", sum)
	other := strings.Repeat("ab", 32)
	cases := []struct {
		name, digest, fragment string
		code                   ExitCode
		from                   string
		downloads              bool
	}{
		{"server digest ok", "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":", "", 0, HashFromServer, true},
		{"server digest lies", "sha-256=:" + base64.StdEncoding.EncodeToString(make([]byte, 32)) + ":", "", ExitVerify, "", true},
		{"user pin ok", "", "#sha256=" + good, 0, HashFromUser, true},
		{"user pin wrong", "", "#sha256=" + other, ExitVerify, "", true},
		{"user and server disagree", "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":", "#sha256=" + other, ExitVerify, "", false},
	}
	for _, tc := range cases {
		f := &fakeURL{digest: tc.digest}
		f.set(data)
		c, srv := newURLTest(t, f)
		out, err := pullURL(c, srv.URL+"/m.bin"+tc.fragment, t.TempDir())
		srv.Close()
		got := ExitOK
		if err != nil {
			got = AsError(err).Code
		}
		if got != tc.code {
			t.Errorf("%s: exit %d (%v), want %d", tc.name, got, err, tc.code)
			continue
		}
		if err == nil && out.Plan.HashFrom != tc.from {
			t.Errorf("%s: hash_from %q, want %q", tc.name, out.Plan.HashFrom, tc.from)
		}
		if !tc.downloads && f.served.Load() > 1 {
			t.Errorf("%s: downloaded %d bytes before refusing", tc.name, f.served.Load())
		}
	}
}

// The server replaces the file in the middle of a download: the run stops,
// and the next run fetches the new version from scratch, never a mix.
func TestURLChangeMidDownloadNeverMixes(t *testing.T) {
	f := &fakeURL{}
	v1, v2 := randBytes(3<<20), randBytes(3<<20)
	f.set(v1)
	c, srv := newURLTest(t, f)
	defer srv.Close()
	swap := func(total int64) {
		if total > 1<<20 {
			f.set(v2)
		}
	}
	f.onByte.Store(&swap)
	dir := t.TempDir()
	_, err := pullURL(c, srv.URL+"/m.bin", dir)
	if e := AsError(err); e == nil || e.Kind != "changed" {
		t.Fatalf("got %v, want a 'changed' error", err)
	}
	f.onByte.Store(nil)
	out, err := pullURL(c, srv.URL+"/m.bin", dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out.Path); !bytes.Equal(b, v2) {
		t.Fatal("result is not exactly the new version")
	}
}

func TestURLServerWithoutRanges(t *testing.T) {
	f := &fakeURL{noRanges: true}
	data := randBytes(1<<20 + 5)
	f.set(data)
	c, srv := newURLTest(t, f)
	defer srv.Close()
	out, err := pullURL(c, srv.URL+"/m.bin", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out.Path); !bytes.Equal(b, data) {
		t.Fatal("content mismatch")
	}
}

func TestLockRefusesURLsWithQuery(t *testing.T) {
	ref, err := ParseRef("https://bucket.example/m.gguf?X-Amz-Signature=abc")
	if err != nil {
		t.Fatal(err)
	}
	if err := Lockable(ref); err == nil {
		t.Fatal("a signed URL would be written to the lock")
	}
	p := &Plan{Ref: ref, Commit: urlID(ref.Repo), Files: []File{{Path: "m.gguf", Size: 1, SHA256: strings.Repeat("a", 64)}}}
	if err := NewLock(filepath.Join(t.TempDir(), "l")).Upsert(p, Selection{}, Target{Kind: "dir", Path: "."}); err == nil {
		t.Fatal("Upsert accepted a URL with a query string")
	}
}

func torchZip(t *testing.T) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create("archive/data.pkl")
	w.Write([]byte{0x80, 0x02, '}', 'q', 0, '.'})
	zw.Close()
	return b.Bytes()
}

func TestFormatCheckRefusesDisguisedFiles(t *testing.T) {
	gguf := fakeGGUF(1000)
	pickle := append([]byte{0x80, 0x04, 0x95}, randBytes(1000)...)
	cases := []struct {
		name string
		data []byte
		ok   bool
	}{
		{"model.safetensors", pickle, false},          // pickle disguised as safetensors
		{"model.safetensors", torchZip(t), false},     // torch zip disguised as safetensors
		{"model.safetensors", randBytes(1000), false}, // not safetensors at all
		{"model.gguf", pickle, false},
		{"model.onnx", pickle, false},           // no format check for .onnx: only pickle sniffing catches it
		{"tokenizer.model", torchZip(t), false}, // same, torch zip form
		{"model.gguf", randBytes(1000), false},
		{"model.gguf", gguf, true},
		{"model.safetensors", stFile(`{}`, 1000), false},       // well-formed JSON, bytes no tensor owns
		{"model.gguf", gguf[:len(gguf)-1], false},              // its one tensor runs past the end
		{"model.gguf", ggufAround(make([]byte, 31<<10)), true}, // over one read-ahead buffer of data
		{"model.safetensors", fakeSafetensors(1000), true},
		{"pytorch_model.bin", torchZip(t), true}, // named as what it is
		{"weights.pt", pickle, true},
		{"notes.txt", []byte("hello"), true},
	}
	for _, tc := range cases {
		f := &fakeURL{}
		f.set(tc.data)
		c, srv := newURLTest(t, f)
		dir := t.TempDir()
		_, err := pullURL(c, srv.URL+"/"+tc.name, dir)
		srv.Close()
		if tc.ok && err != nil {
			t.Errorf("%s (%d bytes): rejected: %v", tc.name, len(tc.data), err)
		}
		if !tc.ok {
			if e := AsError(err); e == nil || e.Code != ExitVerify || e.Kind != "format" {
				t.Errorf("%s: got %v, want a format error with exit %d", tc.name, err, ExitVerify)
			}
			if _, err := os.Stat(filepath.Join(dir, tc.name)); err == nil {
				t.Errorf("%s: a refused file was exported", tc.name)
			}
			if st := c.Stats(); st.Blobs != 0 || st.Partial != 0 {
				t.Errorf("%s: refused file left in the store: %+v", tc.name, st)
			}
		}
	}
}

// Found by the 2026-11-18 review: a signed URL's credentials reached the
// events, the store's manifest and (in the CLI) the history. Only the
// download sees them now, and rm still finds the model by the full URL.
func TestSignedURLCredentialsAreNotWrittenDown(t *testing.T) {
	f := &fakeURL{}
	f.set(randBytes(64 << 10))
	c, srv := newURLTest(t, f)
	const secret = "SECRETSIG123"
	u := srv.URL + "/m.dat?X-Amz-Signature=" + secret + "&X-Amz-Credential=AKIATEST"
	ref, err := ParseRef(u)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	_, err = c.Pull(context.Background(), Request{Ref: ref, Target: Target{Kind: "dir", Path: t.TempDir()}}, func(e Event) {
		b, _ := json.Marshal(e)
		seen = append(seen, string(b))
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range seen {
		if strings.Contains(s, secret) || strings.Contains(s, "AKIATEST") {
			t.Fatalf("an event carries the credential: %s", s)
		}
	}
	if !strings.Contains(strings.Join(seen, ""), "X-Amz-Signature=REDACTED") {
		t.Fatal("the redacted URL does not say what it was")
	}
	filepath.WalkDir(c.store.Root(), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json") {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), secret) {
				t.Errorf("%s keeps the credential", p)
			}
		}
		return nil
	})
	if strings.Contains(ref.Display(), secret) || strings.Contains(ref.Redacted().String(), secret) {
		t.Fatal("Display or Redacted shows the credential")
	}
	if _, err := c.Remove(ref); err != nil {
		t.Fatalf("rm by the full URL: %v", err)
	}
}
