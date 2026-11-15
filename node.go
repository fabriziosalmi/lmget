package lmget

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"
)

// A node (lmget-node) mirrors the models of the signed index into its own
// store and serves them over HTTP(S) by sha256, together with the signed
// index itself. It serves only blobs the verified index names: nothing else
// the operator's store may hold.

// MirrorResult reports one model of a mirror run.
type MirrorResult struct {
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
	Err    string `json:"error,omitempty"`
}

// MirrorIndex pulls every model of the cached, verified index into the store,
// pinned to the indexed commit and files: the source must still match the
// index, or the model is refused as drift. Models whose license is not on
// the allowlist are skipped.
func (c *Client) MirrorIndex(ctx context.Context, emit func(Event)) ([]MirrorResult, error) {
	ix, err := c.Index()
	if err != nil {
		return nil, err
	}
	var out []MirrorResult
	for _, m := range ix.Models {
		r := MirrorResult{Ref: m.Ref, Commit: m.Commit}
		ref, err := ParseRef(m.Ref)
		switch {
		case err != nil:
			r.Err = err.Error()
		case !LicenseAllowed(m.License):
			r.Err = "license " + quote(m.License) + " is not on the allowlist"
		default:
			sel := Selection{AllowPickle: true}
			for _, f := range m.Files {
				sel.Include = append(sel.Include, GlobEscape(f.Path))
			}
			pin := LockModel{Ref: m.Ref, Commit: m.Commit, To: "none", License: m.License, Include: sel.Include, AllowPickle: true, Files: m.LockFiles()}
			if _, err := c.Pull(ctx, Request{Ref: ref, Selection: sel, Target: Target{Kind: "none"}, Pin: &pin}, emit); err != nil {
				r.Err = AsError(err).Error()
			}
		}
		out = append(out, r)
	}
	return out, nil
}

var blobPath = regexp.MustCompile(`^/blobs/sha256/([0-9a-f]{64})$`)

// NodeHandler serves GET and HEAD for /index.json, /index.json.minisig and
// /blobs/sha256/<hex> (with ranges) for the blobs the verified index names.
func (c *Client) NodeHandler() http.Handler {
	return &nodeHandler{c: c}
}

type nodeHandler struct {
	c    *Client
	mu   sync.Mutex
	snap *nodeSnapshot
}

// nodeSnapshot is the cached index as verified once: its bytes and
// signature (served together, so they always match) and the blobs it names
// with their sizes. It is rebuilt when the cached file changes and dropped
// when the index expires.
type nodeSnapshot struct {
	stamp     time.Time // mtime of the cached index file
	expires   time.Time
	data, sig []byte
	blobs     map[string]int64 // sha256 -> size, models on the license allowlist only
}

func (h *nodeHandler) snapshot() (*nodeSnapshot, error) {
	p, ps := h.c.indexPaths()
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.snap; s != nil && fi.ModTime().Equal(s.stamp) && time.Now().Before(s.expires) {
		return s, nil
	}
	h.snap = nil
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	sig, err := os.ReadFile(ps)
	if err != nil {
		return nil, err
	}
	ix, err := h.c.openIndex(data, string(sig), time.Now()) // a pair read mid-update fails here: 503, then fine
	if err != nil {
		return nil, err
	}
	s := &nodeSnapshot{stamp: fi.ModTime(), expires: ix.Expires, data: data, sig: sig, blobs: map[string]int64{}}
	for _, m := range ix.Models {
		if !LicenseAllowed(m.License) { // as mirror and seed
			continue
		}
		for _, f := range m.Files {
			s.blobs[f.SHA256] = f.Size
		}
	}
	h.snap = s
	return s, nil
}

func (h *nodeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snap, err := h.snapshot()
	if err != nil { // never serve anything without an index that verifies
		http.Error(w, "no valid index", http.StatusServiceUnavailable)
		return
	}
	switch r.URL.Path {
	case "/index.json", "/index.json.minisig":
		body, ctype := snap.data, "application/json"
		if r.URL.Path == "/index.json.minisig" {
			body, ctype = snap.sig, "text/plain; charset=utf-8"
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "", snap.stamp, bytes.NewReader(body))
		return
	}
	m := blobPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	size, ok := snap.blobs[m[1]]
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(h.c.store.BlobPath(m[1]))
	if err != nil {
		http.NotFound(w, r) // named by the index, not mirrored (yet)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != size {
		http.NotFound(w, r) // not mirrored, or not the signed size: clients would refuse it anyway
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, m[1]))
	http.ServeContent(w, r, "", time.Time{}, f)
}
