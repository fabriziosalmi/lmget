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
// at its source at the locked commit: the source must still publish exactly
// the locked files, the model must not be gated, and the license the source
// declares (not the lock's) must be on the allowlist.
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
		sel := Selection{AllowPickle: true}
		for _, f := range lm.Files {
			sel.Include = append(sel.Include, GlobEscape(f.Path))
		}
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
			case f.Size != lf.Size, f.SHA256 != "" && f.SHA256 != lf.SHA256, f.GitSHA1 != "" && lf.GitSHA1 != "" && f.GitSHA1 != lf.GitSHA1:
				return nil, refuse("%s differs between the lock and the source", lf.Path)
			}
		}
		if len(byPath) != len(lm.Files) {
			return nil, refuse("the source has files the lock does not list")
		}
		// files the source names only by git id: their sha256 in the lock is
		// checked by downloading them (they are the small ones) and verifying
		// both hashes, so a lock cannot put a wrong sha256 under the signature
		var gitOnly []LockFile
		for _, lf := range lm.Files {
			if byPath[lf.Path].SHA256 == "" || byPath[lf.Path].GitSHA1 != "" {
				gitOnly = append(gitOnly, lf)
			}
		}
		if len(gitOnly) > 0 {
			pin := lm
			pin.Files, pin.Include = gitOnly, nil
			gsel := Selection{AllowPickle: true}
			for _, lf := range gitOnly {
				pin.Include = append(pin.Include, GlobEscape(lf.Path))
			}
			gsel.Include = pin.Include
			if _, err := c.Pull(ctx, Request{Ref: ref, Selection: gsel, Target: Target{Kind: "none"}, Pin: &pin}, nil); err != nil {
				return nil, refuse("the locked sha256 of %d small files could not be confirmed: %s", len(gitOnly), AsError(err).Msg)
			}
		}
		ix.Models = append(ix.Models, IndexModel{Ref: ref.String(), Commit: lm.Commit, License: p.License, Files: lm.Files})
	}
	if err := ix.validate(); err != nil {
		return nil, newErr(ExitUsage, "index", "the index would not be valid").why("%s", err.Error())
	}
	return ix, nil
}
