package lmget

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func ggufRepo() []fakeFile {
	return []fakeFile{
		{path: "m-Q4_K_M.gguf", data: fakeGGUF(6000), lfs: true},
		{path: "mmproj-m-f16.gguf", data: fakeGGUF(3000), lfs: true},
		{path: "README.md", data: []byte("# m")},
	}
}

func TestJanTarget(t *testing.T) {
	f := &fakeHF{files: ggufRepo()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	tg := Target{Kind: "jan", Path: t.TempDir()} // Jan's data folder
	out := pullTo(t, c, "org/m", tg)
	dir := filepath.Join(tg.Path, "llamacpp", "models", "org", "m")
	if out.Path != dir {
		t.Fatalf("exported to %s, want %s", out.Path, dir)
	}
	w := f.files[0]
	if !hasContent(filepath.Join(dir, w.path), int64(len(w.data)), sha(w.data)) {
		t.Fatal("the GGUF is not in Jan's model folder")
	}
	yml, err := os.ReadFile(filepath.Join(dir, "model.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		janMarker,
		`model_path: "llamacpp/models/org/m/m-Q4_K_M.gguf"`, // relative to the data folder, as Jan writes it
		`name: "m"`,
		fmt.Sprintf("size_bytes: %d\n", len(w.data)+len(f.files[1].data)), // the projector counted, as Jan counts it
		"model_sha256: " + sha(w.data),
		fmt.Sprintf("model_size_bytes: %d\n", len(w.data)),
		fmt.Sprintf("mmproj_size_bytes: %d\n", len(f.files[1].data)),
		`mmproj_path: "llamacpp/models/org/m/mmproj-m-f16.gguf"`,
	} {
		if !strings.Contains(string(yml), want) {
			t.Fatalf("model.yml lacks %q:\n%s", want, yml)
		}
	}
	// Jan rewrites model.yml with its own keys and no comments: the file stays lmget's
	jan := "embedding: false\nembedding_check_v: 2\n"
	for _, l := range strings.Split(string(yml), "\n") {
		if !strings.HasPrefix(l, "#") {
			jan += l + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "model.yml"), []byte(jan), 0o644); err != nil {
		t.Fatal(err)
	}
	pullTo(t, c, "org/m", tg) // an update goes through
	os.WriteFile(filepath.Join(dir, "model.yml"), []byte(jan), 0o644)
	if _, err := c.Remove(hfRef("org/m")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"model.yml", w.path} {
		if _, err := os.Lstat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Fatalf("rm left %s", p)
		}
	}

	// a model.yml Jan wrote itself is not overwritten
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "model.yml"), []byte("model_path: llamacpp/models/org/m/x.gguf\n"), 0o644)
	if _, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: tg}, nil); err == nil {
		t.Fatal("Jan's own model.yml was overwritten")
	}
}

func TestJanNeedsOneGGUF(t *testing.T) {
	two := append(ggufRepo(), fakeFile{path: "m-Q8_0.gguf", data: fakeGGUF(7000), lfs: true})
	for name, files := range map[string][]fakeFile{"two quantizations": two, "no GGUF": modelFiles()} {
		f := &fakeHF{files: files}
		srv := httptest.NewServer(f.handler())
		c := newTestClient(t, srv, t.TempDir())
		_, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "jan", Path: t.TempDir()}}, nil)
		srv.Close()
		if AsError(err).Code != ExitUsage {
			t.Errorf("%s: %v", name, err)
		}
		for _, x := range files {
			if c.store.Has(sha(x.data), int64(len(x.data))) {
				t.Errorf("%s: refused after downloading %s", name, x.path)
			}
		}
	}
}

func TestLocalAITarget(t *testing.T) {
	f := &fakeHF{files: ggufRepo()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	tg := Target{Kind: "localai", Path: t.TempDir()}
	pullTo(t, c, "org/m", tg)
	for _, x := range f.files[:2] { // the GGUFs, flat: the file name is LocalAI's model name
		if !hasContent(filepath.Join(tg.Path, x.path), int64(len(x.data)), sha(x.data)) {
			t.Fatalf("%s is not in LocalAI's models path", x.path)
		}
	}
	if _, err := os.Stat(filepath.Join(tg.Path, "README.md")); !os.IsNotExist(err) {
		t.Fatal("a non-model file landed in LocalAI's models path")
	}
	// another model's file under that name is not overwritten
	os.Remove(filepath.Join(tg.Path, "m-Q4_K_M.gguf"))
	os.WriteFile(filepath.Join(tg.Path, "m-Q4_K_M.gguf"), []byte("someone else's"), 0o644)
	if _, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: tg}, nil); err == nil {
		t.Fatal("a file lmget did not create was overwritten")
	}
	os.Remove(filepath.Join(tg.Path, "m-Q4_K_M.gguf"))
	pullTo(t, c, "org/m", tg)
	if _, err := c.Remove(hfRef("org/m")); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(tg.Path); len(entries) != 0 {
		t.Fatalf("rm left %d files", len(entries))
	}
	// LocalAI's llama.cpp backend loads GGUF: safetensors are refused before downloading
	s := &fakeHF{files: modelFiles()}
	srv2 := httptest.NewServer(s.handler())
	defer srv2.Close()
	if _, err := newTestClient(t, srv2, t.TempDir()).Pull(context.Background(), Request{Ref: hfRef("org/s"), Target: Target{Kind: "localai", Path: t.TempDir()}}, nil); AsError(err).Code != ExitUsage {
		t.Fatalf("safetensors to LocalAI: %v", err)
	}
}

func TestExoTarget(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	tg := Target{Kind: "exo", Path: t.TempDir()}
	pullTo(t, c, "mlx-community/m", tg)
	dir := filepath.Join(tg.Path, "mlx-community--m")
	b, err := os.ReadFile(filepath.Join(dir, ".exo-download.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["repo"] != "mlx-community/m" || m["commit"] != commit || m["complete"] != true {
		t.Fatalf("manifest %s", b)
	}
	files := m["files"].([]any)
	if len(files) != len(f.files) {
		t.Fatalf("%d files in the manifest, want %d", len(files), len(f.files))
	}
	for _, x := range files {
		e := x.(map[string]any)
		if _, camel := e["gitSha1"]; !camel {
			t.Fatalf("exo reads camelCase: %v", e)
		}
		p := filepath.Join(dir, e["path"].(string))
		if fi, err := os.Stat(p); err != nil || fi.Size() != int64(e["size"].(float64)) {
			t.Fatalf("%s: not there with its listed size, as exo checks", p)
		}
	}
	if _, err := c.Remove(hfRef("mlx-community/m")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".exo-download.json")); !os.IsNotExist(err) {
		t.Fatal("rm left exo's manifest")
	}
	// exo's manifest for another model in that folder is not overwritten
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, ".exo-download.json"), []byte(`{"repo":"other/x","files":[],"complete":true}`), 0o644)
	if _, err := c.Pull(context.Background(), Request{Ref: hfRef("mlx-community/m"), Target: tg}, nil); err == nil {
		t.Fatal("another model's exo manifest was overwritten")
	}
	// exo loads MLX: a GGUF repository is refused before downloading
	g := &fakeHF{files: ggufRepo()}
	srv2 := httptest.NewServer(g.handler())
	defer srv2.Close()
	if _, err := newTestClient(t, srv2, t.TempDir()).Pull(context.Background(), Request{Ref: hfRef("org/g"), Target: Target{Kind: "exo", Path: t.TempDir()}}, nil); AsError(err).Code != ExitUsage {
		t.Fatalf("a GGUF repository to exo: %v", err)
	}
}

func TestMoreAppDirs(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	for _, env := range []string{"LMGET_JAN_DIR", "LMGET_LOCALAI_DIR", "LMGET_EXO_DIR", "XDG_DATA_HOME", "LOCALAI_MODELS_PATH", "MODELS_PATH"} {
		t.Setenv(env, "")
	}
	for app, want := range map[string]string{
		"localai": filepath.Join(home, ".localai", "models"),
	} {
		if got, err := AppModelsDir(app); err != nil || got != want {
			t.Errorf("%s: %s %v", app, got, err)
		}
	}
	if got, _ := AppModelsDir("jan"); !strings.Contains(got, "Jan") {
		t.Errorf("jan: %s", got)
	}
	for _, env := range []string{"EXO_DEFAULT_MODELS_DIR", "EXO_HOME"} {
		t.Setenv(env, "")
	}
	exoData := filepath.Join(home, ".exo") // off Linux, as exo's constants.py says
	if runtime.GOOS == "linux" {
		exoData = filepath.Join(home, ".local", "share", "exo")
	}
	if got, _ := AppModelsDir("exo"); got != filepath.Join(exoData, "models") {
		t.Errorf("exo: %s", got)
	}
	t.Setenv("EXO_HOME", "exohome") // relative to the home folder, as exo reads it
	if got, _ := AppModelsDir("exo"); got != filepath.Join(home, "exohome", "models") {
		t.Errorf("EXO_HOME: %s", got)
	}
	t.Setenv("EXO_DEFAULT_MODELS_DIR", filepath.Join(home, "exomodels"))
	if got, _ := AppModelsDir("exo"); got != filepath.Join(home, "exomodels") {
		t.Errorf("EXO_DEFAULT_MODELS_DIR: %s", got)
	}
	t.Setenv("MODELS_PATH", filepath.Join(home, "lai"))
	if got, _ := AppModelsDir("localai"); got != filepath.Join(home, "lai") {
		t.Errorf("MODELS_PATH: %s", got)
	}
	t.Setenv("LOCALAI_MODELS_PATH", filepath.Join(home, "lai2"))
	if got, _ := AppModelsDir("localai"); got != filepath.Join(home, "lai2") {
		t.Errorf("LOCALAI_MODELS_PATH comes first, as local-ai reads it: %s", got)
	}
	t.Setenv("LOCALAI_MODELS_PATH", "models") // local-ai's own relative default is refused, not guessed
	if _, err := AppModelsDir("localai"); AsError(err).Code != ExitUsage {
		t.Errorf("a relative models path: %v", err)
	}
	t.Setenv("LOCALAI_MODELS_PATH", "")
	custom := filepath.Join(home, "elsewhere")
	t.Setenv("LMGET_JAN_DIR", custom)
	if got, _ := AppModelsDir("jan"); got != custom {
		t.Errorf("LMGET_JAN_DIR: %s", got)
	}
	for _, k := range []string{"jan", "localai", "exo"} {
		if err := lockTargetSyntax(k); err != nil {
			t.Errorf("lock target %s: %v", k, err)
		}
		if _, err := ParseTarget(k); err != nil {
			t.Errorf("--to %s: %v", k, err)
		}
	}
}
