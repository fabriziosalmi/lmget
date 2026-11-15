package lmget

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func pullTo(t *testing.T, c *Client, repo string, target Target) *Outcome {
	t.Helper()
	out, err := c.Pull(context.Background(), Request{Ref: Ref{"hf", repo, "main"}, Target: target}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func totalSize(files []fakeFile) (n int64) {
	for _, f := range files {
		n += int64(len(f.data))
	}
	return
}

// Two models share every blob. Removing one keeps the blobs; removing the
// second frees exactly their size, and the HF cache is cleaned of both.
func TestRemoveKeepsSharedBlobsAndFreesTheRest(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cache := t.TempDir()
	t.Setenv("HF_HUB_CACHE", cache)
	c := newTestClient(t, srv, t.TempDir())
	pullTo(t, c, "org/a", Target{Kind: "hf"})
	pullTo(t, c, "org/b", Target{Kind: "hf"})
	if st := c.Stats(); st.Blobs != len(f.files) {
		t.Fatalf("dedup: %d blobs for 2 models sharing %d files", st.Blobs, len(f.files))
	}

	r, err := c.Remove(Ref{"hf", "org/a", "main"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Blobs != 0 || r.Freed != 0 {
		t.Fatalf("removing org/a deleted shared blobs: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(cache, "models--org--a")); !os.IsNotExist(err) {
		t.Fatalf("org/a still in the HF cache: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(cache, "models--org--b", "snapshots", commit, "config.json"))
	if err != nil || string(b) != `{"a":1}` {
		t.Fatalf("org/b damaged by removing org/a: %v", err)
	}

	// the user hard-links the weights somewhere else: that space stays in use
	extra := filepath.Join(t.TempDir(), "my-copy.safetensors")
	if err := os.Link(c.store.BlobPath(sha(f.files[0].data)), extra); err != nil {
		t.Fatal(err)
	}
	r, err = c.Remove(Ref{"hf", "org/b", "main"})
	if err != nil {
		t.Fatal(err)
	}
	held := int64(len(f.files[0].data))
	if r.Blobs != len(f.files) || r.Freed != totalSize(f.files)-held || r.Held != held {
		t.Fatalf("got %+v, want %d blobs, %d freed, %d held", r, len(f.files), totalSize(f.files)-held, held)
	}
	if b, err := os.ReadFile(extra); err != nil || !bytes.Equal(b, f.files[0].data) {
		t.Fatal("the user's hard link lost its content")
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 0 {
		t.Fatalf("HF cache not empty after removing everything: %v", entries)
	}
	if list, _ := c.List(); len(list) != 0 {
		t.Fatalf("still listed: %+v", list)
	}
	if _, err := c.Remove(Ref{"hf", "org/b", "main"}); AsError(err).Code != ExitNotFound {
		t.Fatalf("removing twice: %v", err)
	}
}

// Files lmget did not create, or that were changed after export, survive rm.
func TestRemoveLeavesFilesItDidNotCreate(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cache := t.TempDir()
	t.Setenv("HF_HUB_CACHE", cache)
	// huggingface_hub already downloaded the weights before lmget ran
	foreign := filepath.Join(cache, "models--org--m", "blobs", sha(f.files[0].data))
	os.MkdirAll(filepath.Dir(foreign), 0o755)
	os.WriteFile(foreign, f.files[0].data, 0o644)
	dir := t.TempDir()
	c := newTestClient(t, srv, t.TempDir())
	pullTo(t, c, "org/m", Target{Kind: "hf"})
	pullTo(t, c, "org/m", Target{Kind: "dir", Path: dir})
	// the user replaces one exported file with their own
	mine := filepath.Join(dir, "config.json")
	os.Remove(mine)
	os.WriteFile(mine, []byte("edited"), 0o644)

	r, err := c.Remove(Ref{"hf", "org/m", "main"})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(foreign); err != nil || !bytes.Equal(b, f.files[0].data) {
		t.Fatalf("removed or changed a blob huggingface_hub downloaded: %v", err)
	}
	if b, _ := os.ReadFile(mine); string(b) != "edited" {
		t.Fatal("removed a file the user changed")
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "tokenizer.json")); !os.IsNotExist(err) {
		t.Fatal("left an lmget link in the dir export")
	}
	if r.Freed != totalSize(f.files) {
		t.Fatalf("freed %d, want %d (the foreign copy is a separate file)", r.Freed, totalSize(f.files))
	}
}

// A blob corrupted on disk is found by doctor --verify, moved to quarantine
// together with its hard links, and the next pull restores good bytes.
func TestDoctorVerifyQuarantinesAndPullRepairs(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cache := t.TempDir()
	t.Setenv("HF_HUB_CACHE", cache)
	home := t.TempDir()
	c := newTestClient(t, srv, home)
	out := pullTo(t, c, "org/m", Target{Kind: "hf"})

	blob := c.store.BlobPath(sha(f.files[0].data))
	os.Chmod(blob, 0o644)
	b, _ := os.ReadFile(blob)
	b[100] ^= 1
	os.WriteFile(blob, b, 0o644)

	var verify *Check
	cs := c.Doctor(context.Background(), DoctorOptions{Verify: true})
	for i := range cs {
		if cs[i].Name == "verify" {
			verify = &cs[i]
		}
	}
	if verify == nil || verify.Status != "fail" {
		t.Fatalf("doctor --verify missed the corruption: %+v", cs)
	}
	if _, err := os.Stat(filepath.Join(home, "quarantine", sha(f.files[0].data))); err != nil {
		t.Fatal("corrupt blob not in quarantine")
	}
	pullTo(t, c, "org/m", Target{Kind: "hf"})
	got, err := os.ReadFile(filepath.Join(out.Path, "model.safetensors"))
	if err != nil || !bytes.Equal(got, f.files[0].data) {
		t.Fatalf("the HF cache still serves corrupt bytes after repair: %v", err)
	}
	cs = c.Doctor(context.Background(), DoctorOptions{Verify: true})
	for _, ch := range cs {
		if ch.Name == "verify" && ch.Status != "ok" {
			t.Fatalf("after repair: %+v", ch)
		}
	}
}
