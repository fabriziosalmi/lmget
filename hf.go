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
	if ref.Source == "url" {
		return c.resolveURL(ctx, ref, sel)
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
		p.Gated = g
	}
	next := fmt.Sprintf("%s/api/models/%s/tree/%s?recursive=true", c.opt.HFEndpoint, ref.Repo, info.Sha)
	for next != "" {
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

func isLocalSlash(p string) bool {
	if p == "" || p == "." || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, s := range strings.Split(p, "/") {
		if s == ".." || s == "" {
			return false
		}
	}
	return true
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
		return newErr(ExitNetwork, "network", fmt.Sprintf("%s answered %s", c.opt.HFEndpoint, resp.Status))
	}
}

var linkNext = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextLink(h string) string {
	if m := linkNext.FindStringSubmatch(h); m != nil {
		return m[1]
	}
	return ""
}

func licenseString(v any) string {
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
