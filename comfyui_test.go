package lmget

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// tensorFile is a valid safetensors file with one tensor of n bytes, named.
func tensorFile(name string, n int) []byte {
	header := []byte(fmt.Sprintf(`{%q:{"dtype":"U8","shape":[%d],"data_offsets":[0,%d]}}`, name, n, n))
	b := make([]byte, 8, 8+len(header)+n)
	binary.LittleEndian.PutUint64(b, uint64(len(header)))
	return append(append(b, header...), randBytes(n)...)
}

// comfyRepo is laid out as Comfy-Org lays out its repositories, with a full
// checkpoint at the top.
func comfyRepo() []fakeFile {
	return []fakeFile{
		{path: "split_files/diffusion_models/m_fp8.safetensors", data: tensorFile("double_blocks.0.w", 3000), lfs: true},
		{path: "split_files/text_encoders/te.safetensors", data: tensorFile("text_model.w", 2000), lfs: true},
		{path: "split_files/vae/ae.safetensors", data: tensorFile("decoder.w", 1000), lfs: true},
		{path: "m-ckpt.safetensors", data: tensorFile("model.diffusion_model.w", 4000), lfs: true},
		{path: "README.md", data: []byte("# m\n")},
	}
}

func TestComfyUITarget(t *testing.T) {
	f := &fakeHF{files: comfyRepo()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	tg := Target{Kind: "comfyui", Path: t.TempDir()}
	want := map[string]string{ // repository path -> where ComfyUI looks
		"split_files/diffusion_models/m_fp8.safetensors": "diffusion_models/m_fp8.safetensors",
		"split_files/text_encoders/te.safetensors":       "text_encoders/te.safetensors",
		"split_files/vae/ae.safetensors":                 "vae/ae.safetensors",
		"m-ckpt.safetensors":                             "checkpoints/m-ckpt.safetensors",
	}
	for range 2 { // a second pull finds its own links and leaves them
		pullTo(t, c, "org/m", tg)
		for _, x := range f.files {
			dst, ok := want[x.path]
			if !ok {
				continue
			}
			if !hasContent(filepath.Join(tg.Path, filepath.FromSlash(dst)), int64(len(x.data)), sha(x.data)) {
				t.Fatalf("%s is not at %s", x.path, dst)
			}
		}
	}
	var n int
	filepath.Walk(tg.Path, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n++
		}
		return nil
	})
	if n != len(want) { // README.md stays in the store
		t.Fatalf("%d files in the models directory, want %d", n, len(want))
	}
	if _, err := c.Remove(hfRef("org/m")); err != nil {
		t.Fatal(err)
	}
	for _, dst := range want {
		if _, err := os.Lstat(filepath.Join(tg.Path, filepath.FromSlash(dst))); !os.IsNotExist(err) {
			t.Fatalf("rm left %s: %v", dst, err)
		}
	}
}

// A file whose folder neither its path nor its tensors tell is not guessed:
// lmget asks, and the answer costs no second download.
func TestComfyUIAsksForTheFolder(t *testing.T) {
	bare := tensorFile("double_blocks.0.w", 3000) // a diffusion model alone, as black-forest-labs ships Flux
	f := &fakeHF{files: []fakeFile{{path: "flux.safetensors", data: bare, lfs: true}}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	tg := Target{Kind: "comfyui", Path: t.TempDir()}
	_, err := c.Pull(context.Background(), Request{Ref: hfRef("org/flux"), Target: tg}, nil)
	e := AsError(err)
	if e == nil || e.Code != ExitUsage || !strings.Contains(fmt.Sprint(e.Fixes), "--include 'flux.safetensors' --to comfyui:FOLDER") {
		t.Fatalf("a bare diffusion model was placed somewhere: %v", err)
	}
	served := f.served.Load()
	tg.Folder = "diffusion_models"
	pullTo(t, c, "org/flux", tg)
	if f.served.Load() != served {
		t.Fatalf("the answer downloaded again: %d bytes", f.served.Load()-served)
	}
	if !hasContent(filepath.Join(tg.Path, "diffusion_models", "flux.safetensors"), int64(len(bare)), sha(bare)) {
		t.Fatal("not linked where the user said")
	}
}

// What can be refused before a download is: a weight file that only a header
// could place but that has none, two files one name would hold.
func TestComfyUIRefusesBeforeDownloading(t *testing.T) {
	for name, files := range map[string][]fakeFile{
		"pickle at the top": {{path: "m.ckpt", data: randBytes(500), lfs: true}},
		"one name twice": {
			{path: "a/vae/ae.safetensors", data: tensorFile("decoder.w", 100), lfs: true},
			{path: "b/vae/ae.safetensors", data: tensorFile("decoder.w", 200), lfs: true},
		},
		"no weights":                        {{path: "README.md", data: []byte("hi")}},
		"in a folder ComfyUI does not have": {{path: "fp8/m.safetensors", data: tensorFile("model.diffusion_model.w", 100), lfs: true}},
		"diffusers layout": { // its vae/ is diffusers', not ComfyUI's
			{path: "model_index.json", data: []byte(`{}`)},
			{path: "flux.safetensors", data: tensorFile("double_blocks.0.w", 100), lfs: true},
			{path: "vae/diffusion_pytorch_model.safetensors", data: tensorFile("decoder.w", 100), lfs: true},
		},
	} {
		f := &fakeHF{files: files}
		srv := httptest.NewServer(f.handler())
		c := newTestClient(t, srv, t.TempDir())
		_, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "comfyui", Path: t.TempDir()},
			Selection: Selection{AllowPickle: true}}, nil)
		srv.Close()
		if e := AsError(err); e == nil || e.Code != ExitUsage || f.served.Load() != 0 {
			t.Errorf("%s: %v, %d bytes downloaded", name, err, f.served.Load())
		} else if name == "diffusers layout" && !strings.Contains(fmt.Sprint(e.Fixes), "--include 'flux.safetensors'") {
			t.Errorf("the single file is not suggested: %v", e.Fixes)
		}
	}
}

func TestComfyUIDoesNotOverwrite(t *testing.T) {
	files := comfyRepo()[2:3] // the VAE
	f := &fakeHF{files: files}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	tg := Target{Kind: "comfyui", Path: t.TempDir()}
	dst := filepath.Join(tg.Path, "vae", "ae.safetensors")
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.WriteFile(dst, []byte("the user's own VAE"), 0o644)
	if _, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: tg}, nil); err == nil {
		t.Fatal("overwrote a file lmget did not create")
	}
	if b, _ := os.ReadFile(dst); string(b) != "the user's own VAE" {
		t.Fatal("the user's file changed")
	}
	os.WriteFile(dst, files[0].data, 0o644) // the same bytes: left as they are
	pullTo(t, c, "org/m", tg)
	if _, err := c.Remove(hfRef("org/m")); err != nil {
		t.Fatal(err)
	}
	// on Windows lmget copies where it cannot link, so a regular file with a
	// blob's exact bytes is taken as its own (copiesAreLinks), and rm removes it
	if !copiesAreLinks && !hasContent(dst, int64(len(files[0].data)), sha(files[0].data)) {
		t.Fatal("rm removed the user's own copy")
	}
}

func TestComfyUITargetSyntax(t *testing.T) {
	models := t.TempDir()
	t.Setenv("LMGET_COMFYUI_DIR", models)
	tg, err := ParseTarget("comfyui:vae")
	if err != nil || tg.Kind != "comfyui" || tg.Folder != "vae" || tg.Path != models {
		t.Fatalf("comfyui:vae: %+v %v", tg, err)
	}
	for _, bad := range []string{"comfyui:", "comfyui:../x", "comfyui:VAE", "comfyui:diffusers", "comfyuix"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
		if lockTargetSyntax(bad) == nil {
			t.Errorf("a lock may name %s", bad)
		}
	}
	l := &Lock{path: filepath.Join(t.TempDir(), "lmget.lock")}
	if s, err := l.targetString(tg); err != nil || s != "comfyui:vae" || lockTargetSyntax(s) != nil {
		t.Fatalf("a lock keeps the folder: %q %v", s, err)
	}
	if TargetName("comfyui") != "ComfyUI" || TargetName("exo") != "exo" || TargetName("none") == TargetName("hf") {
		t.Fatal("targets are named to people by what they are")
	}
}

func TestComfyUIModelsDir(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("LMGET_COMFYUI_DIR", "")
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	if _, err := AppModelsDir("comfyui"); AsError(err).Code != ExitUsage {
		t.Fatalf("no ComfyUI: %v", err) // never a made-up directory
	}
	ws := filepath.Join(home, "Documents", "comfy", "ComfyUI", "models")
	if runtime.GOOS == "linux" {
		ws = filepath.Join(home, "comfy", "ComfyUI", "models")
	}
	os.MkdirAll(ws, 0o755)
	if got, err := AppModelsDir("comfyui"); err != nil || got != ws {
		t.Fatalf("comfy-cli's workspace: %s %v", got, err)
	}
	if cfg := comfyDesktopConfig(home); cfg != "" { // Desktop's base path comes first
		base := filepath.Join(home, "Comfy Desktop base")
		os.MkdirAll(filepath.Join(base, "models"), 0o755)
		os.MkdirAll(filepath.Dir(cfg), 0o755)
		os.WriteFile(cfg, fmt.Appendf(nil, `{"basePath":%q,"installState":"installed"}`, base), 0o644)
		if got, err := AppModelsDir("comfyui"); err != nil || got != filepath.Join(base, "models") {
			t.Fatalf("Desktop: %s %v", got, err)
		}
		os.WriteFile(cfg, []byte(`{"basePath":"relative"}`), 0o644)
		if _, err := AppModelsDir("comfyui"); err == nil {
			t.Fatal("a relative base path was used")
		}
	}
	t.Setenv("LMGET_COMFYUI_DIR", filepath.Join(home, "elsewhere"))
	if got, _ := AppModelsDir("comfyui"); got != filepath.Join(home, "elsewhere") {
		t.Fatalf("LMGET_COMFYUI_DIR comes first: %s", got)
	}
}
