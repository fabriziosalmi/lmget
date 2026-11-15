package lmget

// Regression tests for sec-audit run 2 on bff754b (5 confirmed findings, the
// critics' variants and the cheap hardening notes).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
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

// LOW: the offline plan must not take a git id from the lock that the
// verified index does not map to the locked sha256 (it names HF cache blobs).
func TestOfflineSyncChecksLockedGitIDs(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cache := t.TempDir()
	t.Setenv("HF_HUB_CACHE", cache)
	c := newTestClient(t, srv, t.TempDir())
	out := pullTo(t, c, "org/m", Target{Kind: "hf"})
	l := NewLock(filepath.Join(t.TempDir(), DefaultLockName))
	if err := l.Upsert(out.Plan, Selection{}, Target{Kind: "hf"}); err != nil {
		t.Fatal(err)
	}
	forged := strings.Repeat("b", 40) // another file's blob name in the HF cache
	for i := range l.Models[0].Files {
		if l.Models[0].Files[i].Path == "config.json" {
			l.Models[0].Files[i].GitSHA1 = forged
		}
	}
	req, _ := l.Request(&l.Models[0], false)
	before := f.served.Load()
	_, err := c.Pull(context.Background(), req, nil)
	if AsError(err).Code != ExitVerify {
		t.Fatalf("got %v, want the forged git id refused (exit %d)", err, ExitVerify)
	}
	if _, err := os.Stat(filepath.Join(cache, "models--org--m", "blobs", forged)); err == nil {
		t.Fatal("a blob was written under the forged git id")
	}
	_ = before
}

// MEDIUM: a lock cannot make a URL without a published hash serve a blob
// that some other download put in the store.
func TestURLLockCannotRelabelAStoredBlob(t *testing.T) {
	stored := randBytes(4096)
	real := randBytes(4096) // same size, other bytes: what the vendor really serves
	vendor := &fakeURL{}
	vendor.set(real)
	other := &fakeURL{}
	other.set(stored)
	vs := httptest.NewTLSServer(vendor)
	defer vs.Close()
	os2 := httptest.NewTLSServer(other)
	defer os2.Close()
	t.Setenv("HF_TOKEN", "")
	cl := vs.Client()
	cl.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true // both test servers
	c, err := New(Options{Home: t.TempDir(), HFEndpoint: "https://hf.invalid", HTTP: cl})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pullURL(c, os2.URL+"/seed.bin", t.TempDir()); err != nil { // the attacker's blob in the store
		t.Fatal(err)
	}
	dir := t.TempDir()
	u := vs.URL + "/tool.bin"
	l := NewLock(filepath.Join(dir, DefaultLockName))
	l.Models = []LockModel{{Ref: u, Commit: urlID(u), To: "dir:.",
		Files: []LockFile{{Path: "tool.bin", Size: int64(len(stored)), SHA256: sha(stored)}}}}
	if err := l.Write(); err != nil {
		t.Fatal(err)
	}
	l2, err := ReadLock(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	req, _ := l2.Request(&l2.Models[0], false)
	_, err = c.Pull(context.Background(), req, nil)
	if AsError(err).Code != ExitVerify {
		t.Fatalf("got %v, want exit %d: the vendor's bytes do not match the lock", err, ExitVerify)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "tool.bin")); string(b) == string(stored) {
		t.Fatal("the stored blob was served as the vendor's file")
	}
}

// Critic variant: a #sha256= fragment in a lock ref would make the lock's hash
// look published by the user; lmget never writes one, so a lock with one is refused.
func TestLockRefsCarryNoFragment(t *testing.T) {
	u := "https://example.org/m.gguf"
	text := fmt.Sprintf("version = 1\n[[model]]\nref = %q\ncommit = %q\nto = \"none\"\n[[model.file]]\npath = \"m.gguf\"\nsize = 1\nsha256 = %q\n",
		u+"#sha256="+strings.Repeat("a", 64), urlID(u), strings.Repeat("a", 64))
	p := filepath.Join(t.TempDir(), "lmget.lock")
	os.WriteFile(p, []byte(text), 0o644)
	if _, err := ReadLock(p); err == nil {
		t.Fatal("a lock ref with a #sha256= fragment was accepted")
	}
}

// Hardening: the token stays with the exact host of the endpoint.
func TestTokenIsNotForwardedToAnotherPort(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		w.Write([]byte(`{"sha":"` + commit + `"}`))
	}))
	defer other.Close()
	endpoint := httptest.NewServer(http.RedirectHandler(other.URL+"/moved", http.StatusFound))
	defer endpoint.Close()
	c, err := New(Options{Home: t.TempDir(), HFEndpoint: endpoint.URL, HFToken: "hf_test_token"})
	if err != nil {
		t.Fatal(err)
	}
	c.Resolve(context.Background(), hfRef("org/m"), Selection{})
	if leaked.Load() {
		t.Fatal("the token followed a redirect to another port")
	}
}

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

func TestIgnorableCodePointsAndUnreadableZips(t *testing.T) {
	// two names that differ only by an ignorable code point collide on HFS+
	if isLocalSlash("model‌.safetensors") {
		t.Error("a file name with an ignorable code point was accepted")
	}
	// a user-chosen export directory is not a lock target: exportContained is the guard
	root := t.TempDir()
	if err := exportContained(root, filepath.Join(root, ".g‌it", "hooks")); err == nil {
		t.Error("an export directory aliasing .git was accepted")
	}
	if err := exportContained(root, filepath.Join(root, "models", "q")); err != nil {
		t.Errorf("a normal export directory was refused: %v", err)
	}
	// a zip Go cannot read may still be opened by torch's reader: fail closed
	p := filepath.Join(t.TempDir(), "model.onnx")
	os.WriteFile(p, []byte("PK\x03\x04 this is not a readable zip archive"), 0o644)
	if err := checkFormat(p, "model.onnx", false); err == nil {
		t.Error("an unreadable zip passed the format check")
	}
}
