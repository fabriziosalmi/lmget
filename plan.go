package lmget

import (
	"path"
	"sort"
	"strings"
	"syscall"
)

// File is one file of a resolved model.
type File struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256,omitempty"`   // published by the source (LFS), or known from the store
	GitSHA1 string `json:"git_sha1,omitempty"` // git blob id, for files the source hashes only with git
	Cached  bool   `json:"cached"`             // already verified in the store: nothing to fetch
	Skipped string `json:"skipped,omitempty"`  // reason the file is not selected
	From    string `json:"from,omitempty"`     // second origin the bytes came from (modelscope)
	url     string
	// direct URLs only
	ifRange  string // validator sent with every ranged request
	noRanges bool   // the server ignores Range: one sequential stream
	tmpKey   string // partial-download key when no hash is known in advance
	// mustFetch: the stored blob cannot be trusted for this file (its git id
	// was never verified against it), so it is downloaded and checked
	mustFetch bool
	// data: the bytes are already known (an Ollama manifest read while
	// resolving); they go through the same verify, check and commit
	data []byte
	// ociRepo: the blob is on an OCI registry and needs its pull token
	ociRepo string
}

// Selected reports whether the file will be fetched or exported.
func (f File) Selected() bool { return f.Skipped == "" }

// Plan is everything lmget knows before downloading a byte.
type Plan struct {
	Ref      Ref      `json:"ref"`
	Commit   string   `json:"commit"`
	License  string   `json:"license,omitempty"`
	Gated    string   `json:"gated,omitempty"`
	HashFrom string   `json:"hash_from,omitempty"` // direct URLs: user, server or first
	Files    []File   `json:"files"`
	Bytes    int64    `json:"bytes"`            // selected files
	ToFetch  int64    `json:"to_fetch"`         // selected and not cached
	Pickle   []string `json:"pickle,omitempty"` // selected files in a pickle format
	DiskFree int64    `json:"disk_free"`
	// FromIndex: the source was unreachable, the files and hashes come from
	// the signed index, and the bytes from its nodes
	FromIndex bool `json:"from_index,omitempty"`
	offline   bool // built from a lockfile and verified bindings, without the source
}

// Selection filters the files of a repository.
type Selection struct {
	Include     []string // globs; if set, only matching files
	Exclude     []string // globs
	AllowPickle bool     // keep pickle weights even when safetensors exist
}

// pickleExt are the names of formats a loader unpickles: torch (.bin, .pt,
// .pth, .ckpt), libtorch and rust-bert (.ot, a torch zip with data.pkl, as in
// sentence-transformers/all-MiniLM-L6-v2), scikit-learn (.joblib) and plain
// pickles.
var pickleExt = map[string]bool{".bin": true, ".pt": true, ".pth": true, ".ckpt": true, ".pkl": true, ".pickle": true,
	".ot": true, ".joblib": true}

// isPickle is a name heuristic. A .bin next to a same-stem .xml is OpenVINO
// IR weights (raw tensors), not a pickle.
func isPickle(p string, all map[string]bool) bool {
	ext := strings.ToLower(path.Ext(p))
	if !pickleExt[ext] {
		return false
	}
	if ext == ".bin" && all[strings.TrimSuffix(p, path.Ext(p))+".xml"] {
		return false
	}
	return true
}

func matchAny(globs []string, p string) (bool, error) {
	for _, g := range globs {
		for _, cand := range []string{p, path.Base(p)} {
			ok, err := path.Match(g, cand)
			if err != nil {
				return false, newErr(ExitUsage, "usage", "bad pattern "+quote(g)).wrap(err)
			}
			if ok {
				return true, nil
			}
		}
	}
	return false, nil
}

func (p *Plan) apply(sel Selection, st *Store, storeRoot string) error {
	hasSafetensors := false
	all := make(map[string]bool, len(p.Files))
	for _, f := range p.Files {
		all[f.Path] = true
		if strings.HasSuffix(strings.ToLower(f.Path), ".safetensors") {
			hasSafetensors = true
		}
	}
	p.Bytes, p.ToFetch, p.Pickle = 0, 0, nil
	for i := range p.Files {
		f := &p.Files[i]
		f.Skipped = ""
		if len(sel.Include) > 0 {
			ok, err := matchAny(sel.Include, f.Path)
			if err != nil {
				return err
			}
			if !ok {
				f.Skipped = "not included"
			}
		}
		if f.Selected() && len(sel.Exclude) > 0 {
			ok, err := matchAny(sel.Exclude, f.Path)
			if err != nil {
				return err
			}
			if ok {
				f.Skipped = "excluded"
			}
		}
		if f.Selected() && isPickle(f.Path, all) && hasSafetensors && !sel.AllowPickle {
			f.Skipped = "pickle, safetensors available"
		}
		if f.SHA256 == "" && f.GitSHA1 != "" {
			if s, ok := st.LookupGit(f.GitSHA1); ok {
				f.SHA256 = s
			}
		}
		f.Cached = f.SHA256 != "" && !f.mustFetch && st.Has(f.SHA256, f.Size)
		if !f.Selected() {
			continue
		}
		p.Bytes += f.Size
		if !f.Cached {
			p.ToFetch += f.Size
		}
		if isPickle(f.Path, all) {
			p.Pickle = append(p.Pickle, f.Path)
		}
	}
	sort.SliceStable(p.Files, func(i, j int) bool { return p.Files[i].Path < p.Files[j].Path })
	p.DiskFree = diskFree(storeRoot)
	return nil
}

// recount refreshes Cached, Bytes, ToFetch, Pickle and DiskFree after the
// file list or hashes changed.
func (p *Plan) recount(st *Store) {
	all := make(map[string]bool, len(p.Files))
	for _, f := range p.Files {
		all[f.Path] = true
	}
	p.Bytes, p.ToFetch, p.Pickle = 0, 0, nil
	for i := range p.Files {
		f := &p.Files[i]
		f.Cached = f.SHA256 != "" && !f.mustFetch && st.Has(f.SHA256, f.Size)
		if !f.Selected() {
			continue
		}
		p.Bytes += f.Size
		if !f.Cached {
			p.ToFetch += f.Size
		}
		if isPickle(f.Path, all) {
			p.Pickle = append(p.Pickle, f.Path)
		}
	}
	sort.SliceStable(p.Files, func(i, j int) bool { return p.Files[i].Path < p.Files[j].Path })
	p.DiskFree = diskFree(st.Root())
}

// Selected returns the files that will be fetched or exported.
func (p *Plan) Selected() []File {
	var out []File
	for _, f := range p.Files {
		if f.Selected() {
			out = append(out, f)
		}
	}
	return out
}

func diskFree(dir string) int64 {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return -1
	}
	return int64(s.Bavail) * int64(s.Bsize)
}
