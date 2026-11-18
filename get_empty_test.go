package lmget

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// Found by the 2026-11-18 review: an empty repository was reported as "no
// files selected, adjust --include / --exclude", though nothing was there.
func TestAnEmptyRepositorySaysSo(t *testing.T) {
	f := &fakeHF{files: []fakeFile{}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	_, err := c.Pull(context.Background(), Request{Ref: hfRef("org/empty"), Target: Target{Kind: "none"}}, nil)
	e := AsError(err)
	if e == nil || e.Code != ExitNotFound || !strings.Contains(e.Msg, "has no files") {
		t.Fatalf("got %v", err)
	}
	// a selection that leaves nothing is still the selection's doing
	f2 := &fakeHF{files: modelFiles()}
	srv2 := httptest.NewServer(f2.handler())
	defer srv2.Close()
	_, err = newTestClient(t, srv2, t.TempDir()).Pull(context.Background(),
		Request{Ref: hfRef("org/m"), Selection: Selection{Include: []string{"*.nothing"}}, Target: Target{Kind: "none"}}, nil)
	if e := AsError(err); e == nil || e.Code != ExitUsage || !strings.Contains(e.Msg, "no files selected") {
		t.Fatalf("an empty selection: %v", err)
	}
}
