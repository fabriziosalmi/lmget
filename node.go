package lmget

import (
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
			pin := LockModel{Ref: m.Ref, Commit: m.Commit, To: "none", License: m.License, Include: sel.Include, AllowPickle: true, Files: m.Files}
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
	h := &nodeHandler{c: c}
	return h
}

type nodeHandler struct {
	c       *Client
	mu      sync.Mutex
	stamp   time.Time // mtime of the cached index the set was built from
	allowed map[string]bool
}

// blobs returns the hashes the current verified index names, rebuilt when
// the cached index file changes.
func (h *nodeHandler) blobs() (map[string]bool, error) {
	p, _ := h.c.indexPaths()
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.allowed != nil && fi.ModTime().Equal(h.stamp) {
		return h.allowed, nil
	}
	ix, err := h.c.Index()
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, m := range ix.Models {
		for _, f := range m.Files {
			set[f.SHA256] = true
		}
	}
	h.allowed, h.stamp = set, fi.ModTime()
	return set, nil
}

func (h *nodeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p, ps := h.c.indexPaths()
	switch r.URL.Path {
	case "/index.json", "/index.json.minisig":
		if _, err := h.c.Index(); err != nil { // never serve an index that does not verify
			http.Error(w, "no valid index", http.StatusServiceUnavailable)
			return
		}
		file := p
		if r.URL.Path == "/index.json.minisig" {
			file = ps
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, file)
		return
	}
	m := blobPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	allowed, err := h.blobs()
	if err != nil {
		http.Error(w, "no valid index", http.StatusServiceUnavailable)
		return
	}
	if !allowed[m[1]] {
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
	if err != nil || !fi.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, m[1]))
	http.ServeContent(w, r, "", time.Time{}, f)
}
