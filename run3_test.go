package lmget

// Regression tests for sec-audit run 3 on d04e5b4: rm and shared blob
// directories, lmget.lock read-modify-write, and the hardening notes.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// the blob files an Ollama tag file names (config and layers)
func tagBlobs(t *testing.T, models, tag string) []string {
	t.Helper()
	b, err := os.ReadFile(tag)
	if err != nil {
		t.Fatal(err)
	}
	var m ollamaManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range append([]ollamaDescriptor{m.Config}, m.Layers...) {
		out = append(out, filepath.Join(models, "blobs", "sha256-"+strings.TrimPrefix(d.Digest, "sha256:")))
	}
	return out
}

func missing(paths []string) []string {
	var out []string
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			out = append(out, filepath.Base(p))
		}
	}
	return out
}

// LOW: rm of one Ollama tag must not delete layers another tag still uses.
func TestRemoveKeepsOllamaLayersAnotherTagUses(t *testing.T) {
	f := newFakeOllama()
	lic, tmpl := []byte("Apache License 2.0 text"), []byte("{{ .Prompt }}")
	f.publish("library/fam", "small", layer{"model", fakeGGUF(2000)}, layer{"license", lic}, layer{"template", tmpl})
	f.publish("library/fam", "big", layer{"model", fakeGGUF(3000)}, layer{"license", lic}, layer{"template", tmpl})
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	models := t.TempDir()
	t.Setenv("OLLAMA_MODELS", models)
	c := newOllamaClient(t, srv, t.TempDir())
	small, big := ollamaRef(t, "ollama:fam:small"), ollamaRef(t, "ollama:fam:big")
	for _, r := range []Ref{small, big} {
		if _, err := c.Pull(context.Background(), Request{Ref: r, Target: Target{Kind: "ollama"}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	tagDir := filepath.Join(models, "manifests", hostOf(srv.URL), "library", "fam")
	smallBlobs := tagBlobs(t, models, filepath.Join(tagDir, "small"))
	if _, err := c.Remove(small); err != nil {
		t.Fatal(err)
	}
	if m := missing(tagBlobs(t, models, filepath.Join(tagDir, "big"))); len(m) > 0 {
		t.Fatalf("rm of fam:small deleted blobs fam:big still uses: %v", m)
	}
	if _, err := os.Stat(filepath.Join(tagDir, "small")); err == nil {
		t.Fatal("the fam:small tag file is still there")
	}
	if m := missing(smallBlobs); len(m) != 1 { // only small's own model layer goes
		t.Fatalf("rm removed %v of fam:small's blobs, want only its model layer", m)
	}
	// removing the last user removes the rest, and the models directory stays
	if _, err := c.Remove(big); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(models); len(ents) != 0 {
		t.Fatalf("%d entries left after removing both tags", len(ents))
	}
}

// LOW: a blob lmget linked that Ollama's own model reuses stays in place.
func TestRemoveKeepsBlobsOllamaItselfStillUses(t *testing.T) {
	f := newFakeOllama()
	lic := []byte("MIT License text")
	f.publish("library/fam", "small", layer{"model", fakeGGUF(2000)}, layer{"license", lic})
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	models := t.TempDir()
	t.Setenv("OLLAMA_MODELS", models)
	c := newOllamaClient(t, srv, t.TempDir())
	small := ollamaRef(t, "ollama:fam:small")
	if _, err := c.Pull(context.Background(), Request{Ref: small, Target: Target{Kind: "ollama"}}, nil); err != nil {
		t.Fatal(err)
	}
	// Ollama pulls another model that reuses the license blob lmget linked
	own := fakeGGUF(1500)
	ownBlob := filepath.Join(models, "blobs", "sha256-"+sha(own))
	os.WriteFile(ownBlob, own, 0o644)
	licBlob := filepath.Join(models, "blobs", "sha256-"+sha(lic))
	m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": ollamaManifestType,
		"config": map[string]any{"mediaType": "application/vnd.docker.container.image.v1+json", "digest": "sha256:" + sha(own), "size": len(own)},
		"layers": []map[string]any{{"mediaType": "application/vnd.ollama.image.license", "digest": "sha256:" + sha(lic), "size": len(lic)}}})
	ollamaTag := filepath.Join(models, "manifests", "registry.ollama.ai", "library", "theirs", "latest")
	os.MkdirAll(filepath.Dir(ollamaTag), 0o755)
	os.WriteFile(ollamaTag, m, 0o644)
	if _, err := c.Remove(small); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(licBlob); err != nil {
		t.Fatal("rm deleted a blob Ollama's own model still names")
	}
	if b, err := os.ReadFile(ollamaTag); err != nil || !bytes.Equal(b, m) {
		t.Fatal("rm touched Ollama's own tag file")
	}
}

// LOW (HF variant): rm of one commit must not delete a blob another
// recorded commit's snapshot still links to.
func TestRemoveKeepsHFBlobsAnotherCommitUses(t *testing.T) {
	cA, cB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	cfg := []byte(`{"shared":1}`)
	files := map[string][]fakeFile{
		cA: {{path: "config.json", data: cfg}, {path: "model.safetensors", data: fakeSafetensors(4096), lfs: true}},
		cB: {{path: "config.json", data: cfg}, {path: "model.safetensors", data: fakeSafetensors(8192), lfs: true}},
	}
	revs := map[string]string{"v1": cA, "v2": cB, cA: cA, cB: cB}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case len(p) >= 6 && p[0] == "api" && p[4] == "revision":
			c, ok := revs[p[5]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": "org/x", "sha": c})
		case len(p) >= 6 && p[0] == "api" && p[4] == "tree":
			var es []map[string]any
			for _, ff := range files[p[5]] {
				e := map[string]any{"type": "file", "path": ff.path, "size": len(ff.data), "oid": gitID(ff.data)}
				if ff.lfs {
					e["lfs"] = map[string]any{"oid": sha(ff.data), "size": len(ff.data)}
				}
				es = append(es, e)
			}
			json.NewEncoder(w).Encode(es)
		case len(p) >= 5 && p[2] == "resolve":
			for _, ff := range files[p[3]] {
				if ff.path == strings.Join(p[4:], "/") {
					http.ServeContent(w, r, ff.path, time.Time{}, bytes.NewReader(ff.data))
					return
				}
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cache := t.TempDir()
	t.Setenv("HF_HUB_CACHE", cache)
	c := newTestClient(t, srv, t.TempDir())
	for _, rev := range []string{"v1", "v2"} {
		if _, err := c.Pull(context.Background(), Request{Ref: Ref{"hf", "org/x", rev, "", ""}, Target: Target{Kind: "hf"}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Remove(Ref{"hf", "org/x", cA[:8], "", ""}); err != nil {
		t.Fatal(err)
	}
	snapB := filepath.Join(cache, "models--org--x", "snapshots", cB)
	for _, name := range []string{"config.json", "model.safetensors"} {
		if _, err := os.ReadFile(filepath.Join(snapB, name)); err != nil {
			t.Fatalf("commit B's %s is broken after rm of commit A: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(cache, "models--org--x", "snapshots", cA)); err == nil {
		t.Fatal("commit A's snapshot is still there")
	}
	if _, err := c.Remove(Ref{"hf", "org/x", "main", "", ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, "models--org--x")); err == nil {
		t.Fatal("the repository directory is left after removing every commit")
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

// Hardening: repeated layer names never collide with a name already taken.
func TestOllamaLayerNamesStayUnique(t *testing.T) {
	c, err := New(Options{Home: t.TempDir(), OllamaRegistry: "https://registry.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	d := func(typ, hexc string) map[string]any {
		return map[string]any{"mediaType": "application/vnd.ollama.image." + typ, "digest": "sha256:" + strings.Repeat(hexc, 64), "size": 1}
	}
	b, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": ollamaManifestType,
		"config": map[string]any{"mediaType": "x", "digest": "sha256:" + strings.Repeat("a", 64), "size": 1},
		"layers": []any{d("model", "b"), d("license-2", "c"), d("license", "d"), d("license", "e")}})
	p, err := c.ollamaPlan(ollamaRef(t, "ollama:m"), b, sha(b), Selection{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range p.Files {
		if seen[f.Path] {
			t.Fatalf("two plan files named %s", f.Path)
		}
		seen[f.Path] = true
	}
}

// Hardening: a lock that lists only some layers cannot make lmget write a
// tag file whose layers are not linked.
func TestOllamaExportRefusesALockWithoutEveryLayer(t *testing.T) {
	f := newFakeOllama()
	ls := smolLayers()
	f.publish("library/smollm2", "135m", ls...)
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	models := t.TempDir()
	t.Setenv("OLLAMA_MODELS", models)
	c := newOllamaClient(t, srv, t.TempDir())
	ref := ollamaRef(t, "ollama:smollm2:135m")
	out, err := c.Pull(context.Background(), Request{Ref: ref, Target: Target{Kind: "none"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLock(filepath.Join(t.TempDir(), DefaultLockName))
	if err := l.Upsert(out.Plan, Selection{}, Target{Kind: "ollama"}); err != nil {
		t.Fatal(err)
	}
	var kept []LockFile
	for _, lf := range l.Models[0].Files {
		if lf.Path != "template" {
			kept = append(kept, lf)
		}
	}
	l.Models[0].Files = kept
	req, err := l.Request(&l.Models[0], false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pull(context.Background(), req, nil); AsError(err).Code != ExitUsage {
		t.Fatalf("got %v, want the partial export refused", err)
	}
	if _, err := os.Stat(filepath.Join(models, "manifests")); err == nil {
		t.Fatal("a tag file was written for a model with an unlinked layer")
	}
}

// Hardening: an rm between the plan and the store lock does not turn into a
// pull that reports success while the store lacks the files.
func TestPullFetchesWhatAnRmRemovedAfterThePlan(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	home := t.TempDir()
	c := newTestClient(t, srv, home)
	pullTo(t, c, "org/m", Target{Kind: "none"})
	other := newTestClient(t, srv, home)
	emit := func(e Event) {
		if e.Type == EvPlan {
			if _, err := other.Remove(hfRef("org/m")); err != nil {
				t.Error(err)
			}
		}
	}
	out, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, emit)
	if err != nil {
		t.Fatal(err)
	}
	for _, fl := range out.Plan.Selected() {
		if !c.store.Has(fl.SHA256, fl.Size) {
			t.Fatalf("%s is missing after a successful pull", fl.Path)
		}
	}
	if out.Fetched == 0 {
		t.Fatal("nothing was fetched again")
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

// LOW (dir variant): two models exported to one directory share a file with
// identical bytes; removing one keeps the file the other still records.
func TestRemoveKeepsAFileAnotherModelExportedToTheSameDir(t *testing.T) {
	shared := []byte(`{"tokenizer":"same"}`)
	f := &fakeHF{byRepo: map[string][]fakeFile{
		"org/a": {{path: "tokenizer.json", data: shared}, {path: "a.safetensors", data: fakeSafetensors(2048), lfs: true}},
		"org/b": {{path: "tokenizer.json", data: shared}, {path: "b.safetensors", data: fakeSafetensors(4096), lfs: true}},
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	dir := t.TempDir()
	pullTo(t, c, "org/a", Target{Kind: "dir", Path: dir})
	pullTo(t, c, "org/b", Target{Kind: "dir", Path: dir})
	if _, err := c.Remove(hfRef("org/a")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "tokenizer.json")); err != nil || !bytes.Equal(b, shared) {
		t.Fatal("rm of org/a deleted the tokenizer org/b exported to the same directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "a.safetensors")); err == nil {
		t.Fatal("org/a's own file is still there")
	}
}

// LOW (HF variant): a snapshot huggingface_hub wrote, which lmget never
// recorded, keeps the blob it links to.
func TestRemoveKeepsHFBlobsASnapshotOfHuggingfaceHubUses(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cache := t.TempDir()
	t.Setenv("HF_HUB_CACHE", cache)
	c := newTestClient(t, srv, t.TempDir())
	pullTo(t, c, "org/m", Target{Kind: "hf"})
	repo := filepath.Join(cache, "models--org--m")
	theirs := filepath.Join(repo, "snapshots", strings.Repeat("e", 40))
	os.MkdirAll(theirs, 0o755)
	var blob string
	ents, _ := os.ReadDir(filepath.Join(repo, "blobs"))
	blob = ents[0].Name()
	os.Symlink(filepath.Join("..", "..", "blobs", blob), filepath.Join(theirs, "config.json"))
	if _, err := c.Remove(hfRef("org/m")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(filepath.Join(theirs, "config.json")); err != nil {
		t.Fatalf("rm deleted a blob huggingface_hub's own snapshot links to: %v", err)
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
