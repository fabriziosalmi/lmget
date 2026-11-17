package lmget

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// allowedLicenses are the licenses whose models the project nodes may
// redistribute. A model is mirrored only if every license it declares is
// here, it is not gated, and a maintainer reviewed it (no NSFW content).
var allowedLicenses = map[string]bool{
	"apache-2.0": true, "mit": true, "bsd-2-clause": true, "bsd-3-clause": true,
	"isc": true, "cc0-1.0": true, "cc-by-4.0": true, "unlicense": true,
}

// LicenseAllowed reports whether every license in s (comma separated, as
// Hugging Face lists several) is on the allowlist.
func LicenseAllowed(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	for _, l := range strings.Split(s, ",") {
		if !allowedLicenses[strings.ToLower(strings.TrimSpace(l))] {
			return false
		}
	}
	return true
}

// BuildOptions configure BuildIndex.
type BuildOptions struct {
	Sequence int64
	Valid    time.Duration // how long the index stays valid
	Nodes    []string
	Now      time.Time
}

// BuildIndex writes an index from a lockfile. Every model is resolved again
// at its source at the locked commit: the lock must list every file the
// source publishes there (pickle weights next to safetensors only with
// allow_pickle), with the source's own size, sha256 and git id, the model
// must not be gated, and the license the source declares (not the lock's)
// must be on the allowlist. An entry is a whole model, never a selection: a
// client that falls back to the index applies its own selection to it.
func (c *Client) BuildIndex(ctx context.Context, l *Lock, o BuildOptions) (*Index, error) {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	ix := &Index{Version: indexVersion, Sequence: o.Sequence, Issued: o.Now.UTC().Truncate(time.Second),
		Expires: o.Now.Add(o.Valid).UTC().Truncate(time.Second), Nodes: o.Nodes}
	for _, lm := range l.Models {
		ref, err := ParseRef(lm.Ref)
		if err != nil {
			return nil, err
		}
		refuse := func(format string, a ...any) error {
			return newErr(ExitUsage, "index", ref.Display()+": "+fmt.Sprintf(format, a...))
		}
		sel := Selection{AllowPickle: lm.AllowPickle}
		at := ref
		at.Revision = lm.Commit
		p, err := c.Resolve(ctx, at, sel)
		if err != nil {
			return nil, err
		}
		switch {
		case p.Commit != lm.Commit:
			return nil, refuse("the source resolves to %s, the lock has %s", p.Commit[:7], lm.Commit[:7])
		case p.Gated != "":
			return nil, refuse("gated models are never mirrored")
		case !LicenseAllowed(p.License):
			return nil, refuse("license %s is not on the allowlist", quote(p.License))
		case lm.License != "" && lm.License != p.License:
			return nil, refuse("the license changed: %s in the lock, %s at the source; lock the model again after reading it", quote(lm.License), quote(p.License))
		}
		byPath := map[string]File{}
		for _, f := range p.Selected() {
			byPath[f.Path] = f
		}
		for _, lf := range lm.Files {
			f, ok := byPath[lf.Path]
			switch {
			case !ok:
				return nil, refuse("%s is in the lock but not at the source", lf.Path)
			// the git id too: it names the blob in the HF cache, so a lock
			// must not sign one the source never published (LFS files: none)
			case f.Size != lf.Size, f.SHA256 != "" && f.SHA256 != lf.SHA256, f.GitSHA1 != lf.GitSHA1:
				return nil, refuse("%s differs between the lock and the source", lf.Path)
			}
		}
		if len(byPath) != len(lm.Files) {
			return nil, refuse("the source has %d files at %s, the lock lists %d: the index mirrors whole models",
				len(byPath), lm.Commit[:7], len(lm.Files))
		}
		// every file into the builder's store, pinned to the lock: this
		// verifies each one against the source (and the small git-id files
		// against the lock's sha256, so a lock cannot put a wrong hash under
		// the signature), and gives the bytes the torrent identity needs
		pin := lm
		pin.Include, pin.Exclude = nil, nil
		if _, err := c.Pull(ctx, Request{Ref: ref, Selection: sel, Target: Target{Kind: "none"}, Pin: &pin}, nil); err != nil {
			return nil, refuse("the locked files could not be confirmed at the source: %s", AsError(err).Msg)
		}
		entry := IndexModel{Ref: ref.String(), Commit: lm.Commit, License: p.License}
		for _, lf := range lm.Files {
			bt, err := NewBlobTorrent(c.store.BlobPath(lf.SHA256), lf.SHA256)
			if err != nil {
				return nil, refuse("%s: %v", lf.Path, err)
			}
			entry.Files = append(entry.Files, IndexFile{LockFile: lf, BTIH2: bt.Infohash})
		}
		ix.Models = append(ix.Models, entry)
	}
	if err := ix.validate(); err != nil {
		return nil, newErr(ExitUsage, "index", "the index would not be valid").why("%s", err.Error())
	}
	return ix, nil
}
