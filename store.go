package lmget

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	if err := os.Rename(part, dst); err != nil {
		return err
	}
	if gitOID != "" && hex40.MatchString(gitOID) {
		return writeFileAtomic(filepath.Join(s.root, "index", "git-sha1", gitOID), []byte(sha256))
	}
	return nil
}

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
	Updated time.Time      `json:"updated"`
}

func (s *Store) manifestPath(ref Ref, commit string) (string, error) {
	if !hex40.MatchString(commit) {
		return "", fmt.Errorf("not a commit: %q", commit)
	}
	if _, err := validRef(ref.Source, ref.Repo, "main", ref.Repo); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "refs", ref.Source, filepath.FromSlash(ref.Repo), commit+".json"), nil
}

// saveManifest writes the plan and merges a new export record (if any) with
// the ones already recorded for this commit.
func (s *Store) saveManifest(p *Plan, rec *ExportRecord) error {
	path, err := s.manifestPath(p.Ref, p.Commit)
	if err != nil {
		return err
	}
	m := Manifest{Plan: p}
	if old, err := readManifest(path); err == nil {
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

// Manifests returns every model recorded in the store, with its file path.
func (s *Store) Manifests() (map[string]*Manifest, error) {
	out := map[string]*Manifest{}
	root := filepath.Join(s.root, "refs")
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json") {
			return err
		}
		m, err := readManifest(p)
		if err != nil {
			return nil // unreadable manifests are reported by doctor, not fatal here
		}
		out[p] = m
		return nil
	})
	return out, err
}

func writeFileAtomic(path string, b []byte) error {
	tmp := fmt.Sprintf("%s.tmp%d", path, os.Getpid())
	f, err := os.Create(tmp)
	if err != nil {
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
