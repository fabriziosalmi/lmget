package lmget

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The models directory of each app: the variable wins, then oMLX's own
// settings, then the app's default; relative paths are refused.
func TestAppModelsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LMGET_OMLX_DIR", "")
	t.Setenv("LMGET_LMSTUDIO_DIR", "")
	check := func(app, want string) {
		t.Helper()
		got, err := AppModelsDir(app)
		if err != nil {
			t.Fatalf("%s: %v", app, err)
		}
		if got != want {
			t.Fatalf("%s: got %s, want %s", app, got, want)
		}
	}
	refused := func(app, why string) {
		t.Helper()
		if _, err := AppModelsDir(app); err == nil {
			t.Fatalf("%s: %s was accepted", app, why)
		}
	}
	check("omlx", filepath.Join(home, ".omlx", "models")) // no settings file
	check("lmstudio", filepath.Join(home, ".lmstudio", "models"))

	settings := filepath.Join(home, ".omlx", "settings.json")
	os.MkdirAll(filepath.Dir(settings), 0o755)
	write := func(body string) {
		if err := os.WriteFile(settings, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"server":{"port":1234},"auth":{"api_key":"not-read"},"model":{"model_dir":"/srv/models","model_dirs":["/srv/models","/x"]}}`)
	check("omlx", "/srv/models")
	write(`{"model":{"model_dir":"~/m"}}`)
	check("omlx", filepath.Join(home, "m"))
	write(`{"server":{}}`) // no key: oMLX's default
	check("omlx", filepath.Join(home, ".omlx", "models"))
	write(`{"model":{"model_dir":"rel/models"}}`)
	refused("omlx", "a relative model_dir")
	write(`{not json`)
	refused("omlx", "unreadable settings")

	t.Setenv("LMGET_OMLX_DIR", "/opt/omlx") // the variable wins, even over broken settings
	check("omlx", "/opt/omlx")
	t.Setenv("LMGET_LMSTUDIO_DIR", "/opt/lms")
	check("lmstudio", "/opt/lms")
	t.Setenv("LMGET_LMSTUDIO_DIR", "lms")
	refused("lmstudio", "a relative variable")

	if _, err := ParseTarget("omlx"); err != nil {
		t.Fatal(err)
	}
	if err := lockTargetSyntax("omlx"); err != nil {
		t.Fatal(err)
	}
	if err := lockTargetSyntax("lmstudio"); err != nil {
		t.Fatal(err)
	}
}

func appTarget(t *testing.T, kind string) Target {
	t.Helper()
	return Target{Kind: kind, Path: filepath.Join(t.TempDir(), "models")}
}

// A model lands as <dir>/<org>/<name>, and a lock records only the kind, so
// a cloned lock resolves the directory of the machine it runs on.
func TestAppExportLayoutAndLock(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	for _, kind := range []string{"omlx", "lmstudio"} {
		tg := appTarget(t, kind)
		out := pullTo(t, c, "org/m", tg)
		dir := filepath.Join(tg.Path, "org", "m")
		for _, p := range []string{"config.json", "model.safetensors", "sub/tokenizer.json"} {
			if !hasContent(filepath.Join(dir, p), int64(len(fileData(t, f, p))), sha(fileData(t, f, p))) {
				t.Fatalf("%s: %s missing or wrong", kind, p)
			}
		}
		if out.Path != dir {
			t.Fatalf("%s: reported %s, want %s", kind, out.Path, dir)
		}
		s, err := NewLock(filepath.Join(t.TempDir(), "lmget.lock")).targetString(tg)
		if err != nil || s != kind {
			t.Fatalf("%s: lock records %q (%v)", kind, s, err)
		}
	}
}

// oMLX only sees a directory with config.json and *.safetensors: a selection
// without them is refused before anything is written there.
func TestOMLXRefusesAModelItCannotLoad(t *testing.T) {
	f := &fakeHF{files: []fakeFile{{path: "m-Q4_K_M.gguf", data: fakeGGUF(5000), lfs: true}, {path: "README.md", data: []byte("x")}}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	out := pullTo(t, c, "org/g", Target{Kind: "none"})
	tg := appTarget(t, "omlx")
	if _, err := c.Export(out.Plan, tg); err == nil || !strings.Contains(err.Error(), "safetensors") {
		t.Fatalf("a GGUF-only model was exported to oMLX: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tg.Path, "org", "g")); !os.IsNotExist(err) {
		t.Fatal("the refused export left a directory behind")
	}
	if _, err := c.Export(out.Plan, appTarget(t, "lmstudio")); err != nil { // LM Studio loads GGUF
		t.Fatal(err)
	}
}

// Updating a model in an app directory replaces lmget's own links and drops
// the ones the new revision no longer has, so the app never loads a mix;
// files lmget did not create are left alone, and one in the way stops it.
func TestAppExportUpdatesOnlyItsOwnLinks(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	tg := appTarget(t, "omlx")
	pullTo(t, c, "org/m", tg)
	dir := filepath.Join(tg.Path, "org", "m")
	notes := filepath.Join(dir, "notes.txt") // the user's own file
	if err := os.WriteFile(notes, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	// the next revision: new weights, no tokenizer
	f.files = []fakeFile{
		{path: "model.safetensors", data: fakeSafetensors(2<<20 + 7), lfs: true},
		{path: "config.json", data: []byte(`{"a":2}`)},
	}
	pullTo(t, c, "org/m", tg)
	for _, p := range []string{"model.safetensors", "config.json"} {
		if !hasContent(filepath.Join(dir, p), int64(len(fileData(t, f, p))), sha(fileData(t, f, p))) {
			t.Fatalf("%s was not updated", p)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "sub", "tokenizer.json")); !os.IsNotExist(err) {
		t.Fatal("the old revision's tokenizer.json is still there")
	}
	if _, err := os.Stat(filepath.Join(dir, "sub")); !os.IsNotExist(err) {
		t.Fatal("the emptied sub/ directory is still there")
	}
	if b, err := os.ReadFile(notes); err != nil || string(b) != "mine" {
		t.Fatal("the user's own file was touched")
	}

	// a file lmget did not create, in the way of the next revision
	os.Remove(filepath.Join(dir, "config.json"))
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"user":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f.files[1].data = []byte(`{"a":3}`)
	if _, err := c.Pull(t.Context(), Request{Ref: hfRef("org/m"), Target: tg}, nil); err == nil {
		t.Fatal("a file lmget did not create was overwritten")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "config.json")); string(b) != `{"user":true}` {
		t.Fatal("the user's config.json was changed")
	}
}

// lmget rm removes what it linked into an app directory, and nothing else.
func TestRemoveUnlinksAnAppExport(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	tg := appTarget(t, "lmstudio")
	pullTo(t, c, "org/m", tg)
	dir := filepath.Join(tg.Path, "org", "m")
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o644)
	if _, err := c.Remove(hfRef("org/m")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"config.json", "model.safetensors", "sub/tokenizer.json"} {
		if _, err := os.Lstat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Fatalf("%s survived lmget rm", p)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal("lmget rm removed the user's file")
	}
}

func fileData(t *testing.T, f *fakeHF, p string) []byte {
	t.Helper()
	for _, x := range f.files {
		if x.path == p {
			return x.data
		}
	}
	t.Fatalf("no fake file %s", p)
	return nil
}
