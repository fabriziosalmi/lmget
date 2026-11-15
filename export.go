package lmget

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Target is where a pulled model is made visible.
//
//	hf        the Hugging Face cache (transformers, mlx-lm and huggingface_hub read it)
//	dir:PATH  a plain directory with the repository's file names
//	none      store only
type Target struct {
	Kind string `json:"kind"`
	Path string `json:"path,omitempty"`
}

func ParseTarget(s string) (Target, error) {
	switch {
	case s == "" || s == "hf":
		return Target{Kind: "hf"}, nil
	case s == "none":
		return Target{Kind: "none"}, nil
	case strings.HasPrefix(s, "dir:") && len(s) > 4:
		p, err := filepath.Abs(s[4:])
		if err != nil {
			return Target{}, err
		}
		return Target{Kind: "dir", Path: p}, nil
	}
	return Target{}, newErr(ExitUsage, "usage", "unknown target "+quote(s)).
		fix("--to hf", "Hugging Face cache (default)").
		fix("--to dir:./models/name", "plain directory").
		fix("--to none", "store only")
}

// HFCacheDir follows huggingface_hub: $HF_HUB_CACHE, $HF_HOME/hub,
// $XDG_CACHE_HOME/huggingface/hub, ~/.cache/huggingface/hub.
func HFCacheDir() string {
	if p := os.Getenv("HF_HUB_CACHE"); p != "" {
		return p
	}
	if p := os.Getenv("HF_HOME"); p != "" {
		return filepath.Join(p, "hub")
	}
	if p := os.Getenv("XDG_CACHE_HOME"); p != "" {
		return filepath.Join(p, "huggingface", "hub")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".cache", "huggingface", "hub")
}

// Export links the selected, verified files of plan into the target and
// returns the directory that holds them.
func (c *Client) Export(plan *Plan, t Target) (string, error) {
	p, links, err := c.export(plan, t)
	if err != nil {
		return "", err
	}
	if err := c.store.saveManifest(plan, &ExportRecord{Kind: t.Kind, Path: p, Links: links}); err != nil {
		return "", diskErr(err)
	}
	return p, nil
}

// export returns the target directory and the links it created in this call.
func (c *Client) export(plan *Plan, t Target) (string, []string, error) {
	for _, f := range plan.Selected() {
		if !hex64.MatchString(f.SHA256) || !c.store.Has(f.SHA256, f.Size) {
			return "", nil, newErr(ExitFailure, "export", f.Path+" is not in the store")
		}
		if !isLocalSlash(f.Path) {
			return "", nil, newErr(ExitFailure, "export", "unsafe path "+quote(f.Path))
		}
	}
	switch t.Kind {
	case "hf":
		return c.exportHF(plan)
	case "dir":
		return c.exportDir(plan, t.Path)
	}
	return "", nil, nil
}

func (c *Client) exportHF(plan *Plan) (string, []string, error) {
	var links []string
	repoDir := filepath.Join(HFCacheDir(), "models--"+strings.ReplaceAll(plan.Ref.Repo, "/", "--"))
	snap := filepath.Join(repoDir, "snapshots", plan.Commit)
	for _, f := range plan.Selected() {
		etag := f.SHA256 // huggingface_hub names LFS blobs by sha256, others by git id
		if f.GitSHA1 != "" {
			etag = f.GitSHA1
		}
		blob := filepath.Join(repoDir, "blobs", etag)
		if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
			return "", nil, diskErr(err)
		}
		if fi, err := os.Stat(blob); err != nil || fi.Size() != f.Size {
			os.Remove(blob)
			if err := linkOrSymlink(c.store.BlobPath(f.SHA256), blob); err != nil {
				return "", nil, diskErr(err)
			}
			links = append(links, blob)
		}
		dst := filepath.Join(snap, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", nil, diskErr(err)
		}
		rel, err := filepath.Rel(filepath.Dir(dst), blob)
		if err != nil {
			return "", nil, err
		}
		if cur, err := os.Readlink(dst); err != nil || cur != rel {
			os.Remove(dst)
			if err := os.Symlink(rel, dst); err != nil {
				return "", nil, diskErr(err)
			}
			links = append(links, dst)
		}
	}
	if plan.Ref.Revision != plan.Commit {
		ref := filepath.Join(repoDir, "refs", filepath.FromSlash(plan.Ref.Revision))
		if err := os.MkdirAll(filepath.Dir(ref), 0o755); err != nil {
			return "", nil, diskErr(err)
		}
		if err := writeFileAtomic(ref, []byte(plan.Commit)); err != nil {
			return "", nil, diskErr(err)
		}
	}
	return snap, links, nil
}

func (c *Client) exportDir(plan *Plan, root string) (string, []string, error) {
	var links []string
	for _, f := range plan.Selected() {
		dst := filepath.Join(root, filepath.FromSlash(f.Path))
		src := c.store.BlobPath(f.SHA256)
		if same(src, dst) {
			continue
		}
		if _, err := os.Lstat(dst); err == nil {
			if hasContent(dst, f.Size, f.SHA256) {
				continue // identical bytes already there (e.g. from another store): leave them
			}
			return "", nil, newErr(ExitFailure, "export", dst+" already exists").
				why("lmget does not overwrite files it did not create").
				fix("", "remove it, or export to another directory")
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", nil, diskErr(err)
		}
		if err := linkOrSymlink(src, dst); err != nil {
			return "", nil, diskErr(err)
		}
		links = append(links, dst)
	}
	return root, links, nil
}

// hasContent reports whether path is a regular file with exactly these bytes.
func hasContent(path string, size int64, sum string) bool {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != size {
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
	return hex.EncodeToString(h.Sum(nil)) == sum
}

func same(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Lstat(b)
	if err1 != nil || err2 != nil {
		return false
	}
	if fb.Mode()&os.ModeSymlink != 0 {
		t, err := os.Readlink(b)
		return err == nil && t == a
	}
	return os.SameFile(fa, fb)
}

// linkOrSymlink hardlinks (zero copy, survives store cleanup) and falls back
// to an absolute symlink across filesystems.
func linkOrSymlink(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrExist) {
		return os.Symlink(src, dst)
	} else {
		return err
	}
}
