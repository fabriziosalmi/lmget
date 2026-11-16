package lmget

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pullAndLock pulls org/m from a fake source and records it in a new lock.
func pullAndLock(t *testing.T, f *fakeHF, home string, target Target) (*Client, *Lock, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	c := newTestClient(t, srv, home)
	sel := Selection{}
	out, err := c.Pull(context.Background(), Request{Ref: Ref{"hf", "org/m", "main", "", ""}, Selection: sel, Target: target}, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLock(filepath.Join(t.TempDir(), DefaultLockName))
	if err := l.Upsert(out.Plan, sel, target); err != nil {
		t.Fatal(err)
	}
	if err := l.Write(); err != nil {
		t.Fatal(err)
	}
	return c, l, srv
}

func TestLockRoundTrip(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", t.TempDir())
	f := &fakeHF{files: modelFiles()}
	_, l, srv := pullAndLock(t, f, t.TempDir(), Target{Kind: "hf"})
	defer srv.Close()
	got, err := ReadLock(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Models, l.Models) {
		t.Fatalf("round trip changed the lock:\n%+v\n%+v", got.Models, l.Models)
	}
	m := got.Models[0]
	if m.Ref != "hf:org/m" || m.Commit != commit || len(m.Files) != 3 {
		t.Fatalf("unexpected entry %+v", m)
	}
	for _, lf := range m.Files { // every file pinned by sha256, git-only ones too
		if !hex64.MatchString(lf.SHA256) {
			t.Fatalf("%s has no sha256 in the lock", lf.Path)
		}
	}
	b, _ := os.ReadFile(l.Path())
	if !strings.HasPrefix(string(b), "# lmget.lock") || !strings.Contains(string(b), "[[model.file]]") {
		t.Fatalf("unexpected lock text:\n%s", b)
	}
}

func TestLockStoresDirTargetRelative(t *testing.T) {
	dir := t.TempDir()
	l := NewLock(filepath.Join(dir, DefaultLockName))
	if got, err := l.targetString(Target{Kind: "dir", Path: filepath.Join(dir, "models", "q")}); err != nil || got != "dir:./models/q" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := l.targetString(Target{Kind: "dir", Path: t.TempDir()}); err == nil {
		t.Fatal("a directory outside the project was accepted for the lock")
	}
	l.Models = []LockModel{{Ref: "hf:org/m", To: "dir:./models/q"}}
	r, err := l.Request(&l.Models[0], false)
	if err != nil || r.Target.Path != filepath.Join(dir, "models", "q") {
		t.Fatalf("request target %+v, %v", r.Target, err)
	}
}

// With every blob in the store, sync must not need the network at all.
func TestSyncIsOfflineWhenEverythingIsStored(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", t.TempDir())
	f := &fakeHF{files: modelFiles()}
	c, l, srv := pullAndLock(t, f, t.TempDir(), Target{Kind: "hf"})
	srv.Close() // any request now fails
	t.Setenv("HF_HUB_CACHE", t.TempDir())
	req, err := l.Request(&l.Models[0], false)
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Pull(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("offline sync failed: %v", err)
	}
	if out.Fetched != 0 || out.Plan.Commit != commit {
		t.Fatalf("fetched %d, commit %s", out.Fetched, out.Plan.Commit)
	}
	for _, ff := range f.files {
		if _, err := os.Stat(filepath.Join(out.Path, ff.path)); err != nil {
			t.Fatalf("%s not exported: %v", ff.path, err)
		}
	}
}

// The source publishes a different sha256 than the lock: stop before downloading.
func TestSyncRefusesSourceDrift(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", t.TempDir())
	f := &fakeHF{files: modelFiles()}
	_, l, srv := pullAndLock(t, f, t.TempDir(), Target{Kind: "none"})
	defer srv.Close()
	for i, lf := range l.Models[0].Files {
		if lf.Path == "model.safetensors" {
			l.Models[0].Files[i].SHA256 = strings.Repeat("0", 64)
		}
	}
	c := newTestClient(t, srv, t.TempDir()) // empty store: must go to the source
	req, _ := l.Request(&l.Models[0], false)
	before := f.served.Load()
	_, err := c.Pull(context.Background(), req, nil)
	if e := AsError(err); e.Code != ExitVerify || e.Kind != "lock" {
		t.Fatalf("got %v, want a lock drift error with exit %d", err, ExitVerify)
	}
	if f.served.Load() != before {
		t.Fatal("bytes were downloaded before the drift was reported")
	}
}

// A git-only file whose git id matches but whose sha256 differs from the lock
// must be rejected: the lock's sha256 is checked too.
func TestSyncChecksLockedSHA256OfGitFiles(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", t.TempDir())
	f := &fakeHF{files: modelFiles()}
	_, l, srv := pullAndLock(t, f, t.TempDir(), Target{Kind: "none"})
	defer srv.Close()
	for i, lf := range l.Models[0].Files {
		if lf.Path == "config.json" {
			l.Models[0].Files[i].SHA256 = strings.Repeat("1", 64)
		}
	}
	c := newTestClient(t, srv, t.TempDir())
	req, _ := l.Request(&l.Models[0], false)
	_, err := c.Pull(context.Background(), req, nil)
	if e := AsError(err); e.Code != ExitVerify {
		t.Fatalf("got %v, want exit %d", err, ExitVerify)
	}
}

func TestReadLockRejectsHostileInput(t *testing.T) {
	good := `version = 1
[[model]]
ref = "hf:org/m"
commit = "` + commit + `"
to = "hf"
[[model.file]]
path = "a.json"
size = 1
sha256 = "` + strings.Repeat("a", 64) + `"
`
	cases := map[string]string{
		"version 2":       strings.Replace(good, "version = 1", "version = 2", 1),
		"path traversal":  strings.Replace(good, `"a.json"`, `"../../etc/passwd"`, 1),
		"absolute path":   strings.Replace(good, `"a.json"`, `"/etc/passwd"`, 1),
		"bad sha256":      strings.Replace(good, strings.Repeat("a", 64), "zz", 1),
		"bad commit":      strings.Replace(good, commit, "main", 1),
		"bad ref":         strings.Replace(good, `"hf:org/m"`, `"hf:../x"`, 1),
		"bad target":      strings.Replace(good, `to = "hf"`, `to = "s3:bucket"`, 1),
		"target escapes":  strings.Replace(good, `to = "hf"`, `to = "dir:../../.ssh"`, 1),
		"target absolute": strings.Replace(good, `to = "hf"`, `to = "dir:/etc"`, 1),
		"target sneaky":   strings.Replace(good, `to = "hf"`, `to = "dir:./a/../../b"`, 1),
		"unknown key":     strings.Replace(good, `to = "hf"`, "to = \"hf\"\nscript = \"rm -rf\"", 1),
		"duplicate model": good + strings.TrimPrefix(good, "version = 1\n"),
		"negative size":   strings.Replace(good, "size = 1", "size = -1", 1),
		"not toml":        "[[[",
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "lmget.lock")
	os.WriteFile(p, []byte(good), 0o644)
	if _, err := ReadLock(p); err != nil {
		t.Fatalf("good lock rejected: %v", err)
	}
	for name, text := range cases {
		os.WriteFile(p, []byte(text), 0o644)
		_, err := ReadLock(p)
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if AsError(err).Code != ExitUsage {
			t.Errorf("%s: exit %d, want %d", name, AsError(err).Code, ExitUsage)
		}
	}
}

// A lock whose target directory is a symlink leading out of the project is
// refused before anything is downloaded or written.
func TestLockTargetCannotEscapeThroughSymlink(t *testing.T) {
	proj, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(proj, "models")); err != nil {
		t.Fatal(err)
	}
	l := NewLock(filepath.Join(proj, DefaultLockName))
	l.Models = []LockModel{{Ref: "hf:org/m", Commit: commit, To: "dir:./models/q"}}
	if _, err := l.Request(&l.Models[0], false); err == nil {
		t.Fatal("a target reaching outside through a symlink was accepted")
	}
	l.Models[0].To = "dir:./real/q"
	if _, err := l.Request(&l.Models[0], false); err != nil {
		t.Fatalf("a normal target was refused: %v", err)
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

func lockPlan(repo, commit string) *Plan {
	return &Plan{Ref: Ref{Source: "hf", Repo: repo, Revision: "main"}, Commit: commit,
		Files: []File{{Path: "model.safetensors", Size: 1, SHA256: strings.Repeat("c", 64)}}}
}

// INFORMATIONAL: concurrent lmget runs updating one lmget.lock keep every
// update. Each writer holds its read open long enough for all to overlap.
func TestConcurrentLockUpdatesAreAllKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultLockName)
	const n = 6
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			_, err := UpdateLock(path, true, func(l *Lock) (bool, error) {
				time.Sleep(30 * time.Millisecond) // read done, write pending
				return true, l.Upsert(lockPlan("org/m"+string(rune('a'+i)), strings.Repeat("1", 40)), Selection{}, Target{Kind: "none"})
			})
			errs <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	l, err := ReadLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Models) != n {
		t.Fatalf("the lock holds %d models after %d concurrent updates", len(l.Models), n)
	}
}

// INFORMATIONAL: a pinned sync never writes its entry over a newer one that
// another lmget wrote while it downloaded, whatever the spelling of the old one.
func TestSyncKeepsAnEntryChangedMeanwhile(t *testing.T) {
	old, newer := strings.Repeat("1", 40), strings.Repeat("2", 40)
	entry := func(ref, commit string) string {
		return "version = 1\n[[model]]\nref = \"" + ref + "\"\ncommit = \"" + commit + "\"\nto = \"none\"\n" +
			"[[model.file]]\npath = \"model.safetensors\"\nsize = 1\nsha256 = \"" + strings.Repeat("c", 64) + "\"\n"
	}
	for _, ref := range []string{"hf:org/m", "org/m"} { // canonical, and as a person might write it
		path := filepath.Join(t.TempDir(), DefaultLockName)
		os.WriteFile(path, []byte(entry(ref, old)), 0o644)
		l, err := ReadLock(path)
		if err != nil {
			t.Fatal(err)
		}
		before := l.Models[0]
		os.WriteFile(path, []byte(entry(ref, newer)), 0o644) // sync --update elsewhere, meanwhile
		wrote, err := UpdateLockEntry(path, before, lockPlan("org/m", old), Selection{}, Target{Kind: "none"})
		if err != nil {
			t.Fatal(err)
		}
		l2, _ := ReadLock(path)
		if wrote || l2.Models[0].Commit != newer {
			t.Fatalf("ref %q: the sync wrote over a newer entry (wrote=%v, commit %s)", ref, wrote, l2.Models[0].Commit[:7])
		}
	}
	// unchanged meanwhile: a hand-written entry is rewritten in canonical form
	path := filepath.Join(t.TempDir(), DefaultLockName)
	os.WriteFile(path, []byte(entry("org/m", old)), 0o644)
	l, _ := ReadLock(path)
	wrote, err := UpdateLockEntry(path, l.Models[0], lockPlan("org/m", old), Selection{}, Target{Kind: "none"})
	if err != nil || !wrote {
		t.Fatalf("an unchanged entry was not recorded: wrote=%v err=%v", wrote, err)
	}
	if l2, _ := ReadLock(path); l2.Models[0].Ref != "hf:org/m" {
		t.Fatalf("ref %q, want the canonical form", l2.Models[0].Ref)
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
