package lmget

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

func pullTo(t *testing.T, c *Client, repo string, target Target) *Outcome {
	t.Helper()
	out, err := c.Pull(context.Background(), Request{Ref: Ref{"hf", repo, "main", "", ""}, Target: target}, nil)
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

	r, err := c.Remove(Ref{"hf", "org/a", "main", "", ""})
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
	r, err = c.Remove(Ref{"hf", "org/b", "main", "", ""})
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
	if _, err := c.Remove(Ref{"hf", "org/b", "main", "", ""}); AsError(err).Code != ExitNotFound {
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

	r, err := c.Remove(Ref{"hf", "org/m", "main", "", ""})
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

// Files of a pull that was interrupted belong to it: removing another model
// must not reclaim them.
func TestRemoveKeepsBlobsOfAnInterruptedPull(t *testing.T) {
	other := []fakeFile{{path: "other.json", data: []byte(`{"o":1}`)}}
	f := &fakeHF{files: modelFiles(), byRepo: map[string][]fakeFile{"org/other": other}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	stop := func(total int64) {
		if total > 1<<20 { // config.json is committed, model.safetensors is half way
			cancel()
		}
	}
	f.onByte.Store(&stop)
	if _, err := c.Pull(ctx, Request{Ref: hfRef("org/big"), Target: Target{Kind: "none"}}, nil); AsError(err).Code != ExitInterrupted {
		t.Fatalf("setup: %v", err)
	}
	f.onByte.Store(nil)
	cfg := modelFiles()[1].data
	if s, ok := c.store.LookupGit(gitID(cfg)); !ok || !c.store.Has(s, int64(len(cfg))) {
		t.Fatal("setup: config.json was not committed before the interruption")
	}
	pullTo(t, c, "org/other", Target{Kind: "none"})
	if _, err := c.Remove(hfRef("org/other")); err != nil {
		t.Fatal(err)
	}
	if s, ok := c.store.LookupGit(gitID(cfg)); !ok || !c.store.Has(s, int64(len(cfg))) {
		t.Fatal("rm of another model deleted a verified file of the interrupted pull")
	}
	if list, _ := c.List(); len(list) != 1 || list[0].Missing == 0 {
		t.Fatalf("the interrupted model should be listed as incomplete: %+v", list)
	}
}

func TestRemoveRefusesWhileAManifestIsUnreadable(t *testing.T) {
	other := []fakeFile{{path: "other.json", data: []byte(`{"o":1}`)}}
	f := &fakeHF{files: modelFiles(), byRepo: map[string][]fakeFile{"org/other": other}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	home := t.TempDir()
	c := newTestClient(t, srv, home)
	pullTo(t, c, "org/a", Target{Kind: "none"})
	pullTo(t, c, "org/other", Target{Kind: "none"})
	before := c.Stats().Blobs
	os.WriteFile(filepath.Join(home, "refs", "hf", "org", "a", commit+".json"), []byte("{trunc"), 0o644)
	if _, err := c.Remove(hfRef("org/other")); err == nil {
		t.Fatal("rm went ahead with an unreadable manifest")
	}
	if c.Stats().Blobs != before {
		t.Fatal("blobs were deleted although a manifest could not be read")
	}
	failed := false
	for _, ch := range c.Doctor(context.Background(), DoctorOptions{}) {
		if ch.Name == "manifests" && ch.Status == "fail" {
			failed = true
		}
	}
	if !failed {
		t.Fatal("doctor does not report the unreadable manifest")
	}
}

func TestRemoveKeepsTheManifestWhenALinkCannotBeRemoved(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	dir := t.TempDir()
	c := newTestClient(t, srv, t.TempDir())
	pullTo(t, c, "org/m", Target{Kind: "dir", Path: dir})
	os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)
	if _, err := c.Remove(hfRef("org/m")); AsError(err).Code != ExitDisk {
		t.Fatalf("got %v, want a disk error", err)
	}
	if list, _ := c.List(); len(list) != 1 {
		t.Fatal("the model was forgotten although its links are still on disk")
	}
	os.Chmod(dir, 0o755)
	if _, err := c.Remove(hfRef("org/m")); err != nil {
		t.Fatalf("second rm after fixing permissions: %v", err)
	}
}

func TestRemoveWaitsForNobodyButRefusesDuringAPull(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	pullTo(t, c, "org/m", Target{Kind: "none"})
	unlock, err := c.store.flock(".lock", false, true) // what a running pull holds
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Remove(hfRef("org/m"))
	unlock()
	if e := AsError(err); e == nil || e.Kind != "busy" {
		t.Fatalf("got %v, want busy", err)
	}
	if _, err := c.Remove(hfRef("org/m")); err != nil {
		t.Fatalf("after the pull: %v", err)
	}
}

// An export that fails half way: rm right after must remove what it linked.
func TestFailedExportLinksAreRemovedByRm(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "tokenizer.json"), []byte("mine"), 0o644)
	c := newTestClient(t, srv, t.TempDir())
	if _, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "dir", Path: dir}}, nil); err == nil {
		t.Fatal("setup: the conflicting file was overwritten")
	}
	if _, err := c.Remove(hfRef("org/m")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"config.json", "model.safetensors"} {
		if _, err := os.Lstat(filepath.Join(dir, p)); err == nil {
			t.Errorf("%s, linked before the failure, survived rm", p)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "sub", "tokenizer.json")); string(b) != "mine" {
		t.Fatal("rm touched the user's file")
	}
}

// Manifests written before exports were recorded (M1 slice 1) list no links:
// a new pull adopts the links that are already lmget's, so rm removes them.
func TestRepullAdoptsLinksOfAnOlderManifest(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	home, dir := t.TempDir(), t.TempDir()
	c := newTestClient(t, srv, home)
	out := pullTo(t, c, "org/m", Target{Kind: "dir", Path: dir})
	legacy, _ := json.Marshal(out.Plan) // the early format: a bare Plan, no exports
	os.WriteFile(filepath.Join(home, "refs", "hf", "org", "m", commit+".json"), legacy, 0o644)
	pullTo(t, c, "org/m", Target{Kind: "dir", Path: dir})
	if _, err := c.Remove(hfRef("org/m")); err != nil {
		t.Fatal(err)
	}
	for _, ff := range f.files {
		if _, err := os.Lstat(filepath.Join(dir, ff.path)); err == nil {
			t.Errorf("%s survived rm", ff.path)
		}
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
