package lmget

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// colibriComplete is colibri's is_complete (c/setup_download.py): the
// manifest says complete, and every file it lists is in the folder at its size.
func colibriComplete(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, ".colibri-download.json"))
	if err != nil {
		return false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil || m["complete"] != true {
		return false
	}
	files, _ := m["files"].([]any)
	for _, x := range files {
		e, _ := x.(map[string]any)
		p, _ := e["path"].(string)
		size, _ := e["size"].(float64)
		fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil || fi.Size() != int64(size) {
			return false
		}
	}
	return len(files) > 0
}

func TestColibriTarget(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	tg := Target{Kind: "colibri", Path: t.TempDir()}
	for range 2 { // a second pull finds its own manifest and links
		pullTo(t, c, "Justvugg/m-colibri-int4", tg)
	}
	dir := filepath.Join(tg.Path, "Justvugg--m-colibri-int4")
	b, err := os.ReadFile(filepath.Join(dir, ".colibri-download.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["repo"] != "Justvugg/m-colibri-int4" || m["revision"] != commit || m["lmget_export"] != true || len(m["files"].([]any)) != len(f.files) {
		t.Fatalf("manifest %s", b)
	}
	if !colibriComplete(dir) {
		t.Fatal("colibri would not take the folder as a complete download")
	}
	if _, err := c.Remove(hfRef("Justvugg/m-colibri-int4")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".colibri-download.json")); !os.IsNotExist(err) {
		t.Fatal("rm left lmget's manifest")
	}
}

// A folder colibri downloaded itself is left alone: its manifest is not
// overwritten, nothing is linked into it, and rm does not take it.
func TestColibriLeavesItsOwnDownloads(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	tg := Target{Kind: "colibri", Path: t.TempDir()}
	dir := filepath.Join(tg.Path, "org--m")
	os.MkdirAll(dir, 0o755)
	theirs := `{"repo": "org/m", "revision": "main", "complete": true, "files": []}`
	mp := filepath.Join(dir, ".colibri-download.json")
	os.WriteFile(mp, []byte(theirs), 0o644)
	if _, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: tg}, nil); err == nil {
		t.Fatal("colibri's own download was overwritten")
	}
	if b, _ := os.ReadFile(mp); string(b) != theirs {
		t.Fatal("colibri's manifest changed")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("%d entries linked into colibri's folder", len(entries)-1)
	}
	if ourSidecar(mp, &Plan{Ref: hfRef("org/m")}) {
		t.Fatal("rm would take colibri's manifest for lmget's")
	}
}

// colibri keeps only a model's dense part in memory: the estimate of the
// whole weights would say a model does not fit when it runs.
func TestColibriGetsNoMemoryEstimate(t *testing.T) {
	f := &fakeHF{files: []fakeFile{
		{path: "config.json", data: []byte(cfgQwen3_06B)},
		{path: "model.safetensors", data: fakeSafetensors(4000), lfs: true},
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	for kind, want := range map[string]bool{"none": true, "colibri": false} {
		out, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: kind, Path: t.TempDir()}, DryRun: true}, nil)
		if err != nil || (out.Plan.Memory != nil) != want {
			t.Fatalf("%s: %+v %v", kind, out.Plan.Memory, err)
		}
	}
}

func TestColibriRefusesWhatItCannotLoad(t *testing.T) {
	g := &fakeHF{files: ggufRepo()}
	srv := httptest.NewServer(g.handler())
	defer srv.Close()
	_, err := newTestClient(t, srv, t.TempDir()).Pull(context.Background(), Request{Ref: hfRef("org/g"), Target: Target{Kind: "colibri", Path: t.TempDir()}}, nil)
	e := AsError(err)
	if e == nil || e.Code != ExitUsage || !strings.Contains(fmt.Sprint(e.Fixes), "coli import") || g.served.Load() != 0 {
		t.Fatalf("a GGUF repository to colibri: %v, %d bytes", err, g.served.Load())
	}
}

func TestColibriModelsDir(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("LMGET_COLIBRI_DIR", "")
	if got, err := AppModelsDir("colibri"); err != nil || got != filepath.Join(home, "colibri-models") {
		t.Fatalf("default: %s %v", got, err)
	}
	t.Setenv("LMGET_COLIBRI_DIR", filepath.Join(home, "nvme"))
	if got, _ := AppModelsDir("colibri"); got != filepath.Join(home, "nvme") {
		t.Fatalf("LMGET_COLIBRI_DIR: %s", got)
	}
	if tg, err := ParseTarget("colibri"); err != nil || tg.Kind != "colibri" || lockTargetSyntax("colibri") != nil || TargetName("colibri") != "colibri" {
		t.Fatalf("colibri target: %+v %v", tg, err)
	}
}
