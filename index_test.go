package lmget

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/blake2b"
)

// Signatures made by the real minisign, published in jedisct1/go-minisign.
const (
	vectorKey      = "RWQf6LRCGA9i53mlYecO4IzT51TGPpvWucNSCh1CBM0QTaLn73Y7GFO3" // gitleaks:allow (minisign PUBLIC test key from go-minisign)
	vectorLegacy   = "untrusted comment: signature from minisign secret key\nRWQf6LRCGA9i59SLOFxz6NxvASXDJeRtuZykwQepbDEGt87ig1BNpWaVWuNrm73YiIiJbq71Wi+dP9eKL8OC351vwIasSSbXxwA=\ntrusted comment: timestamp:1635442742\tfile:test\n0YteLgV960ia80vnA/fHbvkyjl/IoP/HNOCaZfrF0CdhAlp7ok+Tpkya+VpWPX5C/Is3q8a/kEDSY7fBmmgJCg==\n"
	vectorPrehashd = "untrusted comment: signature from minisign secret key\nRUQf6LRCGA9i559r3g7V1qNyJDApGip8MfqcadIgT9CuhV3EMhHoN1mGTkUidF/z7SrlQgXdy8ofjb7bNJJylDOocrCo8KLzZwo=\ntrusted comment: timestamp:1635443258\tfile:test\thashed\n/cj37GK60vryibFn+ftOgbCvW9NKhKYgjVpFFQUcWPAnjO23wrvVDTt7cloNC06maoBli9q6qwZDXXoaxweICQ==\n"
)

func TestMinisignOfficialVectors(t *testing.T) {
	k, err := ParseMinisignKey("untrusted comment: minisign public key\n" + vectorKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, sig := range map[string]string{"legacy": vectorLegacy, "prehashed": vectorPrehashd} {
		if _, err := verifyMinisign(k, []byte("test"), sig); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := verifyMinisign(k, []byte("tesT"), sig); err == nil {
			t.Errorf("%s: other data accepted", name)
		}
		lines := strings.Split(sig, "\n")
		lines[2] += "x" // the trusted comment is signed too
		if _, err := verifyMinisign(k, []byte("test"), strings.Join(lines, "\n")); err == nil {
			t.Errorf("%s: a changed trusted comment was accepted", name)
		}
	}
	other := testSigner(t)
	if _, err := verifyMinisign(other.key, []byte("test"), vectorPrehashd); err == nil {
		t.Error("a signature by another key was accepted")
	}
}

// testSigner writes minisign prehashed signatures, the format the verifier
// accepts on the official vectors above.
type signer struct {
	sk  ed25519.PrivateKey
	key MinisignKey
	pub string
}

func testSigner(t testing.TB) signer {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var id [8]byte
	rand.Read(id[:])
	pub := base64.StdEncoding.EncodeToString(append(append([]byte("Ed"), id[:]...), pk...))
	k, _ := ParseMinisignKey(pub)
	return signer{sk, k, pub}
}

func (s signer) sign(data []byte) string {
	h := blake2b.Sum512(data)
	sig := append(append([]byte("ED"), s.key.ID[:]...), ed25519.Sign(s.sk, h[:])...)
	trusted := "timestamp:1794700000\tfile:index.json\thashed"
	global := ed25519.Sign(s.sk, append(append([]byte(nil), sig[10:]...), trusted...))
	return "untrusted comment: signature from minisign secret key\n" + base64.StdEncoding.EncodeToString(sig) +
		"\ntrusted comment: " + trusted + "\n" + base64.StdEncoding.EncodeToString(global) + "\n"
}

func sampleIndex(seq int64) Index {
	return Index{Version: 1, Sequence: seq, Issued: time.Now().Add(-time.Hour).UTC().Truncate(time.Second),
		Expires: time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second),
		Nodes:   []string{"https://node1.example.org"},
		Models: []IndexModel{{Ref: "hf:org/m", Commit: strings.Repeat("a", 40), License: "apache-2.0",
			Files: []IndexFile{{LockFile: LockFile{Path: "model.safetensors", Size: 10, SHA256: strings.Repeat("b", 64)}}}}}}
}

// indexServer serves whatever index and signature the test sets.
type indexServer struct {
	mu        sync.Mutex
	data, sig []byte
}

func (s *indexServer) set(data []byte, sig string) {
	s.mu.Lock()
	s.data, s.sig = data, []byte(sig)
	s.mu.Unlock()
}

func (s *indexServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, ".minisig") {
		w.Write(s.sig)
		return
	}
	w.Write(s.data)
}

func indexSetup(t *testing.T) (*Client, *indexServer, signer) {
	t.Helper()
	s := testSigner(t)
	is := &indexServer{}
	srv := httptest.NewTLSServer(is)
	t.Cleanup(srv.Close)
	c, err := New(Options{Home: t.TempDir(), HTTP: srv.Client(), IndexURL: srv.URL + "/index.json", IndexKeys: []string{s.pub}})
	if err != nil {
		t.Fatal(err)
	}
	return c, is, s
}

func publish(is *indexServer, s signer, ix Index) []byte {
	b, _ := json.Marshal(ix)
	is.set(b, s.sign(b))
	return b
}

func TestIndexUpdateVerifiesAndCaches(t *testing.T) {
	c, is, s := indexSetup(t)
	publish(is, s, sampleIndex(1))
	ix, err := c.UpdateIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := ix.Find(Ref{Source: "hf", Repo: "org/m", Revision: "main"}, strings.Repeat("a", 40)); got == nil {
		t.Fatal("the indexed model is not found")
	}
	if _, err := c.Index(); err != nil {
		t.Fatalf("the cached index does not verify: %v", err)
	}
	// the cache is verified again on every read
	p, _ := c.indexPaths()
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "apache-2.0", "apache-2.1", 1)), 0o644)
	if _, err := c.Index(); AsError(err).Code != ExitVerify {
		t.Fatalf("a changed cached index was accepted: %v", err)
	}
}

func TestIndexRefusesWhatItShouldNot(t *testing.T) {
	c, is, s := indexSetup(t)
	publish(is, s, sampleIndex(5))
	if _, err := c.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	expect := func(name string, ix Index, sig func([]byte) string) {
		t.Helper()
		b, _ := json.Marshal(ix)
		is.set(b, sig(b))
		if _, err := c.UpdateIndex(context.Background()); AsError(err).Code != ExitVerify {
			t.Errorf("%s: got %v, want a verification failure", name, err)
		}
	}
	expect("an older sequence", sampleIndex(4), s.sign)
	same := sampleIndex(5)
	same.Models[0].License = "mit"
	expect("the same sequence, other content", same, s.sign)
	other := testSigner(t)
	expect("another key", sampleIndex(6), other.sign)
	exp := sampleIndex(6)
	exp.Issued, exp.Expires = time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour)
	expect("expired", exp, s.sign)
	bad := sampleIndex(6)
	bad.Nodes = []string{"http://node.example.org"}
	expect("a plain http node", bad, s.sign)
	bad = sampleIndex(6)
	bad.Models[0].Files[0].Path = "../../.bashrc"
	expect("a path that leaves the model", bad, s.sign)
	bad = sampleIndex(6)
	bad.Models[0].Files[0].BTIH2 = "not-an-infohash"
	expect("a malformed infohash", bad, s.sign)
	bad = sampleIndex(6)
	bad.Models[0].Ref = "https://example.org/x?token=1"
	expect("a ref with a query string", bad, s.sign)
	// an unknown field is refused even when signed
	b, _ := json.Marshal(sampleIndex(6))
	b = []byte(strings.Replace(string(b), `"version":1`, `"version":1,"surprise":true`, 1))
	is.set(b, s.sign(b))
	if _, err := c.UpdateIndex(context.Background()); AsError(err).Code != ExitVerify {
		t.Errorf("an unknown field was accepted: %v", err)
	}
	// and a newer, valid one is taken
	publish(is, s, sampleIndex(6))
	if ix, err := c.UpdateIndex(context.Background()); err != nil || ix.Sequence != 6 {
		t.Fatalf("a newer valid index was refused: %v", err)
	}
}

func TestIndexNeedsAKey(t *testing.T) {
	t.Setenv("LMGET_INDEX_KEY", "")
	saved := IndexKeys
	IndexKeys = nil // a build without the project key
	t.Cleanup(func() { IndexKeys = saved })
	hit := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	c, err := New(Options{Home: t.TempDir(), HTTP: srv.Client(), IndexURL: srv.URL + "/index.json"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateIndex(context.Background()); AsError(err).Code != ExitUsage {
		t.Fatalf("got %v, want a usage error: no key, nothing is trusted", err)
	}
	if hit {
		t.Fatal("the index was fetched although no key could verify it")
	}
}

// The built-in project key is the one minisign generated (its key number is
// printed reversed, as a little-endian uint64) and matches lmget-index.pub.
func TestBuiltInProjectKey(t *testing.T) {
	if len(IndexKeys) != 1 {
		t.Fatalf("%d built-in keys", len(IndexKeys))
	}
	k, err := ParseMinisignKey(IndexKeys[0])
	if err != nil {
		t.Fatal(err)
	}
	var num strings.Builder
	for i := 7; i >= 0; i-- {
		fmt.Fprintf(&num, "%02X", k.ID[i])
	}
	if num.String() != "21C7BDA090E969F4" {
		t.Fatalf("key number %s", num.String())
	}
	pub, err := os.ReadFile("lmget-index.pub")
	if err != nil {
		t.Fatal(err)
	}
	if k2, err := ParseMinisignKey(string(pub)); err != nil || string(k2.PK) != string(k.PK) || k2.ID != k.ID {
		t.Fatal("lmget-index.pub is not the built-in key")
	}
}
