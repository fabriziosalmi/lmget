package lmget

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
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
	// the user's origins first, then the lmget servers found on the LAN
	origins := append(append([]string(nil), c.opt.Origins...), c.lanOrigins(ctx)...)
	for i, o := range origins {
		if of, sum, ok := c.fromOrigin(ctx, f, o, i >= len(c.opt.Origins), part, state, cnt); ok {
			return of, sum, true
		}
	}
	return f, "", false
}

// A server found on the LAN is anyone's answer to a multicast query: it
// gets lanProbeTimeout to answer the first byte, and a file
// lanFileTime plus a second per MiB, then the next origin or the source
// takes over. One that trickles a byte at a time cannot hold a pull.
var (
	lanProbeTimeout = 3 * time.Second
	lanFileTime     = 30 * time.Second
)

func lanFileDeadline(size int64) time.Duration {
	return lanFileTime + time.Duration(size>>20)*time.Second
}

func (c *Client) fromOrigin(ctx context.Context, f File, o string, lan bool, part, state string, cnt *counters) (File, string, bool) {
	of := f
	of.url = o + "/blobs/sha256/" + f.SHA256
	of.ifRange, of.ociRepo = "", ""
	of.attempts = 2
	// the whole base URL: two origins on one host keep apart their
	// partial downloads and their down marks
	of.From = UserOrigin + strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://")
	if c.nodeIsDown(of.From) || ctx.Err() != nil {
		return f, "", false
	}
	octx, cancel := context.WithCancel(withOriginRequest(ctx)) // its redirects stay on its host
	defer cancel()
	pctx, fctx := octx, octx
	if lan {
		var c1, c2 context.CancelFunc
		pctx, c1 = context.WithTimeout(octx, lanProbeTimeout)
		defer c1()
		fctx, c2 = context.WithTimeout(octx, lanFileDeadline(f.Size))
		defer c2()
	}
	// failing on its own (not because the pull was stopped): skipped for the
	// rest of the run, not asked for every file again
	down := func(err error, cctx context.Context) {
		if ctx.Err() == nil && (AsError(err).Code == ExitNetwork || errors.Is(cctx.Err(), context.DeadlineExceeded)) {
			c.markNodeDown(of.From)
		}
	}
	ranges, err := c.probeOrigin(pctx, of.url)
	if err != nil {
		down(err, pctx)
		return f, "", false // a 404 is this file only
	}
	of.noRanges = !ranges
	cnt.done.Store(0)
	if err := c.fetch(fctx, of, part, state, cnt); err != nil {
		down(err, fctx)
		return f, "", false
	}
	sum, err := c.verify(part, &of)
	if err != nil {
		os.Remove(part) // other bytes: the next origin, or the source, starts clean
		os.Remove(state)
		if AsError(err).Code == ExitVerify {
			c.markNodeDown(of.From)
		}
		return f, "", false
	}
	return of, sum, true
}

// originKey marks a request to a user's or a LAN origin: a redirect to
// another host is not followed (an origin is anyone's server, and its
// redirects could reach this machine's own services).
type originKey struct{}

func withOriginRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, originKey{}, true)
}

func isOriginRequest(ctx context.Context) bool { v, _ := ctx.Value(originKey{}).(bool); return v }

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
