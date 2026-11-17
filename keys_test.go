package lmget

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// rotation sets up a store whose built-in keys are test keys: root r signs
// key lists, k1 is the built-in index key, k2 the key a list rotates to.
type rotation struct {
	c         *Client
	is        *indexServer
	r, k1, k2 signer
}

func rotationSetup(t *testing.T) *rotation {
	t.Helper()
	t.Setenv("LMGET_INDEX_KEY", "")
	t.Setenv("LMGET_ROOT_KEY", "")
	rt := &rotation{is: &indexServer{}, r: testSigner(t), k1: testSigner(t), k2: testSigner(t)}
	savedI, savedR, savedV := IndexKeys, RootKeys, RevokedKeys
	IndexKeys, RootKeys, RevokedKeys = []string{rt.k1.pub}, []string{rt.r.pub}, nil
	t.Cleanup(func() { IndexKeys, RootKeys, RevokedKeys = savedI, savedR, savedV })
	srv := httptest.NewTLSServer(rt.is)
	t.Cleanup(srv.Close)
	c, err := New(Options{Home: t.TempDir(), HTTP: srv.Client(), IndexURL: srv.URL + "/index.json"})
	if err != nil {
		t.Fatal(err)
	}
	rt.c = c
	return rt
}

func keyList(seq int64, keys []signer, revoked ...signer) KeyList {
	kl := KeyList{Version: 1, Sequence: seq, Issued: time.Now().Add(-time.Hour).UTC().Truncate(time.Second),
		Expires: time.Now().Add(300 * 24 * time.Hour).UTC().Truncate(time.Second), Revoked: []string{}}
	for _, k := range keys {
		kl.IndexKeys = append(kl.IndexKeys, k.pub)
	}
	for _, k := range revoked {
		kl.Revoked = append(kl.Revoked, KeyNumber(k.key))
	}
	return kl
}

func (rt *rotation) publishKeys(by signer, kl KeyList) {
	b, _ := json.Marshal(kl)
	rt.is.setKeys(b, by.sign(b))
}

// update publishes an index of seq signed by s and fetches it.
func (rt *rotation) update(s signer, seq int64) error {
	publish(rt.is, s, sampleIndex(seq))
	_, err := rt.c.UpdateIndex(context.Background())
	return err
}

func TestAKeyListRotatesTheIndexKey(t *testing.T) {
	rt := rotationSetup(t)
	if err := rt.update(rt.k1, 1); err != nil {
		t.Fatalf("with no key list, the built-in key was refused: %v", err)
	}
	if rt.is.keysAsked == 0 {
		t.Fatal("the key list was not asked for")
	}
	rt.publishKeys(rt.r, keyList(1, []signer{rt.k2}, rt.k1))
	if err := rt.update(rt.k1, 2); codeOf(err) != ExitVerify || !strings.Contains(fmtWhy(err), KeyNumber(rt.k1.key)+", which is revoked") {
		t.Fatalf("an index signed by the revoked key: %v (%s)", err, fmtWhy(err))
	}
	if err := rt.update(rt.k2, 2); err != nil {
		t.Fatalf("an index signed by the new key: %v", err)
	}
	if _, err := rt.c.Index(); err != nil {
		t.Fatalf("the cached index does not verify with the new key: %v", err)
	}
	st, err := rt.c.IndexKeyState()
	if err != nil || st.List == nil || st.List.Sequence != 1 || len(st.Keys) != 1 || st.Keys[0] != KeyNumber(rt.k2.key) {
		t.Fatalf("key state %+v, %v", st, err)
	}
	// a key the list does not name is not trusted, even without a revocation
	k3 := testSigner(t)
	if err := rt.update(k3, 3); codeOf(err) != ExitVerify {
		t.Fatalf("an index signed by an unlisted key: %v", err)
	}
	// the list replaces the built-in keys: k1 is out without being revoked
	fresh := rotationSetup(t)
	fresh.publishKeys(fresh.r, keyList(1, []signer{fresh.k2}))
	if err := fresh.update(fresh.k1, 1); codeOf(err) != ExitVerify {
		t.Fatalf("a built-in key the list does not name: %v", err)
	}
}

// once seen, a revocation holds after the list expires or is deleted, and
// with the built-in key revoked nothing verifies until a current list comes
func TestARevocationOutlivesItsKeyList(t *testing.T) {
	rt := rotationSetup(t)
	rt.publishKeys(rt.r, keyList(1, []signer{rt.k2}, rt.k1))
	if err := rt.update(rt.k2, 1); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(MaxKeyListValidity)
	if _, _, err := rt.c.trustedIndexKeys(later); codeOf(err) != ExitVerify {
		t.Fatalf("after the list expired, the revoked built-in key: %v", err)
	}
	p, ps, _, _ := rt.c.keyPaths()
	os.Remove(p)
	os.Remove(ps)
	rt.is.setKeys(nil, "") // and none is published any more
	if err := rt.update(rt.k1, 2); codeOf(err) != ExitVerify {
		t.Fatalf("with the list gone, an index signed by the revoked key: %v", err)
	}
	// an expired list served still records its revocations
	rt2 := rotationSetup(t)
	old := keyList(1, []signer{rt2.k2}, rt2.k1)
	old.Issued, old.Expires = time.Now().Add(-48*time.Hour).UTC(), time.Now().Add(-time.Hour).UTC()
	rt2.publishKeys(rt2.r, old)
	if err := rt2.update(rt2.k1, 1); codeOf(err) != ExitVerify {
		t.Fatalf("an expired list's revocation was ignored: %v", err)
	}
}

func TestAKeyListCannotRollBack(t *testing.T) {
	rt := rotationSetup(t)
	rt.publishKeys(rt.r, keyList(2, []signer{rt.k2}, rt.k1))
	if err := rt.update(rt.k2, 1); err != nil {
		t.Fatal(err)
	}
	rt.publishKeys(rt.r, keyList(1, []signer{rt.k1}))
	if err := rt.update(rt.k1, 2); codeOf(err) != ExitVerify || !strings.Contains(err.Error(), "older") {
		t.Fatalf("an older key list bringing the revoked key back: %v", err)
	}
	other := keyList(2, []signer{rt.k2, rt.k1})
	rt.publishKeys(rt.r, other)
	if err := rt.update(rt.k2, 2); codeOf(err) != ExitVerify {
		t.Fatalf("another list with the same sequence: %v", err)
	}
}

func TestOnlyTheRootSignsKeyListsAndNeverTheIndex(t *testing.T) {
	rt := rotationSetup(t)
	rt.publishKeys(rt.k1, keyList(1, []signer{rt.k2})) // the index key cannot name its successor
	if err := rt.update(rt.k1, 1); codeOf(err) != ExitVerify {
		t.Fatalf("a key list signed by an index key: %v", err)
	}
	rt.is.setKeys(nil, "")
	if err := rt.update(rt.r, 1); codeOf(err) != ExitVerify || !strings.Contains(fmtWhy(err), "signs key lists only") {
		t.Fatalf("an index signed by the root key: %v (%s)", err, fmtWhy(err))
	}
	// the user's own index keys are trusted, the root key never
	rt.c.opt.IndexKeys = []string{rt.r.pub}
	if err := rt.update(rt.r, 1); codeOf(err) != ExitVerify {
		t.Fatalf("an index signed by the root key configured as an index key: %v", err)
	}
}

func TestARevocationBuiltIntoTheRelease(t *testing.T) {
	rt := rotationSetup(t)
	RevokedKeys = []string{KeyNumber(rt.k1.key)}
	if err := rt.update(rt.k1, 1); codeOf(err) != ExitVerify {
		t.Fatalf("an index signed by a key this release revokes: %v", err)
	}
}

// an unreachable key list leaves the keys in force; a broken one stops
func TestAnUnreachableKeyListIsNotFatal(t *testing.T) {
	rt := rotationSetup(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/keys.json") {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		rt.is.ServeHTTP(w, r)
	}))
	defer srv.Close()
	rt.c.opt.IndexURL, rt.c.http = srv.URL+"/index.json", srv.Client()
	if err := rt.update(rt.k1, 1); err != nil {
		t.Fatalf("a key list that cannot be reached stopped the index: %v", err)
	}
	rt.c.opt.IndexURL = rt2url(t, rt)
	b := []byte(`{"version":1}`)
	rt.is.setKeys(b, rt.r.sign(b))
	if err := rt.update(rt.k1, 2); codeOf(err) != ExitVerify {
		t.Fatalf("a signed but invalid key list: %v", err)
	}
}

func rt2url(t *testing.T, rt *rotation) string {
	srv := httptest.NewTLSServer(rt.is)
	t.Cleanup(srv.Close)
	rt.c.http = srv.Client()
	return srv.URL + "/index.json"
}

func TestCheckKeyList(t *testing.T) {
	r, k1, k2 := testSigner(t), testSigner(t), testSigner(t)
	roots := []string{r.pub}
	if err := CheckKeyList(ptr(keyList(1, []signer{k1, k2})), roots); err != nil {
		t.Fatalf("a valid list: %v", err)
	}
	bad := map[string]KeyList{
		"no keys":            keyList(1, nil),
		"the root as a key":  keyList(1, []signer{r}),
		"the root revoked":   keyList(1, []signer{k1}, r),
		"listed and revoked": keyList(1, []signer{k1}, k1),
		"listed twice":       keyList(1, []signer{k1, k1}),
		"no sequence":        keyList(0, []signer{k1}),
	}
	long := keyList(1, []signer{k1})
	long.Expires = long.Issued.Add(MaxKeyListValidity + time.Hour)
	bad["valid too long"] = long
	num := keyList(1, []signer{k1})
	num.Revoked = []string{"21c7bda090e969f4"}
	bad["a lower-case key number"] = num
	for name, kl := range bad {
		if err := CheckKeyList(&kl, roots); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// codeOf is the exit code of err, 0 for none.
func codeOf(err error) ExitCode {
	if err == nil {
		return 0
	}
	return AsError(err).Code
}

// revokedIndex is sampleIndex(seq) revoking one blob.
func revokedIndex(seq int64) Index {
	ix := sampleIndex(seq)
	ix.Revoked = []IndexRevoked{{SHA256: strings.Repeat("c", 64), Reason: "withdrawn"}}
	return ix
}

func (rt *rotation) revokedCount(t *testing.T) int {
	t.Helper()
	rb, err := rt.c.revokedBlobs()
	if err != nil {
		t.Fatal(err)
	}
	return len(rb)
}

// Audit run 6: the revocations of an accepted index are accepted state; a
// key rotation, a revoked signer or an expired key list does not erase them.
func TestRevocationsSurviveKeyChanges(t *testing.T) {
	// the signer leaves the key list while the host still serves its index
	rt := rotationSetup(t)
	publish(rt.is, rt.k1, revokedIndex(1))
	if _, err := rt.c.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	rt.publishKeys(rt.r, keyList(1, []signer{rt.k2}, rt.k1))
	if _, err := rt.c.UpdateIndex(context.Background()); codeOf(err) != ExitVerify {
		t.Fatalf("the index of the revoked signer: %v", err)
	}
	if n := rt.revokedCount(t); n != 1 {
		t.Fatalf("after the signer was revoked, %d revoked blobs are known", n)
	}
	// the key list in force expires
	rt2 := rotationSetup(t)
	kl := keyList(1, []signer{rt2.k2})
	kl.Expires = time.Now().Add(2 * time.Second).UTC()
	rt2.publishKeys(rt2.r, kl)
	publish(rt2.is, rt2.k2, revokedIndex(1))
	if _, err := rt2.c.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(kl.Expires) + 100*time.Millisecond)
	if n := rt2.revokedCount(t); n != 1 {
		t.Fatalf("after the key list expired, %d revoked blobs are known", n)
	}
	// and a newer accepted index that withdraws the revocation releases it
	rt3 := rotationSetup(t)
	publish(rt3.is, rt3.k1, revokedIndex(1))
	rt3.c.UpdateIndex(context.Background())
	if err := rt3.update(rt3.k1, 2); err != nil {
		t.Fatal(err)
	}
	if n := rt3.revokedCount(t); n != 0 {
		t.Fatalf("a withdrawn revocation is still applied (%d)", n)
	}
	// a damaged record is an error, never an empty list
	os.WriteFile(rt3.c.revokedBlobsPath(), []byte("garbage\n"), 0o644)
	if _, err := rt3.c.revokedBlobs(); kindOf(err) != "revoked" {
		t.Fatalf("a damaged record: %v", err)
	}
}

// Audit run 6: the highest record is written before the data, so a failure
// after it leaves the floor ahead of the cache: the previous index stays
// refused even when the cached copy is gone or no longer verifies.
func TestAFailedIndexWriteCannotLetThePreviousIndexBack(t *testing.T) {
	rt := rotationSetup(t)
	old := publish(rt.is, rt.k1, sampleIndex(5))
	oldSig := rt.k1.sign(old)
	if _, err := rt.c.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, _ := rt.c.indexPaths()
	os.Remove(p)
	os.Mkdir(p, 0o755) // the data cannot be written: a crash at that point
	if err := rt.update(rt.k1, 6); err == nil {
		t.Fatal("the index was written over a directory")
	}
	os.Remove(p)
	rt.is.set(old, oldSig) // the host replays the previous index
	if _, err := rt.c.UpdateIndex(context.Background()); codeOf(err) != ExitVerify || !strings.Contains(err.Error(), "older") {
		t.Fatalf("the previous index after a failed write: %v", err)
	}
	if err := rt.update(rt.k1, 6); err != nil {
		t.Fatalf("the genuine newer index is refused: %v", err)
	}
}

// Audit run 6: once a key list was accepted, the built-in keys are not
// trusted again, and a list older than the highest accepted is not in force.
func TestNoFallbackToBuiltInKeysAfterAKeyList(t *testing.T) {
	rt := rotationSetup(t)
	rt.publishKeys(rt.r, keyList(2, []signer{rt.k1}))
	if err := rt.update(rt.k1, 1); err != nil {
		t.Fatal(err)
	}
	// a newer list arrives already expired: it is recorded, the older one
	// it replaces is no longer in force, and the built-in k1 does not return
	expired := keyList(3, []signer{rt.k2})
	expired.Issued, expired.Expires = time.Now().Add(-48*time.Hour).UTC(), time.Now().Add(-time.Hour).UTC()
	rt.publishKeys(rt.r, expired)
	if err := rt.update(rt.k1, 2); codeOf(err) != ExitVerify || !strings.Contains(err.Error(), "no key list is in force") {
		t.Fatalf("with only an expired newer list: %v", err)
	}
}

// Audit run 6: a root key is recognised by its public key, whatever key
// number a copy carries.
func TestARootKeyUnderAnotherNumberIsStillTheRoot(t *testing.T) {
	r, k1 := testSigner(t), testSigner(t)
	b, _ := base64.StdEncoding.DecodeString(r.pub)
	b[2] ^= 0xff // another key number, the same public key
	alias := base64.StdEncoding.EncodeToString(b)
	kl := keyList(1, []signer{k1})
	kl.IndexKeys = append(kl.IndexKeys, alias)
	if err := CheckKeyList(&kl, []string{r.pub}); err == nil || !strings.Contains(err.Error(), "root key") {
		t.Fatalf("a list naming the root under another number: %v", err)
	}
	rt := rotationSetup(t)
	ab, _ := base64.StdEncoding.DecodeString(rt.r.pub)
	ab[2] ^= 0xff
	rt.c.opt.IndexKeys = []string{base64.StdEncoding.EncodeToString(ab)}
	keys, _, _ := rt.c.trustedIndexKeys(time.Now())
	for _, k := range keys {
		if bytes.Equal(k.PK, rt.r.key.PK) {
			t.Fatal("the root key is trusted as an index key under another number")
		}
	}
}
