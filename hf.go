package lmget

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

type hfModelInfo struct {
	ID       string `json:"id"`
	Sha      string `json:"sha"`
	Gated    any    `json:"gated"`
	Disabled bool   `json:"disabled"`
	CardData struct {
		License any `json:"license"`
	} `json:"cardData"`
}

type hfTreeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	Oid  string `json:"oid"`
	Lfs  *struct {
		Oid  string `json:"oid"`
		Size int64  `json:"size"`
	} `json:"lfs"`
}

// Resolve turns a Ref into a Plan: commit, license, files with hashes, and
// what the store already holds. Nothing is downloaded.
func (c *Client) Resolve(ctx context.Context, ref Ref, sel Selection) (*Plan, error) {
	switch ref.Source {
	case "url":
		return c.resolveURL(ctx, ref, sel)
	case "ollama":
		return c.resolveOllama(ctx, ref, sel)
	case "oci":
		return c.resolveOCI(ctx, ref, sel)
	}
	if ref.File != "" { // one file of the repository
		sel.Include = []string{GlobEscape(ref.File)}
	}
	if ref.Source != "hf" {
		return nil, newErr(ExitUsage, "unsupported", "source "+quote(ref.Source)+" is not supported yet")
	}
	var info hfModelInfo
	u := fmt.Sprintf("%s/api/models/%s/revision/%s", c.opt.HFEndpoint, ref.Repo, url.PathEscape(ref.Revision))
	if err := c.hfJSON(ctx, u, ref, &info, nil); err != nil {
		return nil, err
	}
	if !hex40.MatchString(info.Sha) {
		return nil, newErr(ExitFailure, "source", "Hugging Face returned no valid commit for "+ref.Display())
	}
	p := &Plan{Ref: ref, Commit: info.Sha, License: licenseString(info.CardData.License)}
	if g, ok := info.Gated.(string); ok && g != "" {
		p.Gated = printable(g)
	}
	next := fmt.Sprintf("%s/api/models/%s/tree/%s?recursive=true", c.opt.HFEndpoint, ref.Repo, info.Sha)
	seen := map[string]bool{}
	for next != "" {
		if seen[next] || len(seen) >= maxTreePages {
			return nil, newErr(ExitFailure, "source", "the file listing of "+ref.Display()+" does not end").
				why("pagination repeated a page or exceeded %d pages", maxTreePages)
		}
		seen[next] = true
		var page []hfTreeEntry
		var link string
		if err := c.hfJSON(ctx, next, ref, &page, &link); err != nil {
			return nil, err
		}
		for _, e := range page {
			if e.Type != "file" {
				continue
			}
			f, err := c.hfFile(ref, info.Sha, e)
			if err != nil {
				return nil, err
			}
			p.Files = append(p.Files, f)
		}
		next = nextLink(link)
	}
	if err := p.apply(sel, c.store, c.store.Root()); err != nil {
		return nil, err
	}
	return p, nil
}

func (c *Client) hfFile(ref Ref, commit string, e hfTreeEntry) (File, error) {
	clean := path.Clean(e.Path)
	if clean != e.Path || !isLocalSlash(clean) {
		return File{}, newErr(ExitFailure, "source", "refusing unsafe file path "+quote(e.Path)+" from "+ref.Display()).
			why("a path that leaves the model directory could overwrite other files")
	}
	if e.Size < 0 || e.Size > MaxFileSize || (e.Lfs != nil && e.Lfs.Size != e.Size) {
		return File{}, newErr(ExitFailure, "source", "invalid size for "+e.Path+" from "+ref.Display())
	}
	f := File{Path: e.Path, Size: e.Size}
	if e.Lfs != nil {
		if !hex64.MatchString(e.Lfs.Oid) {
			return File{}, newErr(ExitFailure, "source", "invalid sha256 for "+e.Path)
		}
		f.SHA256 = e.Lfs.Oid
	} else {
		if !hex40.MatchString(e.Oid) {
			return File{}, newErr(ExitFailure, "source", "invalid git id for "+e.Path)
		}
		f.GitSHA1 = e.Oid
	}
	segs := strings.Split(e.Path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	f.url = fmt.Sprintf("%s/%s/resolve/%s/%s", c.opt.HFEndpoint, ref.Repo, commit, strings.Join(segs, "/"))
	return f, nil
}

// GlobEscape makes a path match itself literally in --include.
func GlobEscape(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`)
	return r.Replace(p)
}

// isLocalSlash accepts a relative slash path that stays below its root:
// no "..", no empty segments, no backslashes, no control characters (they
// would reach the terminal), and no ".git" segment (lmget never writes into
// a git directory, where a file can change what later git commands run).
func isLocalSlash(p string) bool {
	if p == "" || p == "." || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == utf8.RuneError || ignorable(r) {
			return false
		}
	}
	for _, s := range strings.Split(p, "/") {
		if s == ".." || s == "" || isGitName(s) {
			return false
		}
	}
	return true
}

// ignorable reports code points that some filesystems drop when they compare
// names (HFS+ folds them away, so ".g\u200cit" opens ".git"). No model file
// needs them; a name carrying one is refused.
func ignorable(r rune) bool {
	switch {
	case unicode.Is(unicode.Cf, r): // ZWSP, ZWNJ, ZWJ, LRM/RLM, BOM, soft hyphen, ...
		return true
	case r == 0x034f, r == 0x115f, r == 0x1160, r == 0x17b4, r == 0x17b5, r == 0x3164, r == 0xffa0:
		return true
	case r >= 0x180b && r <= 0x180f, r >= 0xfe00 && r <= 0xfe0f, r >= 0xe0000 && r <= 0xe0fff:
		return true
	}
	return false
}

// isGitName reports whether a path segment names a git directory once
// ignorable code points are dropped and case is folded.
func isGitName(seg string) bool {
	return strings.EqualFold(strings.Map(func(r rune) rune {
		if ignorable(r) {
			return -1
		}
		return r
	}, seg), ".git")
}

// Printable drops control characters (C0, DEL, C1) from text before it is
// displayed, so a source, a lockfile or an error cannot drive the terminal.
func Printable(s string) string { return printable(s) }

// printable drops control characters from text that came from elsewhere
// (a server's status line, a license field) before it can be displayed.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
}

func (c *Client) authorize(req *http.Request) {
	req.Header.Set("User-Agent", c.opt.UserAgent)
	// The token goes only to the configured endpoint. net/http drops the
	// Authorization header on redirects to another host (the CDN).
	if c.opt.HFToken != "" && strings.HasPrefix(req.URL.String(), c.opt.HFEndpoint+"/") {
		req.Header.Set("Authorization", "Bearer "+c.opt.HFToken)
	}
}

func (c *Client) hfJSON(ctx context.Context, u string, ref Ref, v any, link *string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return newErr(ExitNetwork, "network", "cannot reach "+c.opt.HFEndpoint).
			wrap(err).fix("lmget doctor", "check connectivity")
	}
	defer resp.Body.Close()
	if err := c.hfStatus(resp, ref); err != nil {
		return err
	}
	if link != nil {
		*link = resp.Header.Get("Link")
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(v); err != nil {
		return newErr(ExitFailure, "source", "unexpected response from "+c.opt.HFEndpoint).wrap(err)
	}
	return nil
}

func (c *Client) hfStatus(resp *http.Response, ref Ref) error {
	switch {
	case resp.StatusCode == 200 || resp.StatusCode == 206:
		return nil
	case resp.StatusCode == 401:
		e := newErr(ExitAuth, "auth", ref.Display()+" needs a Hugging Face token")
		if strings.HasPrefix(c.tokenSource, "unreadable:") {
			return e.why("the token file %s exists but cannot be read", strings.TrimPrefix(c.tokenSource, "unreadable:")).
				fix("", "fix its permissions, or export HF_TOKEN")
		}
		if c.opt.HFToken == "" {
			return e.why("the repository is private or gated, and no token is configured").
				fix("export HF_TOKEN=...", "a read token from https://huggingface.co/settings/tokens")
		}
		return e.why("the configured token was rejected").
			fix("", "check that the token is valid and has read access")
	case resp.StatusCode == 403:
		return newErr(ExitAuth, "auth", ref.Display()+" is gated").
			why("your account has not been granted access to this model").
			fix("", "accept the conditions at https://huggingface.co/"+ref.Repo)
	case resp.StatusCode == 404:
		return newErr(ExitNotFound, "not_found", ref.Display()+" was not found").
			why("no such repository or revision %q (or it is private and no token is set)", ref.Revision).
			fix("", "check the name at https://huggingface.co/"+ref.Repo)
	case resp.StatusCode == 429:
		return newErr(ExitNetwork, "network", "rate limited by "+c.opt.HFEndpoint).
			fix("", "wait a minute and run again; a token raises the limit")
	default:
		return newErr(ExitNetwork, "network", fmt.Sprintf("%s answered %s", c.opt.HFEndpoint, printable(resp.Status)))
	}
}

// maxTreePages bounds the file listing: HF returns 1000 entries per page.
const maxTreePages = 1000

var linkNext = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextLink(h string) string {
	if m := linkNext.FindStringSubmatch(h); m != nil {
		return m[1]
	}
	return ""
}

func licenseString(v any) string {
	return printable(licenseRaw(v))
}

func licenseRaw(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var s []string
		for _, x := range t {
			if xs, ok := x.(string); ok {
				s = append(s, xs)
			}
		}
		return strings.Join(s, ", ")
	}
	return ""
}
