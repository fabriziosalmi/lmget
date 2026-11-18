package lmget

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Where the sha256 of a URL download comes from (Plan.HashFrom).
const (
	HashFromUser   = "user"   // url#sha256=...
	HashFromServer = "server" // Repr-Digest or Digest header
	HashFromFirst  = "first"  // none published: pinned on the first download
)

var (
	safeName   = regexp.MustCompile(`[^A-Za-z0-9._+-]`)
	digestSHA  = regexp.MustCompile(`(?i)sha-256=:?([A-Za-z0-9+/=]+):?`)
	rangeTotal = regexp.MustCompile(`/(\d+)$`)
)

func urlID(u string) string {
	h := sha1.Sum([]byte(u))
	return hex.EncodeToString(h[:])
}

// resolveURL probes a direct https URL with a one-byte ranged GET: size,
// range support, validator (ETag or Last-Modified) and a published sha256 if
// the server sends one. The plan has one file named after the URL path.
func (c *Client) resolveURL(ctx context.Context, ref Ref, sel Selection) (*Plan, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.Repo, nil)
	if err != nil {
		return nil, newErr(ExitUsage, "usage", "not a valid URL").wrap(err)
	}
	req.Header.Set("User-Agent", c.opt.UserAgent)
	req.Header.Set("Range", "bytes=0-0")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, reachErr(hostOf(ref.Repo), err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1)) // never read a 200 body here
	resp.Body.Close()

	var size int64 = -1
	ranges := false
	switch resp.StatusCode {
	case http.StatusPartialContent:
		ranges = true
		if m := rangeTotal.FindStringSubmatch(resp.Header.Get("Content-Range")); m != nil {
			size, _ = strconv.ParseInt(m[1], 10, 64)
		}
	case http.StatusOK:
		size = resp.ContentLength
	case 401, 403:
		return nil, newErr(ExitAuth, "auth", hostOf(ref.Repo)+" refused the download ("+printable(resp.Status)+")").
			why("lmget sends no credentials to arbitrary hosts")
	case 404, 410:
		return nil, newErr(ExitNotFound, "not_found", ref.Repo+" was not found ("+printable(resp.Status)+")")
	default:
		return nil, newErr(ExitNetwork, "network", fmt.Sprintf("%s answered %s", hostOf(ref.Repo), printable(resp.Status)))
	}
	if size > MaxFileSize {
		return nil, newErr(ExitFailure, "source", fmt.Sprintf("%s reports %d bytes, more than lmget accepts", hostOf(ref.Repo), size))
	}
	if size < 0 {
		return nil, newErr(ExitFailure, "source", hostOf(ref.Repo)+" does not report the file size").
			why("lmget needs the size to plan, check disk space and verify")
	}

	f := File{Path: fileNameOf(resp.Request.URL, ref.Repo), Size: size, url: ref.Repo, noRanges: !ranges}
	if et := resp.Header.Get("ETag"); et != "" && !strings.HasPrefix(et, "W/") {
		f.ifRange = et // strong validator: If-Range detects a change mid-download
	} else if lm := resp.Header.Get("Last-Modified"); lm != "" {
		f.ifRange = lm
	}
	h := sha1.Sum([]byte(ref.Repo + "\x00" + f.ifRange + "\x00" + strconv.FormatInt(size, 10)))
	f.tmpKey = "url-" + hex.EncodeToString(h[:]) // a changed file never resumes an old partial

	p := &Plan{Ref: ref, Commit: urlID(ref.Repo)}
	published := publishedSHA256(resp.Header)
	switch {
	case ref.SHA256 != "" && published != "" && ref.SHA256 != published:
		return nil, newErr(ExitVerify, "verify", "the sha256 you pinned does not match the one the server publishes").
			why("pinned %s, %s publishes %s", short(ref.SHA256), hostOf(ref.Repo), short(published))
	case ref.SHA256 != "":
		f.SHA256, p.HashFrom = ref.SHA256, HashFromUser
	case published != "":
		f.SHA256, p.HashFrom = published, HashFromServer
	default:
		p.HashFrom = HashFromFirst
	}
	p.Files = []File{f}
	if err := p.apply(sel, c.store, c.store.Root()); err != nil {
		return nil, err
	}
	return p, nil
}

// publishedSHA256 reads RFC 9530 Repr-Digest, or the older Digest header,
// both of which describe the whole representation (not a range).
func publishedSHA256(h http.Header) string {
	for _, k := range []string{"Repr-Digest", "Digest"} {
		if m := digestSHA.FindStringSubmatch(h.Get(k)); m != nil {
			if b, err := base64.StdEncoding.DecodeString(m[1]); err == nil && len(b) == 32 {
				return hex.EncodeToString(b)
			}
		}
	}
	return ""
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return raw
}

// fileNameOf names the download after the original URL path (redirects to
// CDNs often end in opaque ids), reduced to a safe single path element.
func fileNameOf(final *url.URL, orig string) string {
	name := ""
	if u, err := url.Parse(orig); err == nil {
		name = path.Base(u.Path)
	}
	if (name == "" || name == "/" || name == ".") && final != nil {
		name = path.Base(final.Path)
	}
	name = safeName.ReplaceAllString(name, "_")
	name = strings.TrimLeft(name, ".")
	if name == "" || name == "/" {
		name = "download"
	}
	if len(name) > 200 {
		name = name[len(name)-200:]
	}
	return name
}

// redactURL keeps a URL's shape and drops what a signed URL carries in its
// query: every value becomes REDACTED, the keys stay, so the URL still says
// what it was without the credential. It is for anything written down or
// shown; the download itself uses the URL as given.
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.RawQuery == "" {
		return s
	}
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, url.QueryEscape(k)+"=REDACTED")
	}
	u.RawQuery = strings.Join(parts, "&")
	return u.String()
}
