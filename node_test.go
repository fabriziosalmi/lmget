package lmget

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

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

// Audit run 4, hardening: serve verifies the index once and serves the index
// and its signature from that snapshot, so the pair always matches and a
// flood of requests does not verify it again each time.
func TestNodeServesTheIndexFromOneVerifiedSnapshot(t *testing.T) {
	m := setupM3(t)
	get := func(path string) (int, []byte) {
		resp, err := m.tlsHTTP.Get(m.node.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	code, data := get("/index.json")
	if code != 200 {
		t.Fatalf("index.json = %d", code)
	}
	_, ps := m.nodeCli.indexPaths()
	if err := os.Remove(ps); err != nil { // the file changes under the server
		t.Fatal(err)
	}
	code, sig := get("/index.json.minisig")
	if code != 200 {
		t.Fatalf("the signature was read from disk again (%d), not from the verified snapshot", code)
	}
	if _, err := verifyMinisign(m.signer.key, data, string(sig)); err != nil {
		t.Fatalf("the index and the signature served do not match: %v", err)
	}
}

// Audit run 4, hardening: serve answers a blob only when the file in its
// store has the size the index signs.
func TestNodeDoesNotServeABlobOfTheWrongSize(t *testing.T) {
	m := setupM3(t)
	sha := m.lock.Models[0].Files[0].SHA256
	p := m.nodeCli.store.BlobPath(sha)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte("not the signed bytes"), 0o444); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, 0o644) // tampering starts by clearing read-only: Windows refuses a rename over a read-only file
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
	resp, err := m.tlsHTTP.Get(m.node.URL + "/blobs/sha256/" + sha)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a blob of the wrong size answered %d; want 404", resp.StatusCode)
	}
}

func TestNodeServesOnlyIndexedBlobs(t *testing.T) {
	m := setupM3(t)
	cl := m.node.Client()
	get := func(path string, hdr ...string) (int, []byte) {
		req, _ := http.NewRequest(http.MethodGet, m.node.URL+path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	model := m.files[0].data
	if code, b := get("/blobs/sha256/" + sha(model)); code != 200 || !bytes.Equal(b, model) {
		t.Fatalf("an indexed blob: %d", code)
	}
	if code, b := get("/blobs/sha256/"+sha(model), "Range", "bytes=10-19"); code != 206 || !bytes.Equal(b, model[10:20]) {
		t.Fatalf("a range: %d", code)
	}
	// a blob the node's store holds but the index does not name
	other := randBytes(100)
	os.WriteFile(m.nodeCli.store.BlobPath(sha(other)), other, 0o444)
	for _, p := range []string{"/blobs/sha256/" + sha(other), "/blobs/sha256/../../index.json", "/blobs/sha256/" + strings.ToUpper(sha(model)), "/", "/refs/"} {
		if code, _ := get(p); code == 200 {
			t.Errorf("%s served", p)
		}
	}
	if code, b := get("/index.json"); code != 200 || !json.Valid(b) {
		t.Fatalf("the index: %d", code)
	}
}

// a node refuses to mirror a model whose source no longer matches the index
func TestMirrorRefusesDrift(t *testing.T) {
	m := setupM3(t)
	m.files[1].data = []byte(`{"a":2}`) // config.json changed at the source, same commit
	m.hf.h = (&fakeHF{files: m.files}).handler()
	n := m.client(t, t.TempDir())
	n.UpdateIndex(context.Background())
	res, err := n.MirrorIndex(context.Background(), nil)
	if err != nil || len(res) != 1 || res[0].Err == "" {
		t.Fatalf("drift was mirrored: %+v %v", res, err)
	}
}

// defence in depth: a node does not mirror a model whose license is not on the
// allowlist, even when a signed index lists it
func TestMirrorSkipsLicensesOffTheAllowlist(t *testing.T) {
	m := setupM3(t)
	ix := sampleIndex(2)
	ix.Nodes = []string{m.node.URL}
	ix.Models[0].Ref, ix.Models[0].Commit, ix.Models[0].License = "hf:org/m", commit, "llama3"
	ix.Models[0].Files = nil
	for _, lf := range m.lock.Models[0].Files {
		ix.Models[0].Files = append(ix.Models[0].Files, IndexFile{LockFile: lf})
	}
	publish(m.idx, m.signer, ix)
	n := m.client(t, t.TempDir())
	if _, err := n.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := n.MirrorIndex(context.Background(), nil)
	if err != nil || len(res) != 1 || !strings.Contains(res[0].Err, "allowlist") {
		t.Fatalf("got %+v %v", res, err)
	}
	if st := n.Stats(); st.Blobs != 0 {
		t.Fatalf("%d blobs mirrored for a model off the allowlist", st.Blobs)
	}
}
