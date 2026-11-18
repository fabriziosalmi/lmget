package lmget

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
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

// indexServer serves whatever index and signature the test sets, and a key
// list once one is set (404 before, as when none is published).
type indexServer struct {
	mu            sync.Mutex
	data, sig     []byte
	keys, keysSig []byte
	keysAsked     int
}

func (s *indexServer) setKeys(data []byte, sig string) {
	s.mu.Lock()
	s.keys, s.keysSig = data, []byte(sig)
	s.mu.Unlock()
}

func (s *indexServer) set(data []byte, sig string) {
	s.mu.Lock()
	s.data, s.sig = data, []byte(sig)
	s.mu.Unlock()
}

func (s *indexServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/keys.json") {
		s.keysAsked++
		switch {
		case s.keys == nil:
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, ".minisig"):
			w.Write(s.keysSig)
		default:
			w.Write(s.keys)
		}
		return
	}
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
	t.Setenv("LMGET_ROOT_KEY", "")
	saved, savedRoot := IndexKeys, RootKeys
	IndexKeys, RootKeys = nil, nil // a build without any key
	t.Cleanup(func() { IndexKeys, RootKeys = saved, savedRoot })
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

// The built-in keys are the ones minisign generated (the key number is
// printed reversed, as a little-endian uint64) and match the .pub files: the
// project key signs the index, the offline backup key is the root.
func TestBuiltInProjectKey(t *testing.T) {
	want := []struct {
		key, number, pubFile string
	}{
		{IndexKeys[0], "21C7BDA090E969F4", "lmget-index.pub"},
		{RootKeys[0], "736BC89C91E98921", "lmget-index-backup.pub"},
	}
	if len(IndexKeys) != 1 || len(RootKeys) != 1 {
		t.Fatalf("%d index keys and %d root keys built in, want 1 and 1", len(IndexKeys), len(RootKeys))
	}
	for i, w := range want {
		k, err := ParseMinisignKey(w.key)
		if err != nil {
			t.Fatal(err)
		}
		if KeyNumber(k) != w.number {
			t.Fatalf("key %d: number %s, want %s", i, KeyNumber(k), w.number)
		}
		pub, err := os.ReadFile(w.pubFile)
		if err != nil {
			t.Fatal(err)
		}
		if k2, err := ParseMinisignKey(string(pub)); err != nil || string(k2.PK) != string(k.PK) || k2.ID != k.ID {
			t.Fatalf("%s is not built-in key %s", w.pubFile, w.number)
		}
	}
}

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

// An index valid for more than MaxIndexValidity is refused, whatever its
// signature: a stolen key cannot sign an index that stays accepted for years.
func TestAnIndexValidForTooLongIsRefused(t *testing.T) {
	c, is, s := indexSetup(t)
	long := sampleIndex(1)
	long.Expires = long.Issued.Add(MaxIndexValidity + time.Hour)
	publish(is, s, long)
	if _, err := c.UpdateIndex(context.Background()); err == nil || !strings.Contains(AsError(err).Why, "days allowed") {
		t.Fatalf("an index valid for %s = %v; want a refusal", MaxIndexValidity+time.Hour, err)
	}
	ok := sampleIndex(2)
	ok.Expires = ok.Issued.Add(MaxIndexValidity - time.Hour)
	publish(is, s, ok)
	if _, err := c.UpdateIndex(context.Background()); err != nil {
		t.Fatalf("an index valid for just under the limit: %v", err)
	}
}

// Audit run 5: the 60-day cap holds against the clock. An index dated years
// ahead with a short window would otherwise stay valid for years.
func TestAnIndexDatedInTheFutureIsRefused(t *testing.T) {
	c, is, s := indexSetup(t)
	future := sampleIndex(1)
	future.Issued = time.Now().AddDate(5, 0, 0).UTC().Truncate(time.Second)
	future.Expires = future.Issued.Add(MaxIndexValidity - time.Hour)
	publish(is, s, future)
	if _, err := c.UpdateIndex(context.Background()); err == nil || !strings.Contains(err.Error(), "dated in the future") {
		t.Fatalf("an index issued %s = %v; want a refusal", future.Issued.Format(time.DateOnly), err)
	}
}

// Audit run 5: a crash after the new index is cached but before the highest
// record is written leaves the record one index behind; the previous index
// must still be refused.
func TestACrashBeforeTheHighestRecordCannotRollBack(t *testing.T) {
	c, is, s := indexSetup(t)
	old := publish(is, s, sampleIndex(5))
	if _, err := c.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	publish(is, s, sampleIndex(6))
	if _, err := c.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(old) // the record as the crash left it: still sequence 5
	if err := os.WriteFile(c.highestPath(), []byte(fmt.Sprintf("5 %x\n", sum)), 0o644); err != nil {
		t.Fatal(err)
	}
	is.set(old, s.sign(old))
	if _, err := c.UpdateIndex(context.Background()); err == nil || !strings.Contains(err.Error(), "older than one already accepted") {
		t.Fatalf("replay of sequence 5 after a crash at 6 = %v; want a refusal", err)
	}
}

// Found by the 2026-11-18 review: two lmget updating the index at once (one
// reading a node that still serves the previous index) could both pass the
// sequence check, and the older index was recorded last. B is held between
// its check and its writes while A runs: A must wait, and 6 must win.
func TestConcurrentIndexUpdatesKeepTheHighest(t *testing.T) {
	cA, isA, s := indexSetup(t)
	publish(isA, s, sampleIndex(4))
	if _, err := cA.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	publish(isA, s, sampleIndex(6))
	isB := &indexServer{}
	srvB := httptest.NewTLSServer(isB)
	defer srvB.Close()
	publish(isB, s, sampleIndex(5))
	cB, err := New(Options{Home: cA.store.Root(), HTTP: srvB.Client(), IndexURL: srvB.URL + "/index.json", IndexKeys: []string{s.pub}})
	if err != nil {
		t.Fatal(err)
	}
	aDone := make(chan error, 1)
	testHookIndexWrite = func(seq int64) {
		if seq != 5 {
			return
		}
		go func() { _, err := cA.UpdateIndex(context.Background()); aDone <- err }()
		select { // A either finishes now (no lock) or waits for B
		case <-aDone:
			aDone <- nil
		case <-time.After(500 * time.Millisecond):
		}
	}
	defer func() { testHookIndexWrite = nil }()
	if _, err := cB.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-aDone
	ix, err := cA.Index()
	if err != nil {
		t.Fatal(err)
	}
	if seq, _, _ := cA.highestIndex(); ix.Sequence != 6 || seq != 6 {
		t.Fatalf("after two concurrent updates the store holds index %d, highest %d; want 6", ix.Sequence, seq)
	}
}
