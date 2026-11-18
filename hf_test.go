package lmget

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Hardening: the token stays with the exact host of the endpoint.
func TestTokenIsNotForwardedToAnotherPort(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		w.Write([]byte(`{"sha":"` + commit + `"}`))
	}))
	defer other.Close()
	endpoint := httptest.NewServer(http.RedirectHandler(other.URL+"/moved", http.StatusFound))
	defer endpoint.Close()
	c, err := New(Options{Home: t.TempDir(), HFEndpoint: endpoint.URL, HFToken: "hf_test_token"})
	if err != nil {
		t.Fatal(err)
	}
	c.Resolve(context.Background(), hfRef("org/m"), Selection{})
	if leaked.Load() {
		t.Fatal("the token followed a redirect to another port")
	}
}

func TestResolveRejectsNegativeSize(t *testing.T) {
	f := &fakeHF{tree: func() []map[string]any {
		return []map[string]any{{"type": "file", "path": "a.bin", "size": -5, "oid": strings.Repeat("a", 40)}}
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	if _, err := c.Resolve(context.Background(), hfRef("org/m"), Selection{}); err == nil {
		t.Fatal("a negative size was accepted (it used to panic in fetch)")
	}
}

func hfRef(repo string) Ref { return Ref{"hf", repo, "main", "", ""} }

func TestRedirectFromHTTPSToHTTPIsRefused(t *testing.T) {
	plain := &fakeURL{}
	plain.set(randBytes(1000))
	ps := httptest.NewServer(plain)
	defer ps.Close()
	tls := httptest.NewTLSServer(http.RedirectHandler(ps.URL+"/m.bin", http.StatusFound))
	defer tls.Close()
	t.Setenv("HF_TOKEN", "")
	c, err := New(Options{Home: t.TempDir(), HFEndpoint: "https://hf.invalid", HTTP: tls.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pullURL(c, tls.URL+"/m.bin", t.TempDir())
	if err == nil || !strings.Contains(AsError(err).Cause+AsError(err).Msg, "https to http") {
		t.Fatalf("got %v, want the downgrade refused", err)
	}
	if plain.served.Load() != 0 {
		t.Fatalf("%d bytes were fetched over plain http", plain.served.Load())
	}
}

func TestTreePaginationThatNeverEndsStops(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/models/org/m/revision/main", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"sha": commit})
	})
	mux.HandleFunc("/api/models/org/m/tree/"+commit, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "<http://"+r.Host+r.URL.Path+"?recursive=true>; rel=\"next\"") // itself
		w.Write([]byte("[]"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	done := make(chan error, 1)
	go func() { _, err := c.Resolve(context.Background(), hfRef("org/m"), Selection{}); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a self-referencing listing was accepted")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pagination loops forever")
	}
}

func TestUnreadableTokenFileIsReported(t *testing.T) {
	skipOnWindows(t, "a file mode cannot make a file unreadable to its owner there")
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("hf_secret"), 0o000)
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HF_TOKEN_PATH", tok)
	if _, src := hfToken(); !strings.HasPrefix(src, "unreadable:") {
		t.Fatalf("source %q: an unreadable token file is reported as missing", src)
	}
}

// Found on 2026-11-18: with HF_HOME pointing elsewhere (a project, a
// container, the tests) lmget fell back to ~/.cache/huggingface/token, the
// user's personal token. huggingface_hub reads one file; so does lmget.
func TestTokenFileIsOneFileNotAChain(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HF_TOKEN_PATH", "")
	t.Setenv("XDG_CACHE_HOME", "")
	personal := filepath.Join(home, ".cache", "huggingface", "token")
	os.MkdirAll(filepath.Dir(personal), 0o700)
	os.WriteFile(personal, []byte("hf_personal"), 0o600)

	if tok, src := hfToken(); tok != "hf_personal" || src != personal {
		t.Fatalf("the default file: %q from %q", tok, src)
	}
	t.Setenv("HF_HOME", t.TempDir()) // a project's own HF_HOME, with no token
	if tok, src := hfToken(); tok != "" {
		t.Fatalf("HF_HOME without a token fell back to %q", src)
	}
	t.Setenv("HF_HOME", "")
	t.Setenv("HF_TOKEN_PATH", filepath.Join(t.TempDir(), "missing"))
	if tok, src := hfToken(); tok != "" {
		t.Fatalf("a missing HF_TOKEN_PATH fell back to %q", src)
	}
	t.Setenv("HF_TOKEN_PATH", "")
	xdg := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdg)
	os.MkdirAll(filepath.Join(xdg, "huggingface"), 0o700)
	os.WriteFile(filepath.Join(xdg, "huggingface", "token"), []byte("hf_xdg"), 0o600)
	if tok, _ := hfToken(); tok != "hf_xdg" {
		t.Fatalf("XDG_CACHE_HOME: %q", tok)
	}
}

// Found on 2026-11-18: Hugging Face answers qwen/qwen3-0.6b for
// Qwen/Qwen3-0.6B, and lmget kept the typed case, so the HF cache got
// models--qwen--qwen3-0.6b, where transformers does not look.
func TestTheRepositorysOwnSpellingIsUsed(t *testing.T) {
	f := &fakeHF{files: modelFiles(), id: "Org/Model-X"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	cache := t.TempDir()
	c := newTestClient(t, srv, t.TempDir())
	t.Setenv("HF_HUB_CACHE", cache)
	out := pullTo(t, c, "org/model-x", Target{Kind: "hf"})
	if out.Plan.Ref.Repo != "Org/Model-X" {
		t.Fatalf("plan repo %q", out.Plan.Ref.Repo)
	}
	if _, err := os.Stat(filepath.Join(cache, "models--Org--Model-X")); err != nil {
		t.Fatal("the HF cache does not use the repository's spelling")
	}
	other := &fakeHF{files: modelFiles(), id: "Someone/Else"}
	srv2 := httptest.NewServer(other.handler())
	defer srv2.Close()
	if _, err := newTestClient(t, srv2, t.TempDir()).Resolve(context.Background(), hfRef("org/model-x"), Selection{}); err == nil {
		t.Fatal("an answer for another repository was accepted")
	}
}
