package lmget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// OCI registries (Docker Hub's ai/ models, ghcr.io and any registry that
// speaks the OCI distribution protocol) serve a model as a manifest whose
// layers are blobs addressed by sha256. The commit of a model is the digest
// of its manifest, which a pinned sync asks for by digest. Layers are named by
// the file name the manifest gives them (the CNCF model spec annotation), or
// after their media type (Docker's AI artifacts). Anonymous pull tokens are
// short-lived (Docker Hub: 300 s), so they are renewed before they expire and
// once more on a 401.

const (
	ociManifestType = "application/vnd.oci.image.manifest.v1+json"
	ociIndexType    = "application/vnd.oci.image.index.v1+json"
	dockerListType  = "application/vnd.docker.distribution.manifest.list.v2+json"
)

var (
	ociHostRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*(:[0-9]{1,5})?$`)
	ociPathSeg  = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*$`)
	ociTagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	ociDigestRe = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)
	// file names for layer types without a file name annotation
	ociLayerNames = map[string]string{
		"application/vnd.docker.ai.gguf.v3":     "model.gguf",
		"application/vnd.docker.ai.license":     "license",
		"application/vnd.docker.ai.mmproj":      "mmproj.gguf",
		"application/vnd.docker.ai.vllm.tar":    "model.tar",
		"application/vnd.docker.ai.safetensors": "model.safetensors",
	}
)

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations"`
}

type ociManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Config        ociDescriptor   `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
	Manifests     []any           `json:"manifests"`
}

// parseOCIRef reads oci://host/path[:tag|@sha256:hex].
func parseOCIRef(raw string) (Ref, error) {
	bad := func(why string) (Ref, error) {
		return Ref{}, newErr(ExitUsage, "usage", "not an OCI reference: "+quote(raw)).why("%s", why).
			fix("lmget oci://docker.io/ai/smollm2", "registry host, repository, optional :tag or @sha256:digest")
	}
	in := strings.TrimPrefix(raw, "oci://")
	repo, rev := in, "latest"
	if i := strings.Index(in, "@"); i >= 0 {
		repo, rev = in[:i], in[i+1:]
		if !ociDigestRe.MatchString(rev) {
			return bad("a digest is sha256: and 64 lowercase hex characters")
		}
	} else if i := strings.LastIndex(in, ":"); i > strings.LastIndex(in, "/") {
		repo, rev = in[:i], in[i+1:]
		if !ociTagRe.MatchString(rev) {
			return bad("not a valid tag")
		}
	}
	segs := strings.Split(repo, "/")
	if len(segs) < 2 || !ociHostRe.MatchString(segs[0]) {
		return bad("expected a registry host and a repository")
	}
	if segs[0] == "docker.io" && len(segs) == 2 {
		segs = []string{"docker.io", "library", segs[1]} // Docker Hub's official images
	}
	for _, s := range segs[1:] {
		if !ociPathSeg.MatchString(s) || len(s) > 128 {
			return bad("repository names are lowercase letters, digits and . _ -")
		}
	}
	return Ref{Source: "oci", Repo: strings.Join(segs, "/"), Revision: rev}, nil
}

func validOCIRepo(repo string) bool {
	r, err := parseOCIRef("oci://" + repo)
	return err == nil && r.Repo == repo
}

// ociAPI splits a repo into the API base URL and the repository path.
func ociAPI(repo string) (base, name string) {
	host, name, _ := strings.Cut(repo, "/")
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	return "https://" + host, name
}

type ociToken struct {
	value string
	until time.Time
}

// ociAuth returns a pull token for repo ("" when the registry needs none),
// from the cache unless it expires within 30 s or refresh is set.
func (c *Client) ociAuth(ctx context.Context, repo string, refresh bool) (string, error) {
	c.ociMu.Lock()
	defer c.ociMu.Unlock()
	if t, ok := c.ociTokens[repo]; ok && !refresh && time.Until(t.until) > 30*time.Second {
		return t.value, nil
	}
	base, name := ociAPI(repo)
	tok, ttl, err := c.ociFetchToken(ctx, base, name)
	if err != nil {
		return "", err
	}
	if c.ociTokens == nil {
		c.ociTokens = map[string]ociToken{}
	}
	c.ociTokens[repo] = ociToken{tok, time.Now().Add(ttl)}
	return tok, nil
}

var bearerParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

func (c *Client) ociFetchToken(ctx context.Context, base, name string) (string, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/", nil)
	if err != nil {
		return "", 0, err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, newErr(ExitNetwork, "network", "cannot reach "+hostOf(base)).wrap(err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return "", 24 * time.Hour, nil // an open registry
	}
	ch := resp.Header.Get("WWW-Authenticate")
	if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(strings.ToLower(ch), "bearer ") {
		return "", 0, newErr(ExitNetwork, "network", fmt.Sprintf("%s answered %s", hostOf(base), printable(resp.Status)))
	}
	params := map[string]string{}
	for _, m := range bearerParam.FindAllStringSubmatch(ch, -1) {
		params[strings.ToLower(m[1])] = m[2]
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Scheme != "https" || realm.Host == "" {
		return "", 0, newErr(ExitFailure, "source", hostOf(base)+" names no https token service")
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", "repository:"+name+":pull")
	realm.RawQuery = q.Encode()
	treq, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", 0, err
	}
	c.authorize(treq)
	tresp, err := c.http.Do(treq)
	if err != nil {
		return "", 0, newErr(ExitNetwork, "network", "cannot reach "+realm.Host).wrap(err)
	}
	defer tresp.Body.Close()
	if tresp.StatusCode != http.StatusOK {
		return "", 0, newErr(ExitAuth, "auth", fmt.Sprintf("%s gave no pull token (%s)", realm.Host, printable(tresp.Status))).
			why("lmget pulls public images only")
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(tresp.Body, 1<<20)).Decode(&t); err != nil {
		return "", 0, newErr(ExitFailure, "source", "unexpected answer from "+realm.Host).wrap(err)
	}
	tok := t.Token
	if tok == "" {
		tok = t.AccessToken
	}
	ttl := time.Duration(t.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second // the distribution spec's default
	}
	return tok, ttl, nil
}

// ociDo sends req to the registry with a pull token, renewing it once on 401.
func (c *Client) ociDo(ctx context.Context, repo string, build func() (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		tok, err := c.ociAuth(ctx, repo, attempt > 0)
		if err != nil {
			return nil, err
		}
		req, err := build()
		if err != nil {
			return nil, err
		}
		c.authorize(req)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.http.Do(req)
		if err != nil || resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return resp, err
		}
		resp.Body.Close()
	}
}

func (c *Client) resolveOCI(ctx context.Context, ref Ref, sel Selection) (*Plan, error) {
	rev := ref.Revision
	if hex64.MatchString(rev) { // a pinned sync asks for the locked digest
		rev = "sha256:" + rev
	}
	base, name := ociAPI(ref.Repo)
	resp, err := c.ociDo(ctx, ref.Repo, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/"+name+"/manifests/"+rev, nil)
		if err == nil {
			req.Header.Set("Accept", strings.Join([]string{ociManifestType, ociIndexType, ollamaManifestType, dockerListType}, ", "))
		}
		return req, err
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, AsError(err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
	case 404:
		return nil, newErr(ExitNotFound, "not_found", ref.Display()+" was not found").
			why("no such repository, tag or digest in %s", hostOf(base))
	case 401, 403:
		return nil, newErr(ExitAuth, "auth", ref.Display()+" is not public").why("lmget pulls public images only")
	case 429:
		return nil, newErr(ExitNetwork, "network", "rate limited by "+hostOf(base)).fix("", "wait and run again")
	default:
		return nil, newErr(ExitNetwork, "network", fmt.Sprintf("%s answered %s", hostOf(base), printable(resp.Status)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, ollamaMaxManifest+1))
	if err != nil {
		return nil, newErr(ExitNetwork, "network", "the manifest of "+ref.Display()+" did not arrive").wrap(err)
	}
	if len(body) > ollamaMaxManifest {
		return nil, newErr(ExitFailure, "source", "the manifest of "+ref.Display()+" is larger than 4 MiB")
	}
	s := sha256.Sum256(body)
	commit := hex.EncodeToString(s[:])
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" && d != "sha256:"+commit {
		return nil, newErr(ExitVerify, "verify", "the manifest of "+ref.Display()+" does not match its digest").
			why("%s says %s, the bytes hash to sha256:%s", hostOf(base), printable(d), commit[:12])
	}
	if strings.HasPrefix(rev, "sha256:") && rev != "sha256:"+commit {
		return nil, newErr(ExitVerify, "verify", "the registry served another manifest than the digest asked for")
	}
	return c.ociPlan(ref, body, commit, sel)
}

func (c *Client) ociPlan(ref Ref, body []byte, commit string, sel Selection) (*Plan, error) {
	bad := func(format string, a ...any) error {
		return newErr(ExitFailure, "source", "unexpected manifest for "+ref.Display()).why(format, a...)
	}
	var m ociManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, bad("not valid JSON: %v", err)
	}
	if m.MediaType == ociIndexType || m.MediaType == dockerListType || len(m.Manifests) > 0 {
		return nil, newErr(ExitUsage, "unsupported", ref.Display()+" is a multi-platform index").
			why("lmget downloads model manifests; pick one with @sha256:<digest>")
	}
	if m.SchemaVersion != 2 || (m.MediaType != ociManifestType && m.MediaType != ollamaManifestType) {
		return nil, bad("schema %d, media type %q", m.SchemaVersion, printable(m.MediaType))
	}
	if len(m.Layers) == 0 || len(m.Layers) > ollamaMaxLayers {
		return nil, bad("%d layers", len(m.Layers))
	}
	base, name := ociAPI(ref.Repo)
	p := &Plan{Ref: ref, Commit: commit}
	p.Files = append(p.Files, File{Path: OllamaManifestFile, Size: int64(len(body)), SHA256: commit, data: body})
	used := map[string]int{OllamaManifestFile: 1}
	add := func(d ociDescriptor, fname string) error {
		mm := ociDigestRe.FindStringSubmatch(d.Digest)
		if mm == nil {
			return bad("invalid digest %q", printable(d.Digest))
		}
		if d.Size < 0 || d.Size > MaxFileSize {
			return bad("invalid size %d for %s", d.Size, mm[1][:12])
		}
		if path.Clean(fname) != fname || !isLocalSlash(fname) {
			return bad("unsafe file name %s", quote(fname))
		}
		fname = uniqueName(used, fname)
		p.Files = append(p.Files, File{Path: fname, Size: d.Size, SHA256: mm[1],
			url: base + "/v2/" + name + "/blobs/" + d.Digest, ociRepo: ref.Repo})
		return nil
	}
	if err := add(m.Config, "config.json"); err != nil {
		return nil, err
	}
	for _, l := range m.Layers {
		fname := l.Annotations["org.cncf.model.filepath"]
		if fname == "" {
			fname = l.Annotations["org.opencontainers.image.title"]
		}
		if fname == "" {
			fname = ociLayerNames[l.MediaType]
		}
		if fname == "" {
			if mm := ociDigestRe.FindStringSubmatch(l.Digest); mm != nil {
				fname = "layer-" + mm[1][:12] // a type lmget does not know: keep it, by digest
			} else {
				return nil, bad("invalid digest %q", printable(l.Digest))
			}
		}
		if err := add(l, fname); err != nil {
			return nil, err
		}
	}
	if err := p.apply(sel, c.store, c.store.Root()); err != nil {
		return nil, err
	}
	for i := range p.Files { // the manifest always comes along
		if p.Files[i].Path == OllamaManifestFile {
			p.Files[i].Skipped = ""
		}
	}
	p.recount(c.store)
	return p, nil
}

// uniqueName returns name, or the first free name-N.ext, and marks it used.
func uniqueName(used map[string]int, name string) string {
	if used[name] > 0 {
		ext := path.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		for k := used[name] + 1; ; k++ {
			if cand := fmt.Sprintf("%s-%d%s", stem, k, ext); used[cand] == 0 {
				used[name]++
				name = cand
				break
			}
		}
	}
	used[name]++
	return name
}
