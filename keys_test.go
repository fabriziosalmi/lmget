package lmget

import (
	"context"
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
