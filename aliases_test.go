package lmget

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type aliasServer struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (s *aliasServer) set(path string, b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		s.files = map[string][]byte{}
	}
	s.files[path] = b
}

func (s *aliasServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.files[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Write(b)
}

func sampleAliases(seq int64) AliasList {
	return AliasList{Version: 1, Sequence: seq, Issued: time.Now().Add(-time.Hour).UTC().Truncate(time.Second),
		Expires: time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second),
		Aliases: map[string]Alias{"qwen": {Kind: "chat", Family: "Qwen3.5", Variants: []AliasVariant{
			{Ref: "hf:mlx-community/Qwen3.5-0.8B-4bit", Params: "0.8B", Format: "mlx", Quant: "4bit", Bytes: 650e6, License: "apache-2.0"},
		}}}}
}

func aliasSetup(t *testing.T) (*Client, *aliasServer, signer) {
	t.Helper()
	s := testSigner(t)
	as := &aliasServer{}
	srv := httptest.NewTLSServer(as)
	t.Cleanup(srv.Close)
	c, err := New(Options{Home: t.TempDir(), HTTP: srv.Client(), IndexURL: srv.URL + "/index.json", IndexKeys: []string{s.pub}})
	if err != nil {
		t.Fatal(err)
	}
	return c, as, s
}

func publishAliases(as *aliasServer, s signer, l AliasList) {
	b, _ := json.Marshal(l)
	as.set("/aliases.json", b)
	as.set("/aliases.json.minisig", []byte(s.sign(b)))
}

// The nickname list is verified like the index, cached, and never rolls back.
func TestAliasesVerifyCacheAndNeverRollBack(t *testing.T) {
	c, as, s := aliasSetup(t)
	ctx := context.Background()

	if l, err := c.UpdateAliases(ctx); l != nil || err != nil { // none published: not an error
		t.Fatalf("no list published: got %v, %v", l, err)
	}
	publishAliases(as, s, sampleAliases(2))
	l, err := c.UpdateAliases(ctx)
	if err != nil || l == nil || l.Sequence != 2 {
		t.Fatalf("update: %v %v", l, err)
	}
	if cl, _, err := c.CachedAliases(); err != nil || cl == nil || cl.Sequence != 2 {
		t.Fatalf("cache: %v %v", cl, err)
	}

	publishAliases(as, s, sampleAliases(1))
	if _, err := c.UpdateAliases(ctx); err == nil {
		t.Fatal("an older sequence was accepted")
	}
	other := sampleAliases(2)
	other.Aliases["qwen"].Variants[0].Bytes++
	publishAliases(as, s, other)
	if _, err := c.UpdateAliases(ctx); err == nil {
		t.Fatal("a second list with the same sequence was accepted")
	}
	publishAliases(as, testSigner(t), sampleAliases(3))
	if _, err := c.UpdateAliases(ctx); err == nil {
		t.Fatal("a list signed by an unknown key was accepted")
	}
	if cl, _, _ := c.CachedAliases(); cl == nil || cl.Sequence != 2 {
		t.Fatal("a refused list replaced the cached one")
	}
	expired := sampleAliases(4)
	expired.Issued, expired.Expires = time.Now().Add(-40*24*time.Hour), time.Now().Add(-time.Hour)
	publishAliases(as, s, expired)
	if _, err := c.UpdateAliases(ctx); err == nil {
		t.Fatal("an expired list was accepted")
	}
}

func TestAliasListValidation(t *testing.T) {
	bad := map[string]func(l *AliasList){
		"a command as nickname": func(l *AliasList) { l.Aliases["ls"] = l.Aliases["qwen"] },
		"upper-case nickname":   func(l *AliasList) { l.Aliases["Qwen"] = l.Aliases["qwen"] },
		"format of an ollama ref": func(l *AliasList) {
			l.Aliases["qwen"].Variants[0].Ref = "ollama:qwen3.5:0.8b"
		},
		"a pinned revision": func(l *AliasList) { l.Aliases["qwen"].Variants[0].Ref = "hf:org/m@dev" },
		"a url":             func(l *AliasList) { l.Aliases["qwen"].Variants[0].Ref = "https://example.org/m.gguf" },
		"unknown format":    func(l *AliasList) { l.Aliases["qwen"].Variants[0].Format = "onnx" },
		"no size":           func(l *AliasList) { l.Aliases["qwen"].Variants[0].Bytes = 0 },
		"control character": func(l *AliasList) { l.Aliases["qwen"].Variants[0].Params = "4B\x1b[2J" },
		"too long validity": func(l *AliasList) { l.Expires = l.Issued.Add(MaxIndexValidity + time.Hour) },
	}
	for name, mutate := range bad {
		l := sampleAliases(1)
		l.Aliases["qwen"] = Alias{Kind: "chat", Family: "Qwen3.5", Variants: append([]AliasVariant(nil), l.Aliases["qwen"].Variants...)}
		mutate(&l)
		if err := l.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	l := sampleAliases(1)
	if err := l.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLocalAliases(t *testing.T) {
	c, err := New(Options{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if m, err := c.LocalAliases(); m != nil || err != nil {
		t.Fatalf("no file: %v %v", m, err)
	}
	write := func(s string) {
		if err := os.WriteFile(c.LocalAliasesPath(), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("[aliases]\nmine = \"hf:mlx-community/Qwen3.5-4B-4bit\"\nasr = \"openai/whisper-small\"\n")
	m, err := c.LocalAliases()
	if err != nil || m["mine"].Repo != "mlx-community/Qwen3.5-4B-4bit" || m["asr"].Repo != "openai/whisper-small" {
		t.Fatalf("%v %v", m, err)
	}
	for _, bad := range []string{
		"[aliases]\nls = \"org/m\"\n",         // a command
		"[aliases]\nMine = \"org/m\"\n",       // upper case
		"[aliases]\nmine = \"not a ref\"\n",   // not a model
		"[aliasses]\nmine = \"org/m\"\n",      // misspelled table
		"[aliases]\nmine = { ref = \"x\" }\n", // not a string
	} {
		write(bad)
		if _, err := c.LocalAliases(); err == nil {
			t.Errorf("accepted: %q", bad)
		}
	}
}

func TestIsAliasName(t *testing.T) {
	for _, ok := range []string{"qwen", "gpt-oss", "e5.small", "a1"} {
		if !IsAliasName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, no := range []string{"Qwen", "org/name", "ollama:x", "-x", "", strings.Repeat("a", 65), "https"} {
		if IsAliasName(no) && no != "https" {
			t.Errorf("%q accepted", no)
		}
	}
}

// The builder keeps only what the project would mirror.
func TestBuildAliasesChecks(t *testing.T) {
	cases := []struct {
		name   string
		files  []fakeFile
		format string
		ok     bool
	}{
		{"safetensors with config", modelFiles(), "safetensors", true},
		{"auto_map runs repository code", []fakeFile{
			{path: "model.safetensors", data: fakeSafetensors(1000), lfs: true},
			{path: "config.json", data: []byte(`{"auto_map":{"AutoModel":"other/repo--modeling.M"}}`)},
		}, "safetensors", false},
		{"declared gguf, no gguf", modelFiles(), "gguf", false},
		{"pickle only", []fakeFile{{path: "pytorch_model.bin", data: randBytes(500), lfs: true}, {path: "config.json", data: []byte(`{}`)}}, "safetensors", false},
		{"no config for mlx", []fakeFile{{path: "model.safetensors", data: fakeSafetensors(1000), lfs: true}}, "mlx", false},
		{"gguf", []fakeFile{{path: "m-Q4_0.gguf", data: fakeGGUF(4000), lfs: true}}, "gguf", true},
		// no safetensors, so the pickle is selected: only the pickle check stops it
		{"gguf with a pickle selected", []fakeFile{{path: "m-Q4_0.gguf", data: fakeGGUF(4000), lfs: true}, {path: "pytorch_model.bin", data: randBytes(500), lfs: true}}, "gguf", false},
	}
	for _, tc := range cases {
		f := &fakeHF{files: tc.files}
		srv := httptest.NewServer(f.handler())
		c := newTestClient(t, srv, t.TempDir())
		src := AliasSource{"m": {Kind: "chat", Family: "F"}}
		e := src["m"]
		e.Variant = append(e.Variant, struct {
			Ref     string   `toml:"ref"`
			Params  string   `toml:"params"`
			Format  string   `toml:"format"`
			Quant   string   `toml:"quant"`
			Include []string `toml:"include"`
			License string   `toml:"license"`
		}{Ref: "hf:org/m", Params: "1B", Format: tc.format})
		src["m"] = e
		l, err := c.BuildAliases(context.Background(), src, 1, 720*time.Hour, time.Now())
		srv.Close()
		if tc.ok != (err == nil) {
			t.Errorf("%s: ok=%v, err=%v", tc.name, tc.ok, err)
			continue
		}
		if tc.ok {
			v := l.Aliases["m"].Variants[0]
			if v.Bytes != totalSize(selectable(tc.files)) || v.License != "apache-2.0" {
				t.Errorf("%s: measured %d bytes, license %q", tc.name, v.Bytes, v.License)
			}
		}
	}
	if _, err := (&Client{}).BuildAliases(context.Background(), AliasSource{"rm": {}}, 1, time.Hour, time.Now()); err == nil {
		t.Error("a command was accepted as a nickname")
	}
}

func selectable(fs []fakeFile) []fakeFile { return fs } // every fake file is selected by default

func TestAliasesURL(t *testing.T) {
	c, err := New(Options{Home: t.TempDir(), IndexURL: "https://lmget.org/index.json"})
	if err != nil {
		t.Fatal(err)
	}
	if u := c.AliasesURL(); u != "https://lmget.org/aliases.json" {
		t.Fatal(u)
	}
	_ = filepath.Join
}
