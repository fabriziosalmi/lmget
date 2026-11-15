package lmget

import (
	"errors"
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
	for _, f := range plan.Selected() {
		if !hex64.MatchString(f.SHA256) || !c.store.Has(f.SHA256, f.Size) {
			return "", newErr(ExitFailure, "export", f.Path+" is not in the store")
		}
		if !isLocalSlash(f.Path) {
			return "", newErr(ExitFailure, "export", "unsafe path "+quote(f.Path))
		}
	}
	switch t.Kind {
	case "hf":
		return c.exportHF(plan)
	case "dir":
		return c.exportDir(plan, t.Path)
	}
	return "", nil
}

func (c *Client) exportHF(plan *Plan) (string, error) {
	repoDir := filepath.Join(HFCacheDir(), "models--"+strings.ReplaceAll(plan.Ref.Repo, "/", "--"))
	snap := filepath.Join(repoDir, "snapshots", plan.Commit)
	for _, f := range plan.Selected() {
		etag := f.SHA256 // huggingface_hub names LFS blobs by sha256, others by git id
		if f.GitSHA1 != "" {
			etag = f.GitSHA1
		}
		blob := filepath.Join(repoDir, "blobs", etag)
		if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
			return "", diskErr(err)
		}
		if fi, err := os.Stat(blob); err != nil || fi.Size() != f.Size {
			os.Remove(blob)
			if err := linkOrSymlink(c.store.BlobPath(f.SHA256), blob); err != nil {
				return "", diskErr(err)
			}
		}
		dst := filepath.Join(snap, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", diskErr(err)
		}
		rel, err := filepath.Rel(filepath.Dir(dst), blob)
		if err != nil {
			return "", err
		}
		if cur, err := os.Readlink(dst); err != nil || cur != rel {
			os.Remove(dst)
			if err := os.Symlink(rel, dst); err != nil {
				return "", diskErr(err)
			}
		}
	}
	if plan.Ref.Revision != plan.Commit {
		ref := filepath.Join(repoDir, "refs", filepath.FromSlash(plan.Ref.Revision))
		if err := os.MkdirAll(filepath.Dir(ref), 0o755); err != nil {
			return "", diskErr(err)
		}
		if err := writeFileAtomic(ref, []byte(plan.Commit)); err != nil {
			return "", diskErr(err)
		}
	}
	return snap, nil
}

func (c *Client) exportDir(plan *Plan, root string) (string, error) {
	for _, f := range plan.Selected() {
		dst := filepath.Join(root, filepath.FromSlash(f.Path))
		src := c.store.BlobPath(f.SHA256)
		if same(src, dst) {
			continue
		}
		if _, err := os.Lstat(dst); err == nil {
			return "", newErr(ExitFailure, "export", dst+" already exists").
				why("lmget does not overwrite files it did not create").
				fix("", "remove it, or export to another directory")
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", diskErr(err)
		}
		if err := linkOrSymlink(src, dst); err != nil {
			return "", diskErr(err)
		}
	}
	return root, nil
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
