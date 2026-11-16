package lmget

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Hardening: an absurd size from a server is refused instead of overflowing.
func TestHugeReportedSizeIsRefused(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-0/9223372036854775807")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("x"))
	}))
	defer srv.Close()
	t.Setenv("HF_TOKEN", "")
	c, err := New(Options{Home: t.TempDir(), HFEndpoint: "https://hf.invalid", HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pullURL(c, srv.URL+"/huge.bin", t.TempDir()); err == nil {
		t.Fatal("a size of MaxInt64 was accepted")
	}
}

// Hardening: committing a blob that is already stored keeps the existing
// inode, so hard links made from it stay recognisably lmget's.
func TestCommitKeepsAnExistingBlob(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := randBytes(1000)
	put := func() {
		part, _ := st.tmpPaths("k")
		os.MkdirAll(filepath.Dir(part), 0o755)
		os.WriteFile(part, data, 0o644)
		if err := st.commit(part, sha(data), ""); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(part); err == nil {
			t.Fatal("the part file was left behind")
		}
	}
	put()
	before, _ := os.Stat(st.BlobPath(sha(data)))
	put()
	after, _ := os.Stat(st.BlobPath(sha(data)))
	if !os.SameFile(before, after) {
		t.Fatal("a second commit replaced the stored blob with a new inode")
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

// Audit run 4, hardening: chunks another origin left on disk are not
// completed from a node. A partial file "from the source" full of garbage
// and marked done would otherwise fail the final check and get the node,
// which serves the right bytes, skipped as bad.
func TestANodeDoesNotResumeAnotherOriginsChunks(t *testing.T) {
	m := setupM3(t)
	u := m.client(t, t.TempDir())
	if _, err := u.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	var big LockFile
	for _, f := range m.lock.Models[0].Files {
		if f.Size > big.Size {
			big = f
		}
	}
	part, state := u.store.tmpPaths(big.SHA256)
	if err := os.MkdirAll(filepath.Dir(part), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part, make([]byte, big.Size), 0o644); err != nil { // zeros, not the model
		t.Fatal(err)
	}
	chunk := int64(256 << 10) // m.client's ChunkSize
	done := make([]bool, (big.Size+chunk-1)/chunk)
	for i := range done {
		done[i] = true
	}
	b, _ := json.Marshal(fetchState{Size: big.Size, Chunk: chunk, Done: done}) // written by the source
	if err := os.WriteFile(state, b, 0o644); err != nil {
		t.Fatal(err)
	}
	m.hf.down.Store(true)
	if _, err := u.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil); err != nil {
		t.Fatalf("the pull from the node failed: %v", err)
	}
	if !u.store.Has(big.SHA256, big.Size) {
		t.Fatal("the file is not in the store")
	}
}
