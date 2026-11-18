package lmget

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// caseFolds reports whether the filesystem at dir (or its nearest existing
// parent) treats names that differ only in case as one: macOS and Windows
// by default. It probes with a file of its own, removed at once.
func caseFolds(dir string) bool {
	for dir != "" {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
	f, err := os.CreateTemp(dir, ".lmget-CaseProbe-*")
	if err != nil {
		return false // cannot tell: the export will report a real collision
	}
	name := f.Name()
	f.Close()
	defer os.Remove(name)
	_, err = os.Stat(filepath.Join(filepath.Dir(name), strings.ToLower(filepath.Base(name))))
	return err == nil
}

// nameCollisions returns pairs of selected paths that one name would hold on
// a filesystem that folds case and normalizes Unicode (macOS, Windows).
func nameCollisions(p *Plan) [][2]string {
	seen := map[string]string{}
	var out [][2]string
	for _, f := range p.Selected() {
		k := strings.ToLower(norm.NFC.String(f.Path))
		if prev, ok := seen[k]; ok {
			out = append(out, [2]string{prev, f.Path})
			continue
		}
		seen[k] = f.Path
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// targetRoot is where a target writes files under the repository's names;
// "" when it writes none (the store, Ollama's digests).
func targetRoot(t Target) string {
	switch t.Kind {
	case "hf":
		return HFCacheDir()
	case "dir", "omlx", "lmstudio":
		return t.Path
	}
	return ""
}

// copiesAreLinks: on Windows lmget copies a blob where a hard link is
// impossible (another drive), so a regular file with a blob's exact bytes at a
// place lmget exported to is lmget's own. Elsewhere lmget never copies, and
// such a file is the user's. Tests switch it on.
var copiesAreLinks = runtime.GOOS == "windows"

// storedCopy reports whether path is a copy of a stored blob: its size is a
// blob's and its bytes hash to that blob's name.
func (s *Store) storedCopy(path string, fi os.FileInfo) bool {
	if !copiesAreLinks || !fi.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	sum := hex.EncodeToString(h.Sum(nil))
	return s.Has(sum, fi.Size())
}
