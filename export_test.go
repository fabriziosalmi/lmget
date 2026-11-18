package lmget

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// MEDIUM: names that some filesystems fold onto .git (HFS+ drops ignorable
// code points) are refused like .git itself, in lock targets, file paths
// and export directories.
func TestGitAliasesWithIgnorableCodePointsAreRefused(t *testing.T) {
	aliases := []string{".g\u200cit", ".gi\u200dt", "\ufeff.git", ".G\u200bIT", ".g\u00adit", ".git\ufe0f"}
	for _, a := range aliases {
		if err := lockTargetSyntax("dir:./" + a); err == nil {
			t.Errorf("lock target %q accepted", a)
		}
		if isLocalSlash(a + "/commondir") {
			t.Errorf("file path %q accepted", a+"/commondir")
		}
		if !isGitName(a) {
			t.Errorf("%q is not recognised as a .git alias", a)
		}
	}
	for _, ok := range []string{"git", ".github", "models", "my.git.txt"} {
		if isGitName(ok) {
			t.Errorf("%q wrongly taken for .git", ok)
		}
	}
	// a source that lists such a path is refused at resolve time
	f := &fakeHF{tree: func() []map[string]any {
		return []map[string]any{{"type": "file", "path": ".g\u200cit/commondir", "size": 1, "oid": strings.Repeat("a", 40)}}
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	if _, err := newTestClient(t, srv, t.TempDir()).Resolve(context.Background(), hfRef("org/m"), Selection{}); err == nil {
		t.Fatal("a source file path aliasing .git was accepted")
	}
}

// Hardening: the library's Export waits for an rm or doctor --verify that
// holds the store, as a pull does.
func TestExportWaitsForTheStoreLock(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	t.Setenv("HF_HUB_CACHE", t.TempDir())
	c := newTestClient(t, srv, t.TempDir())
	out := pullTo(t, c, "org/m", Target{Kind: "none"})
	unlock, err := c.store.flock(".lock", true, false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := c.Export(out.Plan, Target{Kind: "dir", Path: t.TempDir()}); done <- err }()
	select {
	case <-done:
		t.Fatal("Export ran while the store was held exclusively")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Found live: an export to a directory that does not exist yet must create it
// (the containment check resolves the root, so it has to exist first).
func TestExportDirCreatesTheTarget(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	dir := filepath.Join(t.TempDir(), "models", "new")
	pullTo(t, c, "org/m", Target{Kind: "dir", Path: dir})
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal(err)
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

// HIGH: a symlinked directory committed below the lock target must not carry
// a file outside the project.
func TestDirExportRefusesSymlinkedSubdirectory(t *testing.T) {
	files := []fakeFile{{path: "agents/evil.plist", data: []byte("<plist>attacker</plist>")}}
	f := &fakeHF{files: files}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	proj, outside := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(proj, "models"), 0o755)
	os.Symlink(outside, filepath.Join(proj, "models", "agents"))
	c := newTestClient(t, srv, t.TempDir())
	_, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "dir", Path: filepath.Join(proj, "models")}}, nil)
	if err == nil {
		t.Fatal("export followed a symlinked subdirectory")
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.plist")); err == nil {
		t.Fatal("a file was written outside the project")
	}
}

func TestNothingIsWrittenIntoAGitDirectory(t *testing.T) {
	// a lock target inside .git
	if err := lockTargetSyntax("dir:./.git"); err == nil {
		t.Error(`lock target "dir:./.git" accepted`)
	}
	if err := lockTargetSyntax("dir:./sub/.GIT/x"); err == nil {
		t.Error("a .GIT segment was accepted")
	}
	// a source file path inside .git
	if isLocalSlash(".git/commondir") || isLocalSlash("a/.git/hooks/x") {
		t.Error("a .git file path was accepted")
	}
	// an export directory that reaches .git through a symlink
	f := &fakeHF{files: []fakeFile{{path: "commondir", data: []byte("../../evil")}}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, ".git"), 0o755)
	os.Symlink(filepath.Join(proj, ".git"), filepath.Join(proj, "models"))
	c := newTestClient(t, srv, t.TempDir())
	if _, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "dir", Path: filepath.Join(proj, "models")}}, nil); err == nil {
		t.Fatal("exported into .git through a symlink")
	}
	if _, err := os.Stat(filepath.Join(proj, ".git", "commondir")); err == nil {
		t.Fatal("a file was created inside .git")
	}
}

// Found by the 2026-11-18 review: a pinned sync (a cloned lock) moved the
// shared Hugging Face cache's refs/main back to its old commit. Now a pinned
// plan fills an empty ref and never moves one; a fresh pull still does.
func TestPinnedSyncDoesNotMoveTheCachesMain(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	cache := t.TempDir()
	t.Setenv("HF_HUB_CACHE", cache)
	refMain := filepath.Join(cache, "models--org--m", "refs", "main")

	out := pullTo(t, c, "org/m", Target{Kind: "none"})
	l := NewLock(filepath.Join(t.TempDir(), DefaultLockName))
	if err := l.Upsert(out.Plan, Selection{}, Target{Kind: "hf"}); err != nil {
		t.Fatal(err)
	}
	req, err := l.Request(&l.Models[0], false)
	if err != nil {
		t.Fatal(err)
	}
	// an empty cache: the pinned sync fills refs/main, so offline loading works
	if _, err := c.Pull(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(refMain); string(b) != commit {
		t.Fatalf("refs/main = %q after a pinned sync into an empty cache", b)
	}
	// another project moved main on: a pinned sync leaves it there
	newer := strings.Repeat("b", 40)
	os.WriteFile(refMain, []byte(newer), 0o644)
	if _, err := c.Pull(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(refMain); string(b) != newer {
		t.Fatalf("a pinned sync moved refs/main to %q", b)
	}
	// a pull that resolves main now says where main is
	pullTo(t, c, "org/m", Target{Kind: "hf"})
	if b, _ := os.ReadFile(refMain); string(b) != commit {
		t.Fatalf("a fresh pull left refs/main at %q", b)
	}
}
