package lmget

// Regression tests for the sec-audit quick run on 2a2be7c (3 confirmed
// findings, 4 critic leads). Each reproduces the attack and requires it to fail.

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

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

// MEDIUM: a cloned lock re-labels bytes already in the store as a trusted
// repository's file. Offline sync needs a binding verified against the source.
func TestLockCannotRelabelStoredBytesAsAnotherModel(t *testing.T) {
	trusted := modelFiles()
	evil := []fakeFile{{path: "model.safetensors", data: fakeSafetensors(4096), lfs: true}}
	f := &fakeHF{files: trusted, byRepo: map[string][]fakeFile{"org/evil": evil}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cache := t.TempDir()
	t.Setenv("HF_HUB_CACHE", cache)
	c := newTestClient(t, srv, t.TempDir())
	out := pullTo(t, c, "org/trusted", Target{Kind: "hf"})
	pullTo(t, c, "org/evil", Target{Kind: "none"}) // attacker bytes now in the store

	l := NewLock(filepath.Join(t.TempDir(), DefaultLockName))
	if err := l.Upsert(out.Plan, Selection{}, Target{Kind: "hf"}); err != nil {
		t.Fatal(err)
	}
	for i := range l.Models[0].Files { // the attack: point the weights at the evil blob
		if l.Models[0].Files[i].Path == "model.safetensors" {
			l.Models[0].Files[i].SHA256, l.Models[0].Files[i].Size = sha(evil[0].data), int64(len(evil[0].data))
		}
	}
	req, _ := l.Request(&l.Models[0], false)
	_, err := c.Pull(context.Background(), req, nil)
	if AsError(err).Code != ExitVerify {
		t.Fatalf("got %v, want the lock refused (exit %d)", err, ExitVerify)
	}
	got, _ := os.ReadFile(filepath.Join(out.Path, "model.safetensors"))
	if !bytes.Equal(got, trusted[0].data) {
		t.Fatal("the HF cache entry of the trusted model now serves other bytes")
	}
}

// Variant found while fixing: a git-only file whose locked sha256 is already
// stored was exported without checking the source's git id against it.
func TestLockCannotRelabelAGitOnlyFile(t *testing.T) {
	trusted := modelFiles()
	evil := []fakeFile{{path: "config.json", data: []byte(`{"b":2}`)}} // same size as the trusted {"a":1}
	f := &fakeHF{files: trusted, byRepo: map[string][]fakeFile{"org/evil": evil}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	t.Setenv("HF_HUB_CACHE", t.TempDir())
	for _, trustedFirst := range []bool{true, false} {
		c := newTestClient(t, srv, t.TempDir())
		if trustedFirst {
			pullTo(t, c, "org/trusted", Target{Kind: "none"}) // index knows the genuine git id
		}
		pullTo(t, c, "org/evil", Target{Kind: "none"})
		l := NewLock(filepath.Join(t.TempDir(), DefaultLockName))
		l.Models = []LockModel{{Ref: "hf:org/trusted", Commit: commit, To: "none"}}
		for _, tf := range trusted {
			lf := LockFile{Path: tf.path, Size: int64(len(tf.data)), SHA256: sha(tf.data)}
			if tf.path == "config.json" {
				lf.SHA256, lf.Size = sha(evil[0].data), int64(len(evil[0].data))
			}
			l.Models[0].Files = append(l.Models[0].Files, lf)
		}
		req, _ := l.Request(&l.Models[0], false)
		if _, err := c.Pull(context.Background(), req, nil); AsError(err).Code != ExitVerify {
			t.Fatalf("trustedFirst=%v: got %v, want exit %d", trustedFirst, err, ExitVerify)
		}
	}
}

// Critic lead: a URL entry may only use its own id and a single file.
func TestLockURLEntriesCannotNameAnotherRecord(t *testing.T) {
	u := "https://example.org/m.gguf"
	base := "version = 1\n[[model]]\nref = \"" + u + "\"\ncommit = \"%s\"\nto = \"none\"\n" +
		"[[model.file]]\npath = \"m.gguf\"\nsize = 1\nsha256 = \"" + strings.Repeat("a", 64) + "\"\n"
	dir := t.TempDir()
	p := filepath.Join(dir, "lmget.lock")
	write := func(s string) { os.WriteFile(p, []byte(s), 0o644) }
	write(strings.Replace(base, "%s", urlID(u), 1))
	if _, err := ReadLock(p); err != nil {
		t.Fatalf("a correct URL entry was refused: %v", err)
	}
	write(strings.Replace(base, "%s", commit, 1)) // another record's id
	if _, err := ReadLock(p); err == nil {
		t.Error("a URL entry with a foreign commit was accepted")
	}
	write(strings.Replace(base, "%s", urlID(u), 1) + "[[model.file]]\npath = \"b.gguf\"\nsize = 1\nsha256 = \"" + strings.Repeat("b", 64) + "\"\n")
	if _, err := ReadLock(p); err == nil {
		t.Error("a URL entry with two files was accepted")
	}
	write(strings.Replace(strings.Replace(base, "%s", urlID(u), 1), "to = \"none\"", "to = \"none\"\nlicense = \"x\\u001b[2J\"", 1))
	if _, err := ReadLock(p); err == nil {
		t.Error("control characters in a lock license were accepted")
	}
}

// Critic lead: lmget.lock must not be read or written through a symlink, and
// the temporary file must not follow one placed in advance.
func TestLockfileIsNeverASymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	keep := "version = 1\n# keep me\n" // a valid lock: reading through the symlink would succeed
	os.WriteFile(victim, []byte(keep), 0o644)
	p := filepath.Join(dir, "lmget.lock")
	os.Symlink(victim, p)
	if _, err := ReadLock(p); err == nil {
		t.Error("a symlinked lmget.lock was read")
	}
	if err := NewLock(p).Write(); err == nil {
		t.Error("a symlinked lmget.lock was written")
	}
	os.Remove(p)
	// the old predictable temporary name, planted as a symlink
	os.Symlink(victim, p+".tmp"+strconv.Itoa(os.Getpid()))
	if err := writeFileAtomic(p, []byte("lock")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != keep {
		t.Fatal("writing the lock truncated a file through a planted symlink")
	}
}

// LOW: protocol 0 and 1 pickles have no PROTO header.
func TestOldProtocolPicklesAreRecognised(t *testing.T) {
	p0 := []byte("cposix\nsystem\np0\n(Vecho pwned\np1\ntp2\nRp3\n.")
	p1 := []byte("cposix\nsystem\nq\x00(X\x0a\x00\x00\x00echo pwnedq\x01tq\x02Rq\x03.")
	cases := []struct {
		name string
		data []byte
		ok   bool
	}{
		{"model.onnx", p0, false},
		{"model.bin", p1, false},                                 // a .bin next to a decoy .xml is not declared pickle
		{"vocab.txt", []byte("cat\ndog\nend."), true},            // a word list is text
		{"weights.onnx", []byte("cabc\x00\x01\x02 data."), true}, // starts with c, ends with ., no GLOBAL
		{"model.pt", p0, true},                                   // declared pickle by name
	}
	for _, tc := range cases {
		dir := t.TempDir()
		path := filepath.Join(dir, tc.name)
		os.WriteFile(path, tc.data, 0o644)
		siblings := map[string]bool{"model.xml": true} // the decoy OpenVINO pair
		err := checkFormat(path, tc.name, isPickle(tc.name, siblings))
		if tc.ok && err != nil {
			t.Errorf("%s: refused: %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: an old-protocol pickle passed", tc.name)
		}
	}
}

// Critic lead: names that reach the terminal cannot carry control sequences.
func TestControlCharactersNeverReachTheTerminal(t *testing.T) {
	for _, p := range []string{"a\x1b[2Jb.json", "x\x07.bin", "ok\u009b31m.txt"} {
		if isLocalSlash(p) {
			t.Errorf("%q accepted as a file path", p)
		}
	}
	if got := licenseString("mit\x1b]52;c;ZXZpbA==\x07"); strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("license kept control characters: %q", got)
	}
}

// Critic lead: a narrower re-pull of the same commit must keep, for gc, the
// files earlier exports still link.
func TestNarrowerRepullKeepsLinkedBlobs(t *testing.T) {
	other := []fakeFile{{path: "other.json", data: []byte(`{"o":1}`)}}
	f := &fakeHF{files: modelFiles(), byRepo: map[string][]fakeFile{"org/other": other}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	dir := t.TempDir()
	c := newTestClient(t, srv, t.TempDir())
	pullTo(t, c, "org/m", Target{Kind: "dir", Path: dir})
	if _, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Selection: Selection{Include: []string{"config.json"}}, Target: Target{Kind: "none"}}, nil); err != nil {
		t.Fatal(err)
	}
	pullTo(t, c, "org/other", Target{Kind: "none"})
	if _, err := c.Remove(hfRef("org/other")); err != nil {
		t.Fatal(err)
	}
	w := f.files[0] // the bytes the fake served (modelFiles() makes new random ones)
	if !c.store.Has(sha(w.data), int64(len(w.data))) {
		t.Fatal("gc deleted the weights still linked by the first export")
	}
}
