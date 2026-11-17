package lmget

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Origins are blob servers the user names (LMGET_ORIGINS, --origin): a LAN
// cache, a company mirror, an lmget-node, or any static server over another
// machine's LMGET_HOME, which holds blobs at the same paths. lmget asks them
// first for every file whose sha256 the plan knows, at
// <origin>/blobs/sha256/<sha256>, and falls back to the source when none
// serves the right bytes. An origin is never trusted: what it serves is
// verified against the sha256 (and the git id, when the source gives one),
// and no token is sent to it.

// UserOrigin prefixes File.From for bytes that came from a user's origin.
const UserOrigin = "origin:"

const maxOrigins = 16

// parseOrigins reads origins from a list of URLs separated by commas or
// spaces. Each is a plain http or https URL: the bytes are verified, so TLS
// adds privacy, not integrity, and LAN caches often have none.
func parseOrigins(list []string) ([]string, error) {
	var out []string
	for _, item := range list {
		for _, o := range strings.FieldsFunc(item, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
			u, err := url.Parse(o)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
				return nil, newErr(ExitUsage, "usage", "origin "+quote(o)+" is not a plain http or https URL").
					fix("", "an origin serves blobs at <origin>/blobs/sha256/<sha256>, e.g. https://cache.example.lan")
			}
			out = append(out, strings.TrimRight(o, "/"))
		}
	}
	if len(out) > maxOrigins {
		return nil, newErr(ExitUsage, "usage", fmt.Sprintf("%d origins, at most %d", len(out), maxOrigins))
	}
	return out, nil
}

// fromOrigins downloads f from the first origin that serves its exact bytes,
// and returns the file as fetched (its From names the origin) and the
// verified sha256. A file whose sha256 is unknown is not asked for.
func (c *Client) fromOrigins(ctx context.Context, f File, part, state string, cnt *counters) (File, string, bool) {
	if f.SHA256 == "" || f.data != nil || f.Size == 0 {
		return f, "", false
	}
	for _, o := range c.opt.Origins {
		of := f
		of.url = o + "/blobs/sha256/" + f.SHA256
		of.ifRange, of.ociRepo = "", ""
		of.attempts = 2
		// the whole base URL: two origins on one host keep apart their
		// partial downloads and their down marks
		of.From = UserOrigin + strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://")
		if c.nodeIsDown(of.From) || ctx.Err() != nil {
			continue
		}
		ranges, err := c.probeOrigin(ctx, of.url)
		if err != nil {
			if AsError(err).Code == ExitNetwork && ctx.Err() == nil {
				c.markNodeDown(of.From)
			}
			continue // a 404 is this file only
		}
		of.noRanges = !ranges
		cnt.done.Store(0)
		if err := c.fetch(ctx, of, part, state, cnt); err != nil {
			if AsError(err).Code == ExitNetwork && ctx.Err() == nil {
				c.markNodeDown(of.From) // unreachable or failing: not for every file again
			}
			continue // a 404 is this file only
		}
		sum, err := c.verify(part, &of)
		if err != nil {
			os.Remove(part) // other bytes: the next origin, or the source, starts clean
			os.Remove(state)
			if AsError(err).Code == ExitVerify {
				c.markNodeDown(of.From)
			}
			continue
		}
		return of, sum, true
	}
	return f, "", false
}

// probeOrigin asks for the first byte of a blob: 206 means the origin serves
// ranges (parallel chunks, resumable), 200 that it only streams whole files,
// as python3 -m http.server does; anything else, that it cannot serve it.
func (c *Client) probeOrigin(ctx context.Context, u string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, newErr(ExitNetwork, "network", "bad origin URL").wrap(err)
	}
	req.Header.Set("User-Agent", c.opt.UserAgent) // never the HF token
	req.Header.Set("Range", "bytes=0-0")
	resp, err := c.http.Do(req)
	if err != nil {
		return false, newErr(ExitNetwork, "network", "request failed").wrap(err)
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		return true, nil
	case http.StatusOK:
		return false, nil
	}
	return false, fileStatus(resp)
}
