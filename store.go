package lmget

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var (
	hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex40 = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Store is the content-addressed store:
//
//	blobs/sha256/<sha256>    verified content, read-only
//	index/git-sha1/<oid>     sha256 of a blob known by its git id
//	tmp/<key>.part|.state    downloads in progress (resumable)
//	refs/<source>/<repo>/<commit>.json  resolved plans
//
// Every hash coming from a source is checked to be lowercase hex before it
// becomes part of a path.
type Store struct{ root string }

func OpenStore(root string) (*Store, error) {
	for _, d := range []string{"blobs/sha256", "index/git-sha1", "tmp", "refs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{root: root}, nil
}

func (s *Store) Root() string { return s.root }

func (s *Store) BlobPath(sha256 string) string {
	if !hex64.MatchString(sha256) {
		panic("lmget: not a sha256: " + sha256) // callers validate first
	}
	return filepath.Join(s.root, "blobs", "sha256", sha256)
}

// Has reports whether a verified blob of that size is present.
func (s *Store) Has(sha256 string, size int64) bool {
	if !hex64.MatchString(sha256) {
		return false
	}
	fi, err := os.Stat(s.BlobPath(sha256))
	return err == nil && fi.Mode().IsRegular() && fi.Size() == size
}

func (s *Store) LookupGit(oid string) (string, bool) {
	if !hex40.MatchString(oid) {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(s.root, "index", "git-sha1", oid))
	if err != nil || !hex64.MatchString(string(b)) {
		return "", false
	}
	return string(b), true
}

func (s *Store) tmpPaths(key string) (part, state string) {
	base := filepath.Join(s.root, "tmp", key)
	return base + ".part", base + ".state"
}

// commit moves a verified download into the store.
func (s *Store) commit(part, sha256, gitOID string) error {
	dst := s.BlobPath(sha256)
	if err := os.Chmod(part, 0o444); err != nil {
		return err
	}
	// a blob already there keeps its inode: hard-link exports made from it
	// stay recognisably lmget's, and nothing replaces bytes under them
	err := os.Link(part, dst)
	switch {
	case err == nil:
		os.Remove(part)
	case errors.Is(err, fs.ErrExist) && sameSize(part, dst) && s.Has(sha256, fileSize(dst)):
		os.Remove(part)
	default: // no hard links on this filesystem, or a broken blob: replace it
		if err := os.Rename(part, dst); err != nil {
			return err
		}
	}
	// On Windows read-only is an attribute of the file, shared by its hard
	// links, and removing the .part link clears it: set it again
	if err := os.Chmod(dst, 0o444); err != nil {
		return err
	}
	if gitOID != "" && hex40.MatchString(gitOID) {
		return writeFileAtomic(filepath.Join(s.root, "index", "git-sha1", gitOID), []byte(sha256))
	}
	return nil
}

func fileSize(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return -1
}

func sameSize(a, b string) bool { return fileSize(a) >= 0 && fileSize(a) == fileSize(b) }

// ExportRecord lists the links lmget created for one export. rm removes
// exactly these, and only while they still point at lmget's data.
type ExportRecord struct {
	Kind  string   `json:"kind"`
	Path  string   `json:"path"`
	Links []string `json:"links,omitempty"`
}

// Manifest is what the store knows about one model at one commit.
type Manifest struct {
	Plan    *Plan          `json:"plan"`
	Exports []ExportRecord `json:"exports,omitempty"`
	// Verified maps file path to the sha256 that a pull checked against the
	// source (published hash, git id, or a URL's own download). Only these
	// bindings let sync work offline: a lockfile alone cannot create one.
	Verified map[string]string `json:"verified,omitempty"`
	Updated  time.Time         `json:"updated"`
}

// validCommit: a git commit, or for Ollama the sha256 of the manifest.
func validCommit(ref Ref, commit string) bool {
	if ref.Source == "ollama" || ref.Source == "oci" {
		return hex64.MatchString(commit)
	}
	return hex40.MatchString(commit)
}

func (s *Store) manifestPath(ref Ref, commit string) (string, error) {
	if !validCommit(ref, commit) {
		return "", fmt.Errorf("not a commit: %q", commit)
	}
	if ref.Source == "url" { // the commit of a URL plan is the sha1 of the URL
		return filepath.Join(s.root, "refs", "url", commit+".json"), nil
	}
	if ref.Source == "oci" { // host/path/...: validated segment by segment
		if !validOCIRepo(ref.Repo) {
			return "", fmt.Errorf("not an OCI repository: %q", ref.Repo)
		}
		return filepath.Join(s.root, "refs", "oci", filepath.FromSlash(ociDirName(ref.Repo, runtime.GOOS)), commit+".json"), nil
	}
	if _, err := validRef(ref.Source, ref.Repo, "main", ref.Repo); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "refs", ref.Source, filepath.FromSlash(ref.Repo), commit+".json"), nil
}

// saveManifest writes the plan and merges a new export record (if any) with
// the ones already recorded for this commit.
func (s *Store) saveManifest(p *Plan, rec *ExportRecord, verified map[string]string) error {
	path, err := s.manifestPath(p.Ref, p.Commit)
	if err != nil {
		return err
	}
	unlock, err := s.flock("refs/.lock", true, true) // read, merge, write: one writer at a time
	if err != nil {
		return err
	}
	defer unlock()
	m := Manifest{Plan: p}
	if old, err := readManifest(path); err == nil {
		m.Plan = unionPlan(p, old.Plan, s)
		m.Verified = old.Verified
		for _, e := range old.Exports {
			if rec == nil || e.Kind != rec.Kind || e.Path != rec.Path {
				m.Exports = append(m.Exports, e)
			} else {
				rec.Links = mergeLinks(e.Links, rec.Links)
			}
		}
	}
	if rec != nil {
		m.Exports = append(m.Exports, *rec)
	}
	if len(verified) > 0 {
		if m.Verified == nil {
			m.Verified = map[string]string{}
		}
		for k, v := range verified {
			m.Verified[k] = v
		}
	}
	m.Updated = time.Now().UTC()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

// unionPlan keeps, next to the new selection, the files an earlier pull of
// the same commit selected and the store still holds: a narrower --include
// must not make gc forget blobs that earlier exports still link.
func unionPlan(p, old *Plan, s *Store) *Plan {
	if old == nil {
		return p
	}
	have := map[string]bool{}
	for _, f := range p.Selected() {
		have[f.Path] = true
	}
	cp := *p
	cp.Files = append([]File(nil), p.Files...)
	for _, f := range old.Selected() {
		if have[f.Path] || f.SHA256 == "" || !s.Has(f.SHA256, f.Size) {
			continue
		}
		replaced := false
		for i := range cp.Files {
			if cp.Files[i].Path == f.Path { // known, but not selected this time
				cp.Files[i].Skipped, cp.Files[i].SHA256 = "", f.SHA256
				replaced = true
			}
		}
		if !replaced {
			cp.Files = append(cp.Files, f)
		}
	}
	return &cp
}

// loadManifest returns the manifest of ref at commit, if any.
func (s *Store) loadManifest(ref Ref, commit string) (*Manifest, error) {
	path, err := s.manifestPath(ref, commit)
	if err != nil {
		return nil, err
	}
	return readManifest(path)
}

func mergeLinks(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range append(append([]string{}, a...), b...) {
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}

func readManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Plan == nil { // early format: a bare Plan
		var p Plan
		if err := json.Unmarshal(b, &p); err != nil || p.Commit == "" {
			return nil, fmt.Errorf("%s: not a manifest", path)
		}
		m.Plan = &p
	}
	return &m, nil
}

// Manifests returns every model recorded in the store, keyed by file path,
// and the manifests that could not be read. Callers that delete data must
// stop when bad is not empty: an unreadable manifest still owns its blobs.
func (s *Store) Manifests() (ok map[string]*Manifest, bad []string, err error) {
	ok = map[string]*Manifest{}
	root := filepath.Join(s.root, "refs")
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json") {
			return err
		}
		m, err := readManifest(p)
		if err != nil {
			bad = append(bad, p)
			return nil
		}
		ok[p] = m
		return nil
	})
	return ok, bad, err
}

// flock takes an advisory lock on name inside the store. exclusive=false is
// shared. With wait=false a busy lock returns errBusy at once.
func (s *Store) flock(name string, exclusive, wait bool) (func(), error) {
	f, err := os.OpenFile(filepath.Join(s.root, name), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f, exclusive, wait); err != nil {
		f.Close()
		return nil, err
	}
	return func() { unlockFile(f); f.Close() }, nil
}

var errIsSymlink = errors.New("is a symlink")

var errBusy = newErr(ExitFailure, "busy", "another lmget is using the store").
	why("a download or sync is running in another process").
	fix("", "run this again when it has finished")

// writeFileAtomic writes through a fresh temporary file (O_EXCL, random
// name, never a symlink someone placed in advance) and renames it over path.
// The rename replaces a symlink at path itself, never its target.
func writeFileAtomic(path string, b []byte) error { return writeFileAtomicMode(path, b, 0) }

// writeFileAtomicMode is writeFileAtomic with the file mode set on the
// temporary file before the rename (0 keeps CreateTemp's 0600).
func writeFileAtomicMode(path string, b []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if mode != 0 {
		err = f.Chmod(mode)
	}
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// ociDirName is where an OCI repository's manifests go in the store. On
// Windows a registry's port (localhost:5000) cannot be in a directory name;
// '%' is in no valid OCI name, so the encoding is unambiguous. The
// reference itself is read from the manifest, never from the path.
func ociDirName(repo, goos string) string {
	if goos == "windows" {
		return strings.ReplaceAll(repo, ":", "%3A")
	}
	return repo
}
