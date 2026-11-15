package lmget

// Regression tests for the code-metrics audit of 2026-11-15 (commit 2e5ef6e).
// Each one failed, or would have, before its fix.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveRejectsNegativeSize(t *testing.T) {
	f := &fakeHF{tree: func() []map[string]any {
		return []map[string]any{{"type": "file", "path": "a.bin", "size": -5, "oid": strings.Repeat("a", 40)}}
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	if _, err := c.Resolve(context.Background(), hfRef("org/m"), Selection{}); err == nil {
		t.Fatal("a negative size was accepted (it used to panic in fetch)")
	}
}

func hfRef(repo string) Ref { return Ref{"hf", repo, "main", "", ""} }

// A pickle named pytorch_model.bin next to a decoy pytorch_model.xml looks
// like OpenVINO by name; its content must still get it refused.
func TestPickleBehindDecoyXMLIsRefused(t *testing.T) {
	files := append(modelFiles(),
		fakeFile{path: "pytorch_model.bin", data: torchZip(t), lfs: true},
		fakeFile{path: "pytorch_model.xml", data: []byte("<net/>")})
	f := &fakeHF{files: files}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	dir := t.TempDir()
	c := newTestClient(t, srv, t.TempDir())
	_, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "dir", Path: dir}}, nil)
	if e := AsError(err); e == nil || e.Kind != "format" {
		t.Fatalf("got %v, want a format error", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pytorch_model.bin")); err == nil {
		t.Fatal("the pickle was exported")
	}
}

func TestRedirectFromHTTPSToHTTPIsRefused(t *testing.T) {
	plain := &fakeURL{}
	plain.set(randBytes(1000))
	ps := httptest.NewServer(plain)
	defer ps.Close()
	tls := httptest.NewTLSServer(http.RedirectHandler(ps.URL+"/m.bin", http.StatusFound))
	defer tls.Close()
	t.Setenv("HF_TOKEN", "")
	c, err := New(Options{Home: t.TempDir(), HFEndpoint: "https://hf.invalid", HTTP: tls.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pullURL(c, tls.URL+"/m.bin", t.TempDir())
	if err == nil || !strings.Contains(AsError(err).Cause+AsError(err).Msg, "https to http") {
		t.Fatalf("got %v, want the downgrade refused", err)
	}
	if plain.served.Load() != 0 {
		t.Fatalf("%d bytes were fetched over plain http", plain.served.Load())
	}
}

func TestTreePaginationThatNeverEndsStops(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models/org/m/revision/main", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"sha": commit})
	})
	mux.HandleFunc("/api/models/org/m/tree/"+commit, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "<http://"+r.Host+r.URL.Path+"?recursive=true>; rel=\"next\"") // itself
		w.Write([]byte("[]"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	done := make(chan error, 1)
	go func() { _, err := c.Resolve(context.Background(), hfRef("org/m"), Selection{}); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a self-referencing listing was accepted")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pagination loops forever")
	}
}

// A .state file whose .part is gone must not make the next run skip the
// download and then blame the source.
func TestStateWithoutPartialFileStartsOver(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	home := t.TempDir()
	c := newTestClient(t, srv, home)
	big := f.files[0]
	_, state := c.store.tmpPaths(sha(big.data))
	n := (len(big.data) + int(c.opt.ChunkSize) - 1) / int(c.opt.ChunkSize)
	done := make([]bool, n)
	for i := range done {
		done[i] = true
	}
	b, _ := json.Marshal(fetchState{Size: int64(len(big.data)), Chunk: c.opt.ChunkSize, Done: done})
	os.WriteFile(state, b, 0o644)
	if _, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil); err != nil {
		t.Fatalf("a stale state broke the download: %v", err)
	}
}

// Files of a pull that was interrupted belong to it: removing another model
// must not reclaim them.
func TestRemoveKeepsBlobsOfAnInterruptedPull(t *testing.T) {
	other := []fakeFile{{path: "other.json", data: []byte(`{"o":1}`)}}
	f := &fakeHF{files: modelFiles(), byRepo: map[string][]fakeFile{"org/other": other}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	stop := func(total int64) {
		if total > 1<<20 { // config.json is committed, model.safetensors is half way
			cancel()
		}
	}
	f.onByte.Store(&stop)
	if _, err := c.Pull(ctx, Request{Ref: hfRef("org/big"), Target: Target{Kind: "none"}}, nil); AsError(err).Code != ExitInterrupted {
		t.Fatalf("setup: %v", err)
	}
	f.onByte.Store(nil)
	cfg := modelFiles()[1].data
	if s, ok := c.store.LookupGit(gitID(cfg)); !ok || !c.store.Has(s, int64(len(cfg))) {
		t.Fatal("setup: config.json was not committed before the interruption")
	}
	pullTo(t, c, "org/other", Target{Kind: "none"})
	if _, err := c.Remove(hfRef("org/other")); err != nil {
		t.Fatal(err)
	}
	if s, ok := c.store.LookupGit(gitID(cfg)); !ok || !c.store.Has(s, int64(len(cfg))) {
		t.Fatal("rm of another model deleted a verified file of the interrupted pull")
	}
	if list, _ := c.List(); len(list) != 1 || list[0].Missing == 0 {
		t.Fatalf("the interrupted model should be listed as incomplete: %+v", list)
	}
}

func TestRemoveRefusesWhileAManifestIsUnreadable(t *testing.T) {
	other := []fakeFile{{path: "other.json", data: []byte(`{"o":1}`)}}
	f := &fakeHF{files: modelFiles(), byRepo: map[string][]fakeFile{"org/other": other}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	home := t.TempDir()
	c := newTestClient(t, srv, home)
	pullTo(t, c, "org/a", Target{Kind: "none"})
	pullTo(t, c, "org/other", Target{Kind: "none"})
	before := c.Stats().Blobs
	os.WriteFile(filepath.Join(home, "refs", "hf", "org", "a", commit+".json"), []byte("{trunc"), 0o644)
	if _, err := c.Remove(hfRef("org/other")); err == nil {
		t.Fatal("rm went ahead with an unreadable manifest")
	}
	if c.Stats().Blobs != before {
		t.Fatal("blobs were deleted although a manifest could not be read")
	}
	failed := false
	for _, ch := range c.Doctor(context.Background(), DoctorOptions{}) {
		if ch.Name == "manifests" && ch.Status == "fail" {
			failed = true
		}
	}
	if !failed {
		t.Fatal("doctor does not report the unreadable manifest")
	}
}

// A same-size file with other bytes in the HF cache is replaced by the
// verified one; a correct one left by huggingface_hub is kept.
func TestHFExportReplacesAWrongForeignBlob(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cache := t.TempDir()
	t.Setenv("HF_HUB_CACHE", cache)
	w := f.files[0]
	wrong := filepath.Join(cache, "models--org--m", "blobs", sha(w.data))
	os.MkdirAll(filepath.Dir(wrong), 0o755)
	os.WriteFile(wrong, make([]byte, len(w.data)), 0o644)
	c := newTestClient(t, srv, t.TempDir())
	out := pullTo(t, c, "org/m", Target{Kind: "hf"})
	if b, _ := os.ReadFile(filepath.Join(out.Path, "model.safetensors")); !bytes.Equal(b, w.data) {
		t.Fatal("the HF cache still serves the wrong bytes after a verified pull")
	}
}

// Links made before an export failed are recorded, so a later rm removes them.
func TestExportRetryKeepsEveryLinkRecorded(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	conflict := filepath.Join(dir, "sub", "tokenizer.json")
	os.WriteFile(conflict, []byte("mine"), 0o644)
	c := newTestClient(t, srv, t.TempDir())
	req := Request{Ref: hfRef("org/m"), Target: Target{Kind: "dir", Path: dir}}
	if _, err := c.Pull(context.Background(), req, nil); err == nil {
		t.Fatal("setup: the conflicting file was overwritten")
	}
	os.Remove(conflict)
	if _, err := c.Pull(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Remove(hfRef("org/m")); err != nil {
		t.Fatal(err)
	}
	for _, ff := range f.files {
		if _, err := os.Lstat(filepath.Join(dir, ff.path)); err == nil {
			t.Errorf("%s survived rm", ff.path)
		}
	}
}

func TestRemoveKeepsTheManifestWhenALinkCannotBeRemoved(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	dir := t.TempDir()
	c := newTestClient(t, srv, t.TempDir())
	pullTo(t, c, "org/m", Target{Kind: "dir", Path: dir})
	os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)
	if _, err := c.Remove(hfRef("org/m")); AsError(err).Code != ExitDisk {
		t.Fatalf("got %v, want a disk error", err)
	}
	if list, _ := c.List(); len(list) != 1 {
		t.Fatal("the model was forgotten although its links are still on disk")
	}
	os.Chmod(dir, 0o755)
	if _, err := c.Remove(hfRef("org/m")); err != nil {
		t.Fatalf("second rm after fixing permissions: %v", err)
	}
}

func TestRemoveWaitsForNobodyButRefusesDuringAPull(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	pullTo(t, c, "org/m", Target{Kind: "none"})
	unlock, err := c.store.flock(".lock", false, true) // what a running pull holds
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Remove(hfRef("org/m"))
	unlock()
	if e := AsError(err); e == nil || e.Kind != "busy" {
		t.Fatalf("got %v, want busy", err)
	}
	if _, err := c.Remove(hfRef("org/m")); err != nil {
		t.Fatalf("after the pull: %v", err)
	}
}

func TestExportRefusesUnsafePaths(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	out := pullTo(t, c, "org/m", Target{Kind: "none"})
	p := *out.Plan
	p.Files = append([]File(nil), out.Plan.Files...)
	p.Files[0].Path = "../escape.safetensors"
	if _, err := c.Export(&p, Target{Kind: "dir", Path: t.TempDir()}); err == nil {
		t.Fatal("export accepted a path that leaves the target")
	}
}

func TestUnreadableTokenFileIsReported(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("hf_secret"), 0o000)
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HF_TOKEN_PATH", tok)
	if _, src := hfToken(); !strings.HasPrefix(src, "unreadable:") {
		t.Fatalf("source %q: an unreadable token file is reported as missing", src)
	}
}

// An export that fails half way: rm right after must remove what it linked.
func TestFailedExportLinksAreRemovedByRm(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "tokenizer.json"), []byte("mine"), 0o644)
	c := newTestClient(t, srv, t.TempDir())
	if _, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "dir", Path: dir}}, nil); err == nil {
		t.Fatal("setup: the conflicting file was overwritten")
	}
	if _, err := c.Remove(hfRef("org/m")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"config.json", "model.safetensors"} {
		if _, err := os.Lstat(filepath.Join(dir, p)); err == nil {
			t.Errorf("%s, linked before the failure, survived rm", p)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "sub", "tokenizer.json")); string(b) != "mine" {
		t.Fatal("rm touched the user's file")
	}
}

// Manifests written before exports were recorded (M1 slice 1) list no links:
// a new pull adopts the links that are already lmget's, so rm removes them.
func TestRepullAdoptsLinksOfAnOlderManifest(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	home, dir := t.TempDir(), t.TempDir()
	c := newTestClient(t, srv, home)
	out := pullTo(t, c, "org/m", Target{Kind: "dir", Path: dir})
	legacy, _ := json.Marshal(out.Plan) // the early format: a bare Plan, no exports
	os.WriteFile(filepath.Join(home, "refs", "hf", "org", "m", commit+".json"), legacy, 0o644)
	pullTo(t, c, "org/m", Target{Kind: "dir", Path: dir})
	if _, err := c.Remove(hfRef("org/m")); err != nil {
		t.Fatal(err)
	}
	for _, ff := range f.files {
		if _, err := os.Lstat(filepath.Join(dir, ff.path)); err == nil {
			t.Errorf("%s survived rm", ff.path)
		}
	}
}
