package lmget

import (
	"net/url"
	"regexp"
	"strings"
)

// Ref identifies a model at a source. Revision is what the user asked for
// (a branch, tag or commit); the resolved commit lives in the Plan.
//
// For Source "url", Repo is the https URL (without fragment) and Revision is
// empty. File selects one file of a repository (from a .../resolve/rev/path
// URL). SHA256 is a hash the user pinned in the reference (url#sha256=...).
type Ref struct {
	Source   string `json:"source"`
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
	File     string `json:"file,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
}

// String is the canonical form, used as the key in lmget.lock. File and
// SHA256 are not part of it: the lock keeps them as selection and hashes.
func (r Ref) String() string {
	switch r.Source {
	case "url":
		return r.Repo
	case "oci":
		switch {
		case strings.HasPrefix(r.Revision, "sha256:"):
			return "oci://" + r.Repo + "@" + r.Revision
		case r.Revision != "" && r.Revision != "latest":
			return "oci://" + r.Repo + ":" + r.Revision
		}
		return "oci://" + r.Repo
	case "ollama": // Ollama's own spelling: name:tag, library/ implied
		s := "ollama:" + strings.TrimPrefix(r.Repo, "library/")
		if r.Revision != "" && r.Revision != "latest" {
			s += ":" + r.Revision
		}
		return s
	}
	s := r.Source + ":" + r.Repo
	if r.Revision != "" && r.Revision != "main" {
		s += "@" + r.Revision
	}
	return s
}

// Display is the short form shown to people: no source prefix for hf.
// Redacted is r with a URL's query values removed (see redactURL): what is
// written to the history, the store's manifests and events.
func (r Ref) Redacted() Ref {
	if r.Source == "url" {
		r.Repo = redactURL(r.Repo)
	}
	return r
}

func (r Ref) Display() string {
	switch r.Source {
	case "url":
		return redactURL(r.String())
	case "hf":
		d := strings.TrimPrefix(r.String(), "hf:")
		if r.File != "" {
			d += " " + r.File
		}
		return d
	}
	return r.String()
}

var (
	repoSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	revisionRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

// planned sources: parsed so the error can say "not yet" instead of "unknown"
var plannedSources = map[string]string{
	"ms":         "ModelScope",
	"modelscope": "ModelScope",
	"civitai":    "Civitai",
	"magnet":     "BitTorrent magnet links",
	"sha256":     "content addresses",
}

// ParseRef accepts "Org/Name", "Org/Name@rev", "hf:Org/Name[@rev]",
// Hugging Face URLs (https://huggingface.co/Org/Name[/tree/rev]),
// "ollama:name[:tag]" and Ollama URLs (https://ollama.com/library/name:tag).
// Repo and revision are validated before anything builds a path or URL from them.
func ParseRef(s string) (Ref, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return Ref{}, newErr(ExitUsage, "usage", "no model given").
			fix("lmget Qwen/Qwen3-0.6B", "a Hugging Face repo, optionally @revision")
	}
	if strings.HasPrefix(strings.ToLower(in), "oci://") {
		return parseOCIRef("oci://" + in[len("oci://"):])
	}
	if strings.HasPrefix(in, "https://") || strings.HasPrefix(in, "http://") {
		return parseURLRef(in)
	}
	src := "hf"
	if i := strings.Index(in, ":"); i > 0 && !strings.Contains(in[:i], "/") {
		src, in = strings.ToLower(in[:i]), in[i+1:]
	}
	switch src {
	case "hf", "huggingface":
		src = "hf"
	case "ollama":
		return parseOllamaRef(in, s)
	default:
		if name, ok := plannedSources[src]; ok {
			return Ref{}, newErr(ExitUsage, "unsupported", name+" is not supported yet").
				why("lmget resolves Hugging Face repositories, Ollama models, OCI images and https URLs")
		}
		return Ref{}, newErr(ExitUsage, "usage", "unknown source "+quote(src)).
			fix("lmget hf:Org/Name", "Hugging Face is the default source")
	}
	repo, rev := in, "main"
	if i := strings.LastIndex(in, "@"); i >= 0 {
		repo, rev = in[:i], in[i+1:]
	}
	return validRef(src, repo, rev, s)
}

func parseURLRef(raw string) (Ref, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return Ref{}, newErr(ExitUsage, "usage", "not a valid URL").wrap(err)
	}
	host := strings.ToLower(u.Hostname())
	if host == "huggingface.co" || host == "hf.co" || host == "www.huggingface.co" {
		return parseHFURL(u, raw)
	}
	if host == "ollama.com" || host == "www.ollama.com" {
		return parseOllamaURL(u, raw)
	}
	if u.Scheme != "https" {
		return Ref{}, newErr(ExitUsage, "usage", "plain http is not supported").
			why("without TLS nothing authenticates the first download").
			fix(strings.Replace(raw, "http://", "https://", 1), "")
	}
	if u.User != nil {
		return Ref{}, newErr(ExitUsage, "usage", "URLs with credentials are not accepted").
			why("they would end up in logs and in lmget.lock")
	}
	r := Ref{Source: "url"}
	if u.Fragment != "" {
		h, ok := strings.CutPrefix(u.Fragment, "sha256=")
		h = strings.ToLower(h)
		if !ok || !hex64.MatchString(h) {
			return Ref{}, newErr(ExitUsage, "usage", "unknown URL fragment "+quote("#"+u.Fragment)).
				fix(strings.SplitN(raw, "#", 2)[0]+"#sha256=<64 hex>", "pin the expected sha256")
		}
		r.SHA256 = h
	}
	u.Fragment, u.RawFragment = "", ""
	r.Repo = u.String()
	return r, nil
}

func parseHFURL(u *url.URL, raw string) (Ref, error) {
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(parts) > 0 && (parts[0] == "datasets" || parts[0] == "spaces") {
		return Ref{}, newErr(ExitUsage, "unsupported", "Hugging Face "+parts[0]+" are out of scope").
			why("lmget downloads models")
	}
	if len(parts) < 2 {
		return Ref{}, newErr(ExitUsage, "usage", "the URL does not point to a repository").
			fix("lmget https://huggingface.co/Org/Name", "")
	}
	rev, file := "main", ""
	if len(parts) >= 4 && (parts[2] == "tree" || parts[2] == "blob" || parts[2] == "resolve") {
		if r, err := url.PathUnescape(parts[3]); err == nil {
			rev = r
		}
		if len(parts) > 4 && parts[2] != "tree" { // blob/resolve: one file
			p, err := url.PathUnescape(strings.Join(parts[4:], "/"))
			if err != nil || !isLocalSlash(p) {
				return Ref{}, newErr(ExitUsage, "usage", "not a valid file path in the URL")
			}
			file = p
		}
	}
	r, err := validRef("hf", parts[0]+"/"+parts[1], rev, raw)
	r.File = file
	return r, err
}

func validRef(src, repo, rev, orig string) (Ref, error) {
	segs := strings.Split(repo, "/")
	bad := len(segs) != 2
	for _, s := range segs {
		if !repoSegment.MatchString(s) || strings.Contains(s, "..") {
			bad = true
		}
	}
	if bad {
		return Ref{}, newErr(ExitUsage, "usage", "not a repository name: "+quote(orig)).
			why("expected Org/Name, letters, digits, '.', '_' and '-' only").
			fix("lmget Qwen/Qwen3-0.6B", "")
	}
	if !revisionRe.MatchString(rev) || strings.Contains(rev, "..") {
		return Ref{}, newErr(ExitUsage, "usage", "not a valid revision: "+quote(rev)).
			fix("lmget "+repo+"@main", "a branch, tag or commit")
	}
	return Ref{Source: src, Repo: repo, Revision: rev}, nil
}

func quote(s string) string { return "\"" + printable(s) + "\"" }
