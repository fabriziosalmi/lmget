package lmget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
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

// Audit run 5: a primary source is an origin too. A URL server that claimed
// a Hugging Face file's sha256 and stopped leaves chunks (with an empty
// origin, as before this fix, or its own host) that a later Hugging Face
// download must not complete.
func TestAURLServersPartialIsNotResumedByHuggingFace(t *testing.T) {
	for _, left := range []string{"", "src:evil.example"} {
		f := &fakeHF{files: modelFiles()}
		srv := httptest.NewServer(f.handler())
		c := newTestClient(t, srv, t.TempDir())
		var big fakeFile
		for _, x := range f.files {
			if len(x.data) > len(big.data) {
				big = x
			}
		}
		sum := sha256.Sum256(big.data)
		part, state := c.store.tmpPaths(hex.EncodeToString(sum[:]))
		os.MkdirAll(filepath.Dir(part), 0o755)
		os.WriteFile(part, make([]byte, len(big.data)), 0o644) // not the file
		chunk := c.opt.ChunkSize
		done := make([]bool, (int64(len(big.data))+chunk-1)/chunk)
		for i := range done {
			done[i] = true
		}
		b, _ := json.Marshal(fetchState{Size: int64(len(big.data)), Chunk: chunk, Done: done, Origin: left})
		os.WriteFile(state, b, 0o644)
		if _, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil); err != nil {
			t.Errorf("origin %q left behind: the Hugging Face pull failed: %v", left, err)
		}
		srv.Close()
	}
}

// A download makes its chunks durable in groups, not one sync per chunk: on
// macOS a sync flushes the drive's cache. The state still never lists a
// chunk before its sync (checkpoint), and the last group is synced in every
// case (TestPullResumesWithoutRefetching interrupts before any timed one).
func TestChunksAreSyncedInGroups(t *testing.T) {
	big := fakeSafetensors(16 << 20) // 65 chunks of 256 KiB
	var syncs atomic.Int64
	old := syncFile
	syncFile = func(f *os.File) error { syncs.Add(1); return f.Sync() }
	defer func() { syncFile, checkpointEvery, checkpointBytes = old, time.Second, 256<<20 }()
	for _, tc := range []struct {
		every    time.Duration
		bytes    int64
		min, max int64
	}{
		{time.Hour, 1 << 40, 1, 1},      // one sync, at the end
		{time.Hour, 16 << 20 / 4, 4, 6}, // every quarter of the file
	} {
		checkpointEvery, checkpointBytes = tc.every, tc.bytes
		f := &fakeHF{files: []fakeFile{{path: "model.safetensors", data: big, lfs: true}}}
		srv := httptest.NewServer(f.handler())
		c := newTestClient(t, srv, t.TempDir())
		syncs.Store(0)
		if _, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		if n := syncs.Load(); n < tc.min || n > tc.max {
			t.Errorf("checkpoint every %v or %d bytes: %d syncs for 65 chunks, want %d to %d", tc.every, tc.bytes, n, tc.min, tc.max)
		}
	}
}
