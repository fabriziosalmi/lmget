package lmget

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Audit run 4. A registry names a token service (realm) on the HF endpoint:
// lmget must not send the HF token there, so nothing HF answers for the
// user's token can reach the registry.
func TestOCIRealmOnTheHFEndpointGetsNoHFToken(t *testing.T) {
	var mu sync.Mutex
	var hfAuth, regAuth []string
	hf := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hfAuth = append(hfAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer hf_secret_for_hf_only" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"token": "HF-DERIVED"})
	}))
	defer hf.Close()
	reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		regAuth = append(regAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+hf.URL+`/api/spaces/victim/space/jwt",service="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer reg.Close()
	t.Setenv("HF_TOKEN_PATH", filepath.Join(t.TempDir(), "none"))
	t.Setenv("HF_HOME", t.TempDir())
	c, err := New(Options{Home: t.TempDir(), HFEndpoint: hf.URL, HFToken: "hf_secret_for_hf_only", HTTP: reg.Client(), NoMirror: true})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := ParseRef("oci://" + strings.TrimPrefix(reg.URL, "https://") + "/evil/model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pull(context.Background(), Request{Ref: ref, Target: Target{Kind: "none"}}, nil); err == nil {
		t.Fatal("the pull succeeded")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hfAuth) == 0 {
		t.Fatal("the realm was never asked: the test proves nothing")
	}
	for _, a := range hfAuth {
		if a != "" {
			t.Fatalf("the realm on the HF endpoint got %q", a)
		}
	}
	for _, a := range regAuth {
		if strings.Contains(a, "HF-DERIVED") || strings.Contains(a, "hf_secret") {
			t.Fatalf("the registry got %q", a)
		}
	}
}

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

// Audit run 4: serve stops serving blobs once the index it serves from
// expires, as /index.json does, without a restart.
func TestNodeStopsServingBlobsWhenTheIndexExpires(t *testing.T) {
	m := setupM3(t)
	b := m.client(t, t.TempDir())
	ix, err := b.BuildIndex(context.Background(), m.lock, BuildOptions{Sequence: 2, Valid: 3 * time.Second, Nodes: []string{m.node.URL}})
	if err != nil {
		t.Fatal(err)
	}
	publish(m.idx, m.signer, *ix)
	if _, err := m.nodeCli.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(m.nodeCli.NodeHandler())
	defer srv.Close()
	blob := srv.URL + "/blobs/sha256/" + ix.Models[0].Files[0].SHA256
	status := func(u string) int {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if s := status(blob); s != http.StatusOK {
		t.Fatalf("before expiry the blob answered %d", s)
	}
	time.Sleep(time.Until(ix.Expires) + 200*time.Millisecond)
	if s := status(srv.URL + "/index.json"); s != http.StatusServiceUnavailable {
		t.Fatalf("after expiry /index.json answered %d", s)
	}
	if s := status(blob); s != http.StatusServiceUnavailable {
		t.Fatalf("after expiry the blob answered %d; want 503 like /index.json", s)
	}
}
