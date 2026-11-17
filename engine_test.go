package lmget

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeOMLX answers like oMLX 0.8: models owned by omlx, discovered at start
// and on an admin reload that needs the API key, chat completions streamed.
func fakeOMLX(t *testing.T, key string, oom bool) (*httptest.Server, *bool) {
	reloaded := new(bool)
	mux := http.NewServeMux()
	auth := func(r *http.Request) bool { return key == "" || r.Header.Get("Authorization") == "Bearer "+key }
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(401)
			return
		}
		ids := []string{"other-model"}
		if *reloaded {
			ids = append(ids, "Qwen3.5-0.8B-4bit")
		}
		var items []string
		for _, id := range ids {
			items = append(items, fmt.Sprintf(`{"id":%q,"object":"model","owned_by":"omlx"}`, id))
		}
		fmt.Fprintf(w, `{"object":"list","data":[%s]}`, strings.Join(items, ","))
	})
	mux.HandleFunc("POST /admin/api/reload", func(w http.ResponseWriter, r *http.Request) {
		if key == "" || r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"detail":"Admin authentication required"}`)
			return
		}
		*reloaded = true
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if oom {
			w.WriteHeader(507)
			fmt.Fprint(w, `{"error":{"message":"Model does not fit under the dynamic memory ceiling"}}`)
			return
		}
		for _, d := range []string{`{"choices":[{"delta":{"reasoning_content":"thinking..."}}]}`,
			`{"choices":[{"delta":{"content":"I am "}}]}`, `{"choices":[{"delta":{"content":"Qwen."}}]}`, `[DONE]`} {
			fmt.Fprintf(w, "data: %s\n\n", d)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, reloaded
}

func TestRunAsksOMLXAfterAReload(t *testing.T) {
	c := historyClient(t)
	srv, reloaded := fakeOMLX(t, "k3y", false)
	e := &Engine{App: "omlx", URL: srv.URL, key: "k3y"}
	ctx := context.Background()
	if err := c.Identify(ctx, e); err != nil {
		t.Fatal(err)
	}
	plan := &Plan{Ref: hfRef("mlx-community/Qwen3.5-0.8B-4bit")}
	if _, err := c.ModelID(ctx, e, plan, ""); err == nil {
		t.Fatal("a model oMLX does not list yet was found")
	}
	if err := c.Reload(ctx, e); err != nil || !*reloaded {
		t.Fatalf("reload: %v", err)
	}
	id, err := c.ModelID(ctx, e, plan, "")
	if err != nil || id != "Qwen3.5-0.8B-4bit" {
		t.Fatalf("model id %q, %v", id, err)
	}
	var out, think strings.Builder
	st, err := c.Ask(ctx, e, id, "what model are you?", 64, &out, &think)
	if err != nil || out.String() != "I am Qwen." || think.String() != "thinking..." || st.Chars != len("I am Qwen.") {
		t.Fatalf("answer %q, thinking %q, %v", out.String(), think.String(), err)
	}
}

func TestRunOMLXRefusalsAreExplained(t *testing.T) {
	c := historyClient(t)
	ctx := context.Background()
	srv, _ := fakeOMLX(t, "k3y", false)
	err := c.Reload(ctx, &Engine{App: "omlx", URL: srv.URL}) // no key given to lmget
	if AsError(err).Code != ExitAuth || !strings.Contains(fmt.Sprint(AsError(err).Fixes), "restart oMLX") {
		t.Fatalf("a refused reload: %v", err)
	}
	oom, _ := fakeOMLX(t, "", true)
	_, err = c.Ask(ctx, &Engine{App: "omlx", URL: oom.URL}, "m", "hi", 8, &strings.Builder{}, nil)
	if AsError(err).Kind != "memory" {
		t.Fatalf("507 is not reported as memory: %v", err)
	}
	if why := AsError(err).Why; strings.Contains(why, "{") || !strings.Contains(why, "dynamic memory ceiling") {
		t.Fatalf("the app's message is not shown as words: %q", why)
	}
	// another app on the port oMLX was expected at
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"x","owned_by":"organization_owner"}]}`)
	}))
	defer other.Close()
	if err := c.Identify(ctx, &Engine{App: "omlx", URL: other.URL}); err == nil || !strings.Contains(AsError(err).Msg, "is not oMLX") {
		t.Fatalf("another app taken for oMLX: %v", err)
	}
	if err := c.Identify(ctx, &Engine{App: "omlx", URL: "http://127.0.0.1:1"}); AsError(err).Code != ExitNetwork {
		t.Fatalf("nothing listening: %v", err)
	}
}

func TestRunAsksOllama(t *testing.T) {
	c := historyClient(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"version":"0.40.16"}`) })
	mux.HandleFunc("POST /api/chat", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"message":{"role":"assistant","thinking":"hmm"},"done":false}`)
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"Hello"},"done":false}`)
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"!"},"done":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	e := &Engine{App: "ollama", URL: srv.URL}
	if err := c.Identify(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	id, _ := c.ModelID(context.Background(), e, &Plan{}, "smollm2:135m")
	var out, think strings.Builder
	if _, err := c.Ask(context.Background(), e, id, "hi", 8, &out, &think); err != nil || out.String() != "Hello!" || think.String() != "hmm" {
		t.Fatalf("%q %q %v", out.String(), think.String(), err)
	}
}

func TestEngineAddresses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LMGET_OMLX_URL", "")
	t.Setenv("OLLAMA_HOST", "")
	if e, _ := EngineFor("ollama", ""); e.URL != "http://127.0.0.1:11434" {
		t.Fatal(e.URL)
	}
	for in, want := range map[string]string{"0.0.0.0:11500": "http://127.0.0.1:11500", "http://10.0.0.5": "http://10.0.0.5:11434", "gpu.lan:9": "http://gpu.lan:9"} {
		t.Setenv("OLLAMA_HOST", in)
		if e, _ := EngineFor("ollama", ""); e.URL != want {
			t.Errorf("OLLAMA_HOST=%s: %s, want %s", in, e.URL, want)
		}
	}
	if e, _ := EngineFor("omlx", ""); e.URL != "http://127.0.0.1:8000" {
		t.Fatalf("oMLX without settings: %s", e.URL)
	}
	// a base path moved by oMLX's launcher, with the port in its settings
	base := filepath.Join(home, "elsewhere")
	os.MkdirAll(filepath.Join(home, "Library", "Application Support", "oMLX"), 0o755)
	os.WriteFile(filepath.Join(home, "Library", "Application Support", "oMLX", "base-path"), []byte(base+"\n"), 0o644)
	os.MkdirAll(base, 0o755)
	os.WriteFile(filepath.Join(base, "settings.json"), []byte(`{"server":{"host":"127.0.0.1","port":1234},"auth":{"api_key":"never-read"},"model":{"model_dir":"/m"}}`), 0o600)
	if e, _ := EngineFor("omlx", ""); e.URL != "http://127.0.0.1:1234" || e.key != "" {
		t.Fatalf("oMLX from its moved settings: %s (key %q)", e.URL, e.key)
	}
	if d, err := AppModelsDir("omlx"); err != nil || d != "/m" {
		t.Fatalf("oMLX models dir from the moved settings: %s %v", d, err)
	}
	for u, remote := range map[string]bool{"http://127.0.0.1:1234": false, "http://localhost:8000": false, "http://[::1]:8000": false, "http://gpu.lan:8000": true, "https://api.example.org": true} {
		if (&Engine{URL: u}).Remote() != remote {
			t.Errorf("%s remote=%v", u, !remote)
		}
	}
	if _, err := EngineFor("hf", ""); err == nil {
		t.Fatal("the Hugging Face cache has no app to ask")
	}
	if _, err := EngineFor("", "ftp://x"); err == nil {
		t.Fatal("a non-http endpoint was accepted")
	}
}

func TestMemoryCeiling(t *testing.T) {
	now := time.Now()
	run := func(variant, kind string, ago time.Duration) HistoryEntry {
		return HistoryEntry{Time: now.Add(-ago), Command: "run", Nickname: "qwen", Error: kind, Choice: &HistoryChoice{Variant: variant}}
	}
	h := []HistoryEntry{
		run("hf:big", "memory", time.Hour),
		run("hf:huge", "memory", 10*24*time.Hour), // too old
		run("hf:mid", "engine", time.Hour),        // not memory
	}
	got := MemoryCeiling(h, "qwen", now)
	if len(got) != 1 || got["hf:big"].IsZero() {
		t.Fatalf("%v", got)
	}
	if len(MemoryCeiling(h, "gemma", now)) != 0 {
		t.Fatal("another nickname")
	}
}
