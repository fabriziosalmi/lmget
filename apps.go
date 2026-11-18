package lmget

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Apps that load models from a directory of their own: oMLX and LM Studio.
// lmget links a model there as <org>/<name>, the layout both use for what
// they download themselves.

// AppModelsDir is where an app looks for models:
//
//	omlx      $LMGET_OMLX_DIR, else model.model_dir in oMLX's settings.json,
//	          else <base>/models, where <base> is ~/.omlx or the directory
//	          oMLX's launcher reads from ~/Library/Application Support/oMLX/base-path
//	lmstudio  $LMGET_LMSTUDIO_DIR, else ~/.lmstudio/models (LM Studio keeps a
//	          directory moved in the app in no documented file: set the variable)
func AppModelsDir(app string) (string, error) {
	env := map[string]string{"omlx": "LMGET_OMLX_DIR", "lmstudio": "LMGET_LMSTUDIO_DIR",
		"jan": "LMGET_JAN_DIR", "localai": "LMGET_LOCALAI_DIR", "exo": "LMGET_EXO_DIR", "comfyui": "LMGET_COMFYUI_DIR"}[app]
	if env == "" {
		return "", newErr(ExitUsage, "usage", "unknown app "+quote(app))
	}
	abs := func(p, from string) (string, error) {
		if !filepath.IsAbs(p) {
			return "", newErr(ExitUsage, "usage", from+" is not an absolute path: "+quote(p)).
				fix("", "set "+env+" to the app's models directory")
		}
		return filepath.Clean(p), nil
	}
	if p := os.Getenv(env); p != "" {
		return abs(expandHome(p), env)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", newErr(ExitUsage, "usage", "no home directory to find the app's models in").
			fix("", "set "+env)
	}
	switch app {
	case "lmstudio":
		return filepath.Join(home, ".lmstudio", "models"), nil
	case "jan": // Jan's data folder; models go under llamacpp/models
		return janData(home), nil
	case "localai": // the variables local-ai reads, else where LocalAI's launcher keeps them
		for _, v := range []string{"LOCALAI_MODELS_PATH", "MODELS_PATH"} {
			if p := os.Getenv(v); p != "" {
				return abs(expandHome(p), v)
			}
		}
		return filepath.Join(home, ".localai", "models"), nil
	case "exo":
		return abs(exoModels(home), "exo's models directory")
	case "comfyui":
		return comfyModels(home)
	}
	settings := filepath.Join(omlxBase(), "settings.json")
	dir, err := omlxModelDir(settings)
	if err != nil {
		return "", newErr(ExitUsage, "usage", "cannot read the models directory from "+settings).
			why("%s", err.Error()).
			fix("", "set "+env+" to oMLX's models directory")
	}
	if dir == "" {
		return filepath.Join(omlxBase(), "models"), nil
	}
	return abs(expandHome(dir), settings)
}

// omlxModelDir reads model.model_dir from oMLX's settings, "" when the file or
// the key is absent. Only that key is decoded: the file also holds oMLX's API
// keys, which lmget never keeps.
func omlxModelDir(path string) (string, error) {
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var s struct {
		Model struct {
			ModelDir string `json:"model_dir"`
		} `json:"model"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return "", errors.New("not valid JSON")
	}
	return s.Model.ModelDir, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}

var appNames = map[string]string{"omlx": "oMLX", "lmstudio": "LM Studio"}

// exportApp links the model into an app's models directory as <org>/<name>,
// the layout both apps use for what they download themselves.
func (c *Client) exportApp(plan *Plan, t Target) (string, []string, error) {
	name := appNames[t.Kind]
	if plan.Ref.Source != "hf" {
		return "", nil, newErr(ExitUsage, "usage", name+"'s models directory holds Hugging Face repositories here").
			fix("--to dir:PATH", "a plain directory")
	}
	if t.Kind == "omlx" && !hasWeights(plan, ".safetensors") {
		// oMLX finds a model by its config.json and *.safetensors: anything
		// else would sit in its directory unseen
		return "", nil, newErr(ExitUsage, "usage", "oMLX loads safetensors models, and this selection has none").
			why("oMLX discovers a model by config.json and *.safetensors files").
			fix("", "pick an MLX or safetensors repository, e.g. an mlx-community one").
			fix("--to lmstudio", "LM Studio also loads GGUF")
	}
	// the app may be installed but have downloaded nothing yet
	if err := os.MkdirAll(t.Path, 0o755); err != nil {
		return "", nil, diskErr(err)
	}
	dir := filepath.Join(t.Path, filepath.FromSlash(plan.Ref.Repo))
	if err := within(t.Path, dir); err != nil { // the repo name is validated; this is the second lock
		return "", nil, err
	}
	return c.exportDir(plan, dir, true)
}

func hasWeights(plan *Plan, ext string) bool {
	config := false
	weights := false
	for _, f := range plan.Selected() {
		if f.Path == "config.json" {
			config = true
		}
		if strings.HasSuffix(strings.ToLower(f.Path), ext) {
			weights = true
		}
	}
	return config && weights
}

// ownLinkCheck returns a test for "a link lmget made into its store": a
// symlink to a stored blob, or a hard link to one. The store's inodes are
// read once, on first use.
func (c *Client) ownLinkCheck() func(path string) bool {
	blobs := filepath.Join(c.store.Root(), "blobs", "sha256")
	var inodes map[inode]bool
	return func(path string) bool {
		fi, err := os.Lstat(path)
		if err != nil {
			return false
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			t, err := os.Readlink(path)
			if err != nil {
				return false
			}
			if !filepath.IsAbs(t) {
				t = filepath.Join(filepath.Dir(path), t)
			}
			return filepath.Dir(filepath.Clean(t)) == blobs
		}
		if c.store.storedCopy(path, fi) { // a copy lmget made on another drive (Windows)
			return true
		}
		in, ok := inodeOf(path, fi)
		if !ok || !fi.Mode().IsRegular() {
			return false
		}
		if inodes == nil {
			inodes = map[inode]bool{}
			entries, _ := os.ReadDir(blobs)
			for _, e := range entries {
				if bi, err := e.Info(); err == nil {
					if n, ok := inodeOf(filepath.Join(blobs, e.Name()), bi); ok {
						inodes[n] = true
					}
				}
			}
		}
		return inodes[in]
	}
}

// pruneOwn removes, below root, the links lmget made that the plan does not
// name, and the directories that leaves empty. Symlinked directories are not
// followed.
func pruneOwn(root string, plan *Plan, own func(string) bool) error {
	want := map[string]bool{}
	for _, f := range plan.Selected() {
		want[filepath.Join(root, filepath.FromSlash(f.Path))] = true
	}
	var stale []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable: not ours to judge
		}
		if !d.IsDir() && !want[p] && own(p) {
			stale = append(stale, p)
		}
		return nil
	})
	if err != nil {
		return diskErr(err)
	}
	for _, p := range stale {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return diskErr(err)
		}
		removeEmptyUp(filepath.Dir(p), root)
	}
	return nil
}
