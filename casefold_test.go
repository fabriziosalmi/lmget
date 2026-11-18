package lmget

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNameCollisions(t *testing.T) {
	p := &Plan{Files: []File{{Path: "Config.json"}, {Path: "config.json"}, {Path: "café.json"}, {Path: "café.json"}, {Path: "model.safetensors"}}}
	got := nameCollisions(p)
	if len(got) != 2 {
		t.Fatalf("collisions %v, want Config/config and the two cafe", got)
	}
	if c := nameCollisions(&Plan{Files: []File{{Path: "a/b.json"}, {Path: "a/c.json"}}}); len(c) != 0 {
		t.Fatalf("distinct names collide: %v", c)
	}
}

// Found by the 2026-11-18 review: on macOS and Windows two names that differ
// only in case are one file, and the second silently replaced the first.
func TestCaseCollidingFilesAreRefusedWhereTheyCollide(t *testing.T) {
	f := &fakeHF{files: []fakeFile{
		{path: "Config.json", data: []byte(`{"a":1}`)},
		{path: "config.json", data: []byte(`{"a":2}`)},
		{path: "model.safetensors", data: fakeSafetensors(1000), lfs: true},
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	dir := t.TempDir()
	_, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "dir", Path: dir}}, nil)
	if caseFolds(dir) {
		if e := AsError(err); e == nil || e.Code != ExitUsage || !strings.Contains(e.Msg, "would be one file") {
			t.Fatalf("a case collision on a folding filesystem: %v", err)
		}
		if c.store.Has(sha([]byte(`{"a":1}`)), 7) {
			t.Fatal("refused, but after downloading")
		}
	} else if err != nil {
		t.Fatalf("on a case-sensitive filesystem the two files are fine: %v", err)
	}
	// the store alone never collides: blobs are named by hash
	if _, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil); err != nil {
		t.Fatalf("--to none: %v", err)
	}
}

// replaceWithCopy turns an exported hard link into a copy, as lmget makes on
// Windows when the target is on another drive.
func replaceWithCopy(t *testing.T, p string) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(p)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Found by the 2026-11-18 review: lmget's copies on Windows (another drive)
// were not its own: rm left them, an app update failed on them, and every
// get copied the HF snapshot again.
func TestLmgetsOwnCopiesAreItsOwn(t *testing.T) {
	copiesAreLinks = true
	defer func() { copiesAreLinks = runtime.GOOS == "windows" }()
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())

	// rm removes the copy, keeps a file of the user's
	dir := t.TempDir()
	pullTo(t, c, "org/m", Target{Kind: "dir", Path: dir})
	cfg := filepath.Join(dir, "config.json")
	replaceWithCopy(t, cfg)
	mine := filepath.Join(dir, "sub", "tokenizer.json")
	os.Remove(mine)
	os.WriteFile(mine, []byte("the user's own"), 0o644)
	if _, err := c.Remove(hfRef("org/m")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatal("rm left lmget's copy behind")
	}
	if b, _ := os.ReadFile(mine); string(b) != "the user's own" {
		t.Fatal("rm removed a file of the user's")
	}

	// an app update replaces the copy instead of stopping at it
	tg := appTarget(t, "lmstudio")
	pullTo(t, c, "org/m", tg)
	appCfg := filepath.Join(tg.Path, "org", "m", "config.json")
	replaceWithCopy(t, appCfg)
	f.files[1].data = []byte(`{"a":7}`)
	if _, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: tg}, nil); err != nil {
		t.Fatalf("an app update stopped at lmget's copy: %v", err)
	}
	if b, _ := os.ReadFile(appCfg); string(b) != `{"a":7}` {
		t.Fatalf("the copy was not updated: %s", b)
	}

	// the HF snapshot: a right copy is not copied again
	cache := t.TempDir()
	t.Setenv("HF_HUB_CACHE", cache)
	pullTo(t, c, "org/m", Target{Kind: "hf"})
	snapCfg := filepath.Join(cache, "models--org--m", "snapshots", commit, "config.json")
	replaceWithCopy(t, snapCfg)
	before, _ := os.Stat(snapCfg)
	pullTo(t, c, "org/m", Target{Kind: "hf"})
	after, _ := os.Stat(snapCfg)
	if !os.SameFile(before, after) {
		t.Fatal("a right copy in the snapshot was replaced")
	}
}
