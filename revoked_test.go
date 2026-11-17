package lmget

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// revokedSetup: the m3 fixture plus a second model, org/evil, served by the
// source and not indexed; publishRevoked signs an index that revokes blobs.
func revokedSetup(t *testing.T) (*m3, *fakeHF, []fakeFile) {
	t.Helper()
	m := setupM3(t)
	evil := []fakeFile{
		{path: "model.safetensors", data: fakeSafetensors(1 << 20), lfs: true},
		{path: "config.json", data: []byte(`{"evil":true}`)},
	}
	f := &fakeHF{files: m.files, byRepo: map[string][]fakeFile{"org/evil": evil}}
	m.hf.h = f.handler()
	return m, f, evil
}

func publishRevoked(t *testing.T, m *m3, seq int64, shas ...string) {
	t.Helper()
	b := m.client(t, t.TempDir())
	ix, err := b.BuildIndex(context.Background(), m.lock, BuildOptions{Sequence: seq, Valid: 24 * time.Hour, Nodes: []string{m.node.URL},
		Revoked: func() (out []IndexRevoked) {
			for _, s := range shas {
				out = append(out, IndexRevoked{SHA256: s, Reason: "malicious pickle in a renamed file"})
			}
			return
		}()})
	if err != nil {
		t.Fatal(err)
	}
	publish(m.idx, m.signer, *ix)
}

func evilPull(c *Client, dry bool) error {
	_, err := c.Pull(context.Background(), Request{Ref: hfRef("org/evil"), Target: Target{Kind: "none"}, DryRun: dry}, nil)
	return err
}

// a revoked blob is refused before any byte of it is downloaded, by get and
// by info, whatever source serves it
func TestARevokedBlobIsNeverDownloaded(t *testing.T) {
	m, f, evil := revokedSetup(t)
	publishRevoked(t, m, 2, sha(evil[0].data))
	u := m.client(t, t.TempDir())
	if _, err := u.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		before := f.served.Load()
		err := evilPull(u, dry)
		if e := AsError(err); err == nil || e.Code != ExitVerify || e.Kind != "revoked" || !strings.Contains(e.Why, "malicious pickle") {
			t.Fatalf("dry=%v: got %v", dry, err)
		}
		if n := f.served.Load() - before; n != 0 {
			t.Fatalf("dry=%v: %d bytes downloaded", dry, n)
		}
	}
	if u.store.Has(sha(evil[0].data), int64(len(evil[0].data))) {
		t.Fatal("the revoked blob is in the store")
	}
}

// a revocation still holds once its index expired
func TestARevocationOutlivesItsIndex(t *testing.T) {
	m, _, evil := revokedSetup(t)
	b := m.client(t, t.TempDir())
	ix, err := b.BuildIndex(context.Background(), m.lock, BuildOptions{Sequence: 2, Valid: time.Hour, Nodes: []string{m.node.URL},
		Now:     time.Now().Add(-2 * time.Hour),
		Revoked: []IndexRevoked{{SHA256: sha(evil[0].data), Reason: "malicious"}}})
	if err != nil {
		t.Fatal(err)
	}
	u := m.client(t, t.TempDir())
	// cached as UpdateIndex would have when it was current
	data, _ := json.Marshal(ix)
	p, ps := u.indexPaths()
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(ps, []byte(m.signer.sign(data)), 0o644)
	os.WriteFile(p, data, 0o644)
	if _, err := u.Index(); err == nil {
		t.Fatal("the test index has not expired")
	}
	if err := evilPull(u, false); kindOf(err) != "revoked" {
		t.Fatalf("after its index expired, the revoked blob: %v", err)
	}
	// a cached index whose signature does not verify revokes nothing
	os.WriteFile(p, append(data, ' '), 0o644)
	if rb, _ := u.revokedBlobs(); len(rb) != 0 {
		t.Fatal("an unsigned revocation list was used")
	}
}

// a file the source publishes with a git id only is refused once its
// sha256 is known, before it enters the store
func TestARevokedGitOnlyFileNeverEntersTheStore(t *testing.T) {
	m, _, evil := revokedSetup(t)
	publishRevoked(t, m, 2, sha(evil[1].data))
	u := m.client(t, t.TempDir())
	if _, err := u.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := evilPull(u, false); kindOf(err) != "revoked" {
		t.Fatalf("got %v", err)
	}
	if u.store.Has(sha(evil[1].data), int64(len(evil[1].data))) {
		t.Fatal("the revoked file entered the store")
	}
}

// bytes stored before the revocation are refused afterwards, and doctor
// names the model that uses them
func TestARevocationReachesBlobsAlreadyStored(t *testing.T) {
	m, _, evil := revokedSetup(t)
	u := m.client(t, t.TempDir())
	if err := evilPull(u, false); err != nil {
		t.Fatal(err)
	}
	publishRevoked(t, m, 2, sha(evil[0].data))
	if _, err := u.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := evilPull(u, false); kindOf(err) != "revoked" {
		t.Fatalf("a stored revoked blob was exported again: %v", err)
	}
	var got *Check
	for _, ch := range u.Doctor(context.Background(), DoctorOptions{}) {
		if ch.Name == "revoked" {
			got = &ch
		}
	}
	if got == nil || got.Status != "fail" || !strings.Contains(got.Detail, "org/evil") {
		t.Fatalf("doctor: %+v", got)
	}
}

// the index never both serves and revokes a blob
func TestAnIndexCannotListARevokedBlob(t *testing.T) {
	m, _, _ := revokedSetup(t)
	b := m.client(t, t.TempDir())
	w := m.lock.Models[0].Files[0].SHA256
	_, err := b.BuildIndex(context.Background(), m.lock, BuildOptions{Sequence: 2, Valid: time.Hour, Nodes: []string{m.node.URL},
		Revoked: []IndexRevoked{{SHA256: w, Reason: "x"}}})
	if err == nil || !strings.Contains(err.Error()+AsError(err).Why, "revoked") {
		t.Fatalf("the builder indexed a model using a revoked blob: %v", err)
	}
	ix := sampleIndex(1)
	ix.Revoked = []IndexRevoked{{SHA256: ix.Models[0].Files[0].SHA256, Reason: "x"}}
	if err := ix.validate(); err == nil {
		t.Fatal("an index listing and revoking the same blob validated")
	}
	for _, bad := range []IndexRevoked{{SHA256: "abc", Reason: "x"}, {SHA256: strings.Repeat("c", 64)}, {SHA256: strings.Repeat("c", 64), Reason: "a\x1b[31m"}} {
		ix := sampleIndex(1)
		ix.Revoked = []IndexRevoked{bad}
		if err := ix.validate(); err == nil {
			t.Errorf("revoked entry %+v validated", bad)
		}
	}
}

// kindOf is the kind of err, "" for none.
func kindOf(err error) string {
	if err == nil {
		return ""
	}
	return AsError(err).Kind
}

// Audit run 6: the library's Export refuses revoked bytes as Pull does.
func TestExportRefusesARevokedBlob(t *testing.T) {
	m, _, evil := revokedSetup(t)
	u := m.client(t, t.TempDir())
	out, err := u.Pull(context.Background(), Request{Ref: hfRef("org/evil"), Target: Target{Kind: "none"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	publishRevoked(t, m, 2, sha(evil[0].data))
	if _, err := u.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Export(out.Plan, Target{Kind: "dir", Path: t.TempDir()}); kindOf(err) != "revoked" {
		t.Fatalf("Export of a revoked blob: %v", err)
	}
}
