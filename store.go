package lmget

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

func (s *Store) saveRef(p *Plan) error {
	dir := filepath.Join(s.root, "refs", p.Ref.Source, filepath.FromSlash(p.Ref.Repo))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, p.Commit+".json"), b)
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
