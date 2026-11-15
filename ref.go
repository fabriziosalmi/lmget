package lmget

import (
	"net/url"
	"regexp"
	"strings"
)

// Ref identifies a model at a source. Revision is what the user asked for
// (a branch, tag or commit); the resolved commit lives in the Plan.
type Ref struct {
	Source   string `json:"source"`
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
}

func (r Ref) String() string {
	s := r.Source + ":" + r.Repo
	if r.Revision != "" && r.Revision != "main" {
		s += "@" + r.Revision
	}
	return s
}

// Display is the short form shown to people: no source prefix for hf.
func (r Ref) Display() string {
	if r.Source == "hf" {
		return strings.TrimPrefix(r.String(), "hf:")
	}
	return r.String()
}

var (
	repoSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	revisionRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

// planned sources: parsed so the error can say "not yet" instead of "unknown"
var plannedSources = map[string]string{
	"ollama":     "Ollama registry",
	"ms":         "ModelScope",
	"modelscope": "ModelScope",
	"civitai":    "Civitai",
	"oci":        "OCI registries",
	"magnet":     "BitTorrent magnet links",
	"sha256":     "content addresses",
}

// ParseRef accepts "Org/Name", "Org/Name@rev", "hf:Org/Name[@rev]" and
// Hugging Face URLs (https://huggingface.co/Org/Name[/tree/rev]).
// Repo and revision are validated before anything builds a path or URL from them.
func ParseRef(s string) (Ref, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return Ref{}, newErr(ExitUsage, "usage", "no model given").
			fix("lmget Qwen/Qwen3-0.6B", "a Hugging Face repo, optionally @revision")
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
	default:
		if name, ok := plannedSources[src]; ok {
			return Ref{}, newErr(ExitUsage, "unsupported", name+" is not supported yet").
				why("this build resolves Hugging Face repositories only")
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
	if err != nil {
		return Ref{}, newErr(ExitUsage, "usage", "not a valid URL").wrap(err)
	}
	host := strings.ToLower(u.Hostname())
	if host != "huggingface.co" && host != "hf.co" && host != "www.huggingface.co" {
		return Ref{}, newErr(ExitUsage, "unsupported", "direct URL downloads are not supported yet").
			why("this build resolves Hugging Face repositories only")
	}
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(parts) > 0 && (parts[0] == "datasets" || parts[0] == "spaces") {
		return Ref{}, newErr(ExitUsage, "unsupported", "Hugging Face "+parts[0]+" are out of scope").
			why("lmget downloads models")
	}
	if len(parts) < 2 {
		return Ref{}, newErr(ExitUsage, "usage", "the URL does not point to a repository").
			fix("lmget https://huggingface.co/Org/Name", "")
	}
	rev := "main"
	if len(parts) >= 4 && (parts[2] == "tree" || parts[2] == "blob" || parts[2] == "resolve") {
		if r, err := url.PathUnescape(parts[3]); err == nil {
			rev = r
		}
	}
	return validRef("hf", parts[0]+"/"+parts[1], rev, raw)
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

func quote(s string) string { return "\"" + s + "\"" }
