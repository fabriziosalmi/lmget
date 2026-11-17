package lmget

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Target is where a pulled model is made visible.
//
//	hf        the Hugging Face cache (transformers, mlx-lm, vLLM and huggingface_hub read it)
//	ollama    Ollama's models directory (ollama run reads it)
//	omlx      oMLX's models directory, as <dir>/<org>/<name>
//	lmstudio  LM Studio's models directory, as <dir>/<org>/<name>
//	dir:PATH  a plain directory with the repository's file names
//	none      store only
//
// For omlx and lmstudio, Path is the app's models directory, resolved on this
// machine; a lock records only the kind.
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
	case s == "ollama":
		return Target{Kind: "ollama"}, nil
	case s == "omlx" || s == "lmstudio":
		dir, err := AppModelsDir(s)
		if err != nil {
			return Target{}, err
		}
		return Target{Kind: s, Path: dir}, nil
	case strings.HasPrefix(s, "dir:") && len(s) > 4:
		p, err := filepath.Abs(s[4:])
		if err != nil {
			return Target{}, err
		}
		return Target{Kind: "dir", Path: p}, nil
	}
	return Target{}, newErr(ExitUsage, "usage", "unknown target "+quote(s)).
		fix("--to hf", "Hugging Face cache (default)").
		fix("--to ollama", "Ollama's models directory (Ollama models)").
		fix("--to omlx", "oMLX's models directory").
		fix("--to lmstudio", "LM Studio's models directory").
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
// returns the directory that holds them. It holds the store lock shared, as
// a pull does, so rm and doctor --verify cannot run meanwhile.
func (c *Client) Export(plan *Plan, t Target) (string, error) {
	unlock, err := c.store.flock(".lock", false, true)
	if err != nil {
		return "", diskErr(err)
	}
	defer unlock()
	revoked, err := c.revokedBlobs() // as Pull does: revoked bytes are never exported
	if err != nil {
		return "", err
	}
	all := make(map[string]bool, len(plan.Files))
	for _, f := range plan.Files {
		all[f.Path] = true
	}
	for _, f := range plan.Selected() {
		if err := revokedErr(revoked, f.Path, f.SHA256); err != nil {
			return "", err
		}
		if c.store.Has(f.SHA256, f.Size) { // as Pull does: the content under this name
			if err := c.checkStored(&f, isPickle(f.Path, all)); err != nil {
				return "", err
			}
		}
	}
	return c.export2(plan, t)
}

// export2 exports and records the links; the caller holds the store lock.
func (c *Client) export2(plan *Plan, t Target) (string, error) {
	p, links, err := c.export(plan, t)
	// links made before a failure are recorded too, or rm would never see them
	if len(links) > 0 || err == nil {
		if serr := c.store.saveManifest(plan, &ExportRecord{Kind: t.Kind, Path: p, Links: links}, nil); serr != nil && err == nil {
			err = diskErr(serr)
		}
	}
	if err != nil {
		return "", err
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
		if plan.Ref.Source != "hf" {
			return "", nil, newErr(ExitUsage, "usage", "the Hugging Face cache only holds Hugging Face repositories").
				fix("--to dir:PATH", "a plain directory (the default for URLs is the current one)")
		}
		return c.exportHF(plan)
	case "dir":
		return c.exportDir(plan, t.Path, false)
	case "ollama":
		return c.exportOllama(plan)
	case "omlx", "lmstudio":
		return c.exportApp(plan, t)
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
			return snap, links, diskErr(err)
		}
		storeBlob := c.store.BlobPath(f.SHA256)
		mine := true
		fi, err := os.Lstat(blob)
		switch {
		case err != nil: // absent
			if err := linkOrSymlink(storeBlob, blob); err != nil {
				return snap, links, diskErr(err)
			}
		case same(storeBlob, blob): // ours from an earlier run
		case fi.Mode().IsRegular() && hasContent(blob, f.Size, f.SHA256):
			mine = false // huggingface_hub's own download, with the right bytes
		default: // wrong bytes, wrong size or not a file: serve the verified ones
			os.Remove(blob)
			if err := linkOrSymlink(storeBlob, blob); err != nil {
				return snap, links, diskErr(err)
			}
		}
		if mine {
			links = append(links, blob)
		}
		dst := filepath.Join(snap, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return snap, links, diskErr(err)
		}
		rel, err := filepath.Rel(filepath.Dir(dst), blob)
		if err != nil {
			return snap, links, err
		}
		created := false
		if cur, err := os.Readlink(dst); err != nil || cur != rel {
			os.Remove(dst)
			if err := os.Symlink(rel, dst); err != nil {
				return snap, links, diskErr(err)
			}
			created = true
		}
		if created || mine {
			links = append(links, dst)
		}
	}
	if plan.Ref.Revision != plan.Commit {
		ref := filepath.Join(repoDir, "refs", filepath.FromSlash(plan.Ref.Revision))
		if err := os.MkdirAll(filepath.Dir(ref), 0o755); err != nil {
			return snap, links, diskErr(err)
		}
		if err := writeFileAtomic(ref, []byte(plan.Commit)); err != nil {
			return snap, links, diskErr(err)
		}
	}
	return snap, links, nil
}

// exportDir links the plan's files under root. With replaceOwn (an app's
// models directory, one revision per model), links lmget made earlier to
// other stored bytes are replaced, and its links to files the plan no longer
// has are removed, so the app never loads two revisions mixed; files lmget
// did not create are never touched.
func (c *Client) exportDir(plan *Plan, root string, replaceOwn bool) (string, []string, error) {
	var links []string
	// the target is the one the user chose (or a lock target already checked
	// to be inside the project): create it, so the checks below can resolve it
	if err := os.MkdirAll(root, 0o755); err != nil {
		return root, nil, diskErr(err)
	}
	own := c.ownLinkCheck()
	for _, f := range plan.Selected() {
		dst := filepath.Join(root, filepath.FromSlash(f.Path))
		src := c.store.BlobPath(f.SHA256)
		// every file, not only the root: a symlinked directory committed
		// below the target must not carry a file anywhere else
		if err := exportContained(root, filepath.Dir(dst)); err != nil {
			return root, links, err
		}
		if same(src, dst) {
			links = append(links, dst) // ours from an earlier run: keep it recorded
			continue
		}
		if _, err := os.Lstat(dst); err == nil {
			switch {
			case replaceOwn && own(dst):
				if err := os.Remove(dst); err != nil {
					return root, links, diskErr(err)
				}
			case hasContent(dst, f.Size, f.SHA256):
				continue // identical bytes already there (e.g. from another store): leave them
			default:
				return root, links, newErr(ExitFailure, "export", dst+" already exists").
					why("lmget does not overwrite files it did not create").
					fix("", "remove it, or export to another directory")
			}
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return root, links, diskErr(err)
		}
		if err := exportContained(root, filepath.Dir(dst)); err != nil { // after creating it, again
			return root, links, err
		}
		if err := linkOrSymlink(src, dst); err != nil {
			return root, links, diskErr(err)
		}
		links = append(links, dst)
	}
	if replaceOwn {
		if err := pruneOwn(root, plan, own); err != nil {
			return root, links, err
		}
	}
	return root, links, nil
}

// exportContained checks that dir, with symlinks resolved, is root or below
// it, and that no component of the resolved path is a .git directory.
func exportContained(root, dir string) error {
	refuse := func(why string) error {
		return newErr(ExitUsage, "export", "refusing to write "+dir).
			why("%s", why).
			fix("", "remove the symlink or choose another target")
	}
	if err := within(root, dir); err != nil {
		return refuse(err.Error())
	}
	real, err := resolveExisting(dir)
	if err != nil {
		return diskErr(err)
	}
	for _, seg := range strings.Split(filepath.ToSlash(real), "/") {
		if isGitName(seg) {
			return refuse("the path goes through a .git directory")
		}
	}
	return nil
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
	err := os.Link(src, dst)
	if err == nil {
		return nil
	}
	// only when a hard link is impossible here; a missing source is an error,
	// not a reason to leave a dangling symlink
	if errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EMLINK) ||
		errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
		return os.Symlink(src, dst)
	}
	return err
}
