package lmget

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
	if got := l.targetString(Target{Kind: "dir", Path: filepath.Join(dir, "models", "q")}); got != "dir:./models/q" {
		t.Fatalf("got %q", got)
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
