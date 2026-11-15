package lmget

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Audit run 4, hardening: once a newer index has been accepted, an older one
// stays refused even after the newer one expired (the cached copy no longer
// verifies, but the record of the highest accepted sequence remains).
func TestAnOlderIndexStaysRefusedAfterTheNewerOneExpires(t *testing.T) {
	c, is, s := indexSetup(t)
	newer := sampleIndex(3)
	newer.Expires = time.Now().Add(2 * time.Second).UTC().Truncate(time.Second).Add(time.Second)
	publish(is, s, newer)
	if _, err := c.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(newer.Expires) + 100*time.Millisecond)
	if _, err := c.Index(); err == nil {
		t.Fatal("the cached index should have expired: the test proves nothing")
	}
	publish(is, s, sampleIndex(2)) // older, still valid for 30 days
	_, err := c.UpdateIndex(context.Background())
	if err == nil || !strings.Contains(err.Error(), "older than one already accepted") {
		t.Fatalf("UpdateIndex of sequence 2 after 3 expired = %v; want a refusal", err)
	}
	// and a different index with the same sequence as the expired one
	same := sampleIndex(3)
	same.Nodes = []string{"https://other.example.org"}
	publish(is, s, same)
	if _, err := c.UpdateIndex(context.Background()); err == nil || !strings.Contains(err.Error(), "same sequence") {
		t.Fatalf("a different index with sequence 3 = %v; want a refusal", err)
	}
	publish(is, s, sampleIndex(4))
	if _, err := c.UpdateIndex(context.Background()); err != nil {
		t.Fatalf("a newer index must still be accepted: %v", err)
	}
}

// Audit run 4, hardening: the index lists Hugging Face models only. An entry
// of another source, even signed, is refused: its plan from the index would
// skip what that source's resolver checks (a pinned url#sha256, the manifest
// digest of an OCI or Ollama commit).
func TestTheIndexListsHuggingFaceModelsOnly(t *testing.T) {
	c, is, s := indexSetup(t)
	for _, ref := range []string{"https://example.com/m/model.gguf", "ollama:smollm2:135m"} {
		ix := sampleIndex(1)
		ix.Models[0].Ref = ref
		ix.Models[0].Commit = strings.Repeat("c", 64) // an Ollama manifest digest
		if strings.HasPrefix(ref, "https://") {
			ix.Models[0].Commit = urlID(ref) // what a url lock entry carries
		}
		if r, err := ParseRef(ref); err != nil || Lockable(r) != nil {
			t.Fatalf("%s is not a lockable reference (%v): the test proves nothing", ref, err)
		}
		publish(is, s, ix)
		_, err := c.UpdateIndex(context.Background())
		if err == nil || !strings.Contains(AsError(err).Error()+fmtWhy(err), "Hugging Face models only") {
			t.Fatalf("a signed index listing %s = %v; want a refusal", ref, err)
		}
	}
}

func fmtWhy(err error) string { return AsError(err).Why }

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

// Audit run 4, hardening: chunks another origin left on disk are not
// completed from a node. A partial file "from the source" full of garbage
// and marked done would otherwise fail the final check and get the node,
// which serves the right bytes, skipped as bad.
func TestANodeDoesNotResumeAnotherOriginsChunks(t *testing.T) {
	m := setupM3(t)
	u := m.client(t, t.TempDir())
	if _, err := u.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	var big LockFile
	for _, f := range m.lock.Models[0].Files {
		if f.Size > big.Size {
			big = f
		}
	}
	part, state := u.store.tmpPaths(big.SHA256)
	if err := os.MkdirAll(filepath.Dir(part), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part, make([]byte, big.Size), 0o644); err != nil { // zeros, not the model
		t.Fatal(err)
	}
	chunk := int64(256 << 10) // m.client's ChunkSize
	done := make([]bool, (big.Size+chunk-1)/chunk)
	for i := range done {
		done[i] = true
	}
	b, _ := json.Marshal(fetchState{Size: big.Size, Chunk: chunk, Done: done}) // written by the source
	if err := os.WriteFile(state, b, 0o644); err != nil {
		t.Fatal(err)
	}
	m.hf.down.Store(true)
	if _, err := u.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil); err != nil {
		t.Fatalf("the pull from the node failed: %v", err)
	}
	if !u.store.Has(big.SHA256, big.Size) {
		t.Fatal("the file is not in the store")
	}
}
