package lmget

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Audit run 4: the builder signs only identities the source publishes. A
// git id on an LFS file (the source names it by sha256 only) would name the
// blob in the HF cache, so another file's git id there is refused.
func TestBuildIndexRefusesAGitIDTheSourceDidNotPublish(t *testing.T) {
	m := setupM3(t)
	lm := m.lock.Models[0]
	lm.Files = append([]LockFile(nil), lm.Files...)
	var other string
	for _, f := range lm.Files {
		if f.Path == "config.json" {
			other = f.GitSHA1
		}
	}
	if other == "" {
		t.Fatal("config.json has no git id in the lock")
	}
	for i := range lm.Files {
		if lm.Files[i].Path == "model.safetensors" {
			if lm.Files[i].GitSHA1 != "" {
				t.Fatal("the LFS file already has a git id: the test proves nothing")
			}
			lm.Files[i].GitSHA1 = other
		}
	}
	l := &Lock{Version: m.lock.Version, Models: []LockModel{lm}}
	_, err := m.client(t, t.TempDir()).BuildIndex(context.Background(), l, BuildOptions{Sequence: 2, Valid: time.Hour})
	if err == nil || !strings.Contains(err.Error(), "model.safetensors differs between the lock and the source") {
		t.Fatalf("BuildIndex = %v; want a refusal for model.safetensors", err)
	}
}

// Audit run 4: an index entry is a whole model. A lock that lists only some
// of the files the source publishes is refused, so a client falling back to
// the index never takes a selection for the model.
func TestBuildIndexRefusesALockThatOmitsFiles(t *testing.T) {
	m := setupM3(t)
	lm := m.lock.Models[0]
	var kept []LockFile
	for _, f := range lm.Files {
		if f.Path != "sub/tokenizer.json" {
			kept = append(kept, f)
		}
	}
	lm.Files = kept
	l := &Lock{Version: m.lock.Version, Models: []LockModel{lm}}
	_, err := m.client(t, t.TempDir()).BuildIndex(context.Background(), l, BuildOptions{Sequence: 2, Valid: time.Hour})
	if err == nil || !strings.Contains(err.Error(), "the index mirrors whole models") {
		t.Fatalf("BuildIndex = %v; want a refusal of the partial lock", err)
	}
}

// the builder trusts the source, not the lock: license, gating and files
func TestBuildIndexChecksTheSource(t *testing.T) {
	m := setupM3(t)
	b := m.client(t, t.TempDir())
	rev := func(body string) {
		inner := (&fakeHF{files: m.files}).handler()
		m.hf.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/revision/") {
				w.Write([]byte(body))
				return
			}
			inner.ServeHTTP(w, r)
		})
	}
	build := func() error {
		_, err := b.BuildIndex(context.Background(), m.lock, BuildOptions{Sequence: 2, Valid: time.Hour, Nodes: []string{m.node.URL}})
		return err
	}
	rev(`{"sha":"` + commit + `","gated":false,"cardData":{"license":"llama3"}}`)
	if err := build(); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("a non-allowlisted license: %v", err)
	}
	rev(`{"sha":"` + commit + `","gated":"auto","cardData":{"license":"apache-2.0"}}`)
	if err := build(); err == nil || !strings.Contains(err.Error(), "gated") {
		t.Errorf("a gated model: %v", err)
	}
	m.hf.h = (&fakeHF{files: m.files}).handler()
	saved := m.lock.Models[0].License
	m.lock.Models[0].License = "mit" // the source says apache-2.0, both allowlisted
	if err := build(); err == nil || !strings.Contains(err.Error(), "license changed") {
		t.Errorf("a license that changed since the lock: %v", err)
	}
	m.lock.Models[0].License = saved
	for i, name := range []string{"config.json (git id only at the source)", "model.safetensors (sha256 at the source)"} {
		saved := m.lock.Models[0].Files[i].SHA256
		m.lock.Models[0].Files[i].SHA256 = strings.Repeat("e", 64)
		if err := build(); err == nil {
			t.Errorf("a lock with a wrong sha256 for %s was indexed", name)
		}
		m.lock.Models[0].Files[i].SHA256 = saved
	}
	if err := build(); err != nil {
		t.Fatalf("the correct lock was refused: %v", err)
	}
}

func TestLicenseAllowlist(t *testing.T) {
	for l, want := range map[string]bool{"apache-2.0": true, "MIT": true, "apache-2.0, mit": true, "apache-2.0, llama3": false, "": false, "other": false, "cc-by-nc-4.0": false} {
		if LicenseAllowed(l) != want {
			t.Errorf("LicenseAllowed(%q) != %v", l, want)
		}
	}
}

// the index carries the v2 infohash of every blob, as the node will seed it
func TestBuiltIndexCarriesTorrentIdentities(t *testing.T) {
	m := setupM3(t)
	ix, err := m.nodeCli.Index()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range ix.Models[0].Files {
		bt, err := NewBlobTorrent(m.nodeCli.store.BlobPath(f.SHA256), f.SHA256)
		if err != nil || f.BTIH2 != bt.Infohash {
			t.Fatalf("%s: index btih2 %q, blob %v %v", f.Path, f.BTIH2, bt, err)
		}
	}
}
