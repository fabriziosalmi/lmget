package lmget

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A node that fails is skipped for the rest of the run: measured on the
// real nodes, a dead first node cost every file its whole retry sequence
// (about 16 s each) before the next node was asked.
func TestADeadNodeIsAskedForOneFileOnly(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	dead := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[r.URL.Path]++
		mu.Unlock()
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer dead.Close()
	m := setupM3(t, dead.URL) // the index lists the dead node first
	u := m.client(t, t.TempDir())
	u.retryBase = time.Millisecond
	if _, err := u.UpdateIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.hf.down.Store(true)
	var from []string
	out, err := u.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "dir", Path: t.TempDir()}}, func(e Event) {
		if e.Type == EvFileDone {
			from = append(from, e.From)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Plan.FromIndex || len(from) != len(m.files) {
		t.Fatalf("from_index=%v, %d files done; want %d from the index", out.Plan.FromIndex, len(from), len(m.files))
	}
	good := NodeOrigin + strings.TrimPrefix(m.node.URL, "https://")
	for _, f := range from {
		if f != good {
			t.Fatalf("a file came from %q, want %q", f, good)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 {
		t.Fatalf("the dead node was asked for %d different blobs (%v); want 1, then skipped", len(asked), asked)
	}
}
