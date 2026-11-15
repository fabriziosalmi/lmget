package lmget

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ModelScope as a second origin for Hugging Face repositories. Hugging Face
// stays the authority: the plan, the names and the hashes come from it. When
// a file cannot be fetched from Hugging Face (network errors, 5xx, 429 after
// the retries, or the file gone), lmget looks for the same repository on
// ModelScope and takes the file from there only if ModelScope lists the same
// path with the same size and, when Hugging Face publishes one, the same
// sha256. The bytes are then verified against Hugging Face's hash as usual,
// so ModelScope is never trusted. Nothing is asked of ModelScope while
// Hugging Face works, and access refusals (401, 403: gated models) are never
// routed around.

// MirrorModelScope names ModelScope in File.From and in file_done events.
const MirrorModelScope = "modelscope"

type msFile struct {
	Path   string `json:"Path"`
	Type   string `json:"Type"`
	Size   int64  `json:"Size"`
	Sha256 string `json:"Sha256"`
}

type msListing struct {
	Code int `json:"Code"`
	Data struct {
		Files []msFile `json:"Files"`
	} `json:"Data"`
}

// canMirror reports whether a failed Hugging Face download may be retried
// from ModelScope.
func (c *Client) canMirror(ref Ref, err error) bool {
	if c.opt.NoMirror || ref.Source != "hf" || c.opt.ModelScopeEndpoint == "" {
		return false
	}
	e := AsError(err)
	return (e.Code == ExitNetwork && e.Kind != "changed") || e.Code == ExitNotFound
}

// mirrorFile returns f with its URL on ModelScope, when ModelScope lists the
// same file. The listing is fetched once per repository and run.
func (c *Client) mirrorFile(ctx context.Context, ref Ref, f File) (File, bool) {
	files := c.modelScopeFiles(ctx, ref.Repo)
	m, ok := files[f.Path]
	if !ok || m.Size != f.Size {
		return File{}, false
	}
	if f.SHA256 != "" && !strings.EqualFold(m.Sha256, f.SHA256) {
		return File{}, false // a different version of the file: do not even try
	}
	segs := strings.Split(f.Path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	f.url = fmt.Sprintf("%s/models/%s/resolve/master/%s", c.opt.ModelScopeEndpoint, ref.Repo, strings.Join(segs, "/"))
	f.ifRange, f.noRanges = "", false
	f.From = MirrorModelScope
	return f, true
}

func (c *Client) modelScopeFiles(ctx context.Context, repo string) map[string]msFile {
	c.msMu.Lock()
	defer c.msMu.Unlock()
	if files, ok := c.msFiles[repo]; ok {
		return files
	}
	files := map[string]msFile{}
	if c.msFiles == nil {
		c.msFiles = map[string]map[string]msFile{}
	}
	c.msFiles[repo] = files // an unreachable mirror is asked once, not per file
	u := fmt.Sprintf("%s/api/v1/models/%s/repo/files?Recursive=true&Revision=master", c.opt.ModelScopeEndpoint, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return files
	}
	c.authorize(req) // user agent only: the token belongs to the HF endpoint
	resp, err := c.http.Do(req)
	if err != nil {
		return files
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return files
	}
	var l msListing
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&l) != nil || l.Code != 200 {
		return files
	}
	for _, m := range l.Data.Files {
		if m.Type == "blob" && isLocalSlash(m.Path) && m.Size >= 0 && m.Size <= MaxFileSize {
			files[m.Path] = m
		}
	}
	return files
}
