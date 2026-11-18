package lmget

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Jan, LocalAI and exo: apps that load models from a folder of their own,
// each with its own layout, read from its source:
//
//	jan      <Jan data>/llamacpp/models/<org>/<name>/ with the GGUF and a
//	         model.yml (model_path relative to the data folder, as Jan writes
//	         it for its own downloads)
//	localai  <models path>/<file>.gguf: the file name is the model name
//	exo      <models dir>/<org>--<name>/ with the files and .exo-download.json,
//	         the manifest exo writes when it has verified a download
//	colibri  <models dir>/<org>--<name>/ with the files and
//	         .colibri-download.json, the manifest colibri's setup writes when a
//	         download is complete (c/setup_download.py)
//
// The model files are hard links into the store, as for every target; the
// two small files lmget writes (model.yml, .exo-download.json) are known to be
// lmget's by their content.

func init() {
	appNames["jan"] = "Jan"
	appNames["localai"] = "LocalAI"
	appNames["exo"] = "exo"
	appNames["colibri"] = "colibri"
}

// janData is Jan's data folder: $LMGET_JAN_DIR, else Jan's default.
func janData(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Jan", "data")
	case "windows":
		if a := os.Getenv("APPDATA"); a != "" {
			return filepath.Join(a, "Jan", "data")
		}
		return filepath.Join(home, "AppData", "Roaming", "Jan", "data")
	}
	if x := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "Jan", "data")
	}
	return filepath.Join(home, ".local", "share", "Jan", "data")
}

// exoModels is exo's default models folder, as exo's constants.py computes
// it: $EXO_DEFAULT_MODELS_DIR, else <data home>/models, where the data home is
// ~/$EXO_HOME, else ~/.exo off Linux, else $XDG_DATA_HOME/exo or
// ~/.local/share/exo.
func exoModels(home string) string {
	if d := os.Getenv("EXO_DEFAULT_MODELS_DIR"); d != "" {
		return expandHome(d)
	}
	data := filepath.Join(home, ".local", "share", "exo")
	switch x := os.Getenv("XDG_DATA_HOME"); {
	case os.Getenv("EXO_HOME") != "":
		data = filepath.Join(home, os.Getenv("EXO_HOME"))
	case runtime.GOOS != "linux":
		data = filepath.Join(home, ".exo")
	case filepath.IsAbs(x):
		data = filepath.Join(x, "exo")
	}
	return filepath.Join(data, "models")
}

// ggufWeights lists the selected GGUF files that hold weights, and the
// multimodal projector apart (llama.cpp loads it separately).
func ggufWeights(plan *Plan) (weights []File, mmproj []File) {
	for _, f := range plan.Selected() {
		if !strings.HasSuffix(strings.ToLower(f.Path), ".gguf") {
			continue
		}
		if strings.Contains(strings.ToLower(path.Base(f.Path)), "mmproj") {
			mmproj = append(mmproj, f)
		} else {
			weights = append(weights, f)
		}
	}
	return
}

// ggufQuantRe finds the quantization in a GGUF file name, as Hugging Face's
// Ollama registry names its tags: Qwen3-0.6B-Q4_K_M.gguf is Q4_K_M.
var ggufQuantRe = regexp.MustCompile(`(?i)[-_.]((?:I?Q[0-9][A-Z0-9_]*)|BF16|F16|F32)(?:-[0-9]{5}-of-[0-9]{5})?\.gguf$`)

// ggufQuant is the quantization of a GGUF file name, Q4_K_M when the name
// does not tell.
func ggufQuant(name string) string {
	if m := ggufQuantRe.FindStringSubmatch(path.Base(name)); m != nil {
		return strings.ToUpper(m[1])
	}
	return "Q4_K_M"
}

// appAccepts is TargetAccepts for these three apps.
func appAccepts(plan *Plan, t Target) error {
	name := appNames[t.Kind]
	if plan.Ref.Source != "hf" {
		return newErr(ExitUsage, "usage", name+"'s models folder holds Hugging Face repositories here").
			fix("--to dir:PATH", "a plain directory")
	}
	switch t.Kind {
	case "jan":
		w, mm := ggufWeights(plan)
		if len(w) != 1 || len(mm) > 1 {
			e := newErr(ExitUsage, "usage", fmt.Sprintf("Jan loads one GGUF per model, and this selection has %d", len(w)))
			if len(w) == 0 {
				e = e.why("Jan's llama.cpp engine loads GGUF files").fix("", "pick a GGUF repository")
			}
			return e.fix("--include '*Q4_K_M*'", "select one quantization")
		}
	case "localai":
		if w, _ := ggufWeights(plan); len(w) == 0 {
			return newErr(ExitUsage, "usage", "LocalAI's llama.cpp backend loads GGUF files, and this selection has none").
				fix("", "pick a GGUF repository")
		}
	case "exo":
		if !hasWeights(plan, ".safetensors") {
			return newErr(ExitUsage, "usage", "exo loads MLX models (config.json and *.safetensors), and this selection has none").
				fix("", "pick an mlx-community repository")
		}
	case "colibri":
		if !hasWeights(plan, ".safetensors") {
			e := newErr(ExitUsage, "usage", "colibri loads a model folder (config.json and *.safetensors), and this selection has none")
			if w, _ := ggufWeights(plan); len(w) > 0 { // colibri imports a GGUF into a folder of its own
				return e.fix("lmget "+plan.Ref.Repo+" --to dir:PATH", "then ./coli import PATH/"+path.Base(w[0].Path)+", which writes the model folder beside it")
			}
			return e.fix("", "pick a colibri container, e.g. Justvugg/Qwen3-Coder-30B-A3B-colibri-int4")
		}
	}
	return nil
}

func (c *Client) exportJan(plan *Plan, t Target) (string, []string, error) {
	dir := filepath.Join(t.Path, "llamacpp", "models", filepath.FromSlash(plan.Ref.Repo))
	if err := os.MkdirAll(t.Path, 0o755); err != nil {
		return "", nil, diskErr(err)
	}
	if err := within(t.Path, dir); err != nil {
		return "", nil, err
	}
	p, links, err := c.exportDir(plan, dir, true)
	if err != nil {
		return p, links, err
	}
	w, mm := ggufWeights(plan)
	rel := func(f File) string { // relative to Jan's data folder, as Jan writes it
		return path.Join("llamacpp", "models", plan.Ref.Repo, f.Path)
	}
	// the fields Jan writes when it imports a GGUF; size_bytes counts the
	// projector too, as Jan counts it
	var b strings.Builder
	fmt.Fprintf(&b, "# written by lmget: delete the model with lmget rm\n%s\n", janMarker)
	fmt.Fprintf(&b, "model_path: %s\n", yamlQuote(rel(w[0])))
	fmt.Fprintf(&b, "name: %s\n", yamlQuote(path.Base(plan.Ref.Repo)))
	size := w[0].Size
	if len(mm) == 1 {
		size += mm[0].Size
	}
	fmt.Fprintf(&b, "size_bytes: %d\n", size)
	fmt.Fprintf(&b, "model_sha256: %s\n", w[0].SHA256)
	fmt.Fprintf(&b, "model_size_bytes: %d\n", w[0].Size)
	if len(mm) == 1 {
		fmt.Fprintf(&b, "mmproj_path: %s\n", yamlQuote(rel(mm[0])))
		fmt.Fprintf(&b, "mmproj_sha256: %s\n", mm[0].SHA256)
		fmt.Fprintf(&b, "mmproj_size_bytes: %d\n", mm[0].Size)
	}
	yml := filepath.Join(dir, "model.yml")
	if old, err := os.ReadFile(yml); err == nil && !janOwned(old) {
		return dir, links, newErr(ExitFailure, "export", yml+" already exists").
			why("Jan wrote it, not lmget: lmget does not overwrite files it did not create").
			fix("", "remove the model in Jan first, or export elsewhere")
	}
	if err := writeFileAtomic(yml, []byte(b.String())); err != nil {
		return dir, links, diskErr(err)
	}
	return dir, append(links, yml), nil
}

// janMarker is a key in every model.yml lmget writes: rm and updates know the
// file by it. A key, not a comment: Jan reads model.yml, adds its own keys and
// writes it back, which keeps unknown keys and drops comments; Jan's own code
// reads each key it uses by name, so it ignores this one.
const janMarker = "lmget_export: true"

// janOwned reports whether a model.yml carries janMarker as a top-level key,
// however a YAML writer has rewritten the rest.
func janOwned(b []byte) bool {
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimRight(l, " \r") == janMarker {
			return true
		}
	}
	return false
}

func yamlQuote(s string) string {
	b, _ := json.Marshal(s) // a JSON string is a valid YAML double-quoted scalar
	return string(b)
}

func (c *Client) exportLocalAI(plan *Plan, t Target) (string, []string, error) {
	if err := os.MkdirAll(t.Path, 0o755); err != nil {
		return "", nil, diskErr(err)
	}
	w, mm := ggufWeights(plan)
	var links []string
	own := c.ownLinkCheck()
	for _, f := range append(w, mm...) {
		dst := filepath.Join(t.Path, path.Base(f.Path)) // flat: the file name is LocalAI's model name
		if err := within(t.Path, dst); err != nil {
			return t.Path, links, err
		}
		src := c.store.BlobPath(f.SHA256)
		if same(src, dst) {
			links = append(links, dst)
			continue
		}
		if fi, err := os.Lstat(dst); err == nil {
			switch {
			case own(dst):
				os.Remove(dst)
			case fi.Mode().IsRegular() && hasContent(dst, f.Size, f.SHA256):
				continue
			default:
				return t.Path, links, newErr(ExitFailure, "export", dst+" already exists").
					why("lmget does not overwrite files it did not create; LocalAI names a model by its file").
					fix("", "remove it, or set LMGET_LOCALAI_DIR to another models path")
			}
		}
		if err := linkOrSymlink(src, dst); err != nil {
			return t.Path, links, diskErr(err)
		}
		links = append(links, dst)
	}
	return t.Path, links, nil
}

// exoManifest is exo's .exo-download.json: the files it verified, complete.
type exoManifest struct {
	Repo     string    `json:"repo"`
	Commit   string    `json:"commit"`
	Files    []exoFile `json:"files"`
	Complete bool      `json:"complete"`
}

type exoFile struct {
	Path    string  `json:"path"`
	Size    int64   `json:"size"`
	SHA256  *string `json:"sha256"`
	GitSHA1 string  `json:"gitSha1"`
}

func (c *Client) exportExo(plan *Plan, t Target) (string, []string, error) {
	dir := filepath.Join(t.Path, strings.ReplaceAll(plan.Ref.Repo, "/", "--"))
	if err := os.MkdirAll(t.Path, 0o755); err != nil {
		return "", nil, diskErr(err)
	}
	if err := within(t.Path, dir); err != nil {
		return "", nil, err
	}
	p, links, err := c.exportDir(plan, dir, true)
	if err != nil {
		return p, links, err
	}
	m := exoManifest{Repo: plan.Ref.Repo, Commit: plan.Commit, Complete: true}
	for _, f := range plan.Selected() {
		ef := exoFile{Path: f.Path, Size: f.Size, GitSHA1: f.GitSHA1}
		if f.GitSHA1 == "" { // an LFS file: exo knows it by its sha256
			sum := f.SHA256
			ef.SHA256 = &sum
		}
		m.Files = append(m.Files, ef)
	}
	b, _ := json.Marshal(m)
	mp := filepath.Join(dir, ".exo-download.json")
	if old, err := os.ReadFile(mp); err == nil {
		var prev exoManifest
		if json.Unmarshal(old, &prev) != nil || !strings.EqualFold(prev.Repo, plan.Ref.Repo) {
			return dir, links, newErr(ExitFailure, "export", mp+" already exists").
				why("it is not a manifest of this model: lmget does not overwrite it")
		}
	}
	if err := writeFileAtomic(mp, b); err != nil {
		return dir, links, diskErr(err)
	}
	return dir, append(links, mp), nil
}

// colibriManifest is colibri's .colibri-download.json: the repository, the
// revision and the files with their sizes; colibri takes the folder as a
// downloaded model when complete is true and each file is there at its size.
// LmgetExport marks the ones lmget wrote: colibri reads its keys by name.
type colibriManifest struct {
	Repo        string        `json:"repo"`
	Revision    string        `json:"revision"`
	Complete    bool          `json:"complete"`
	Files       []colibriFile `json:"files"`
	Completed   string        `json:"completed,omitempty"`
	LmgetExport bool          `json:"lmget_export,omitempty"`
}

type colibriFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func (c *Client) exportColibri(plan *Plan, t Target) (string, []string, error) {
	dir := filepath.Join(t.Path, strings.ReplaceAll(plan.Ref.Repo, "/", "--"))
	if err := os.MkdirAll(t.Path, 0o755); err != nil {
		return "", nil, diskErr(err)
	}
	if err := within(t.Path, dir); err != nil {
		return "", nil, err
	}
	mp := filepath.Join(dir, ".colibri-download.json")
	if old, err := os.ReadFile(mp); err == nil { // before any link: colibri's own download stays as it is
		var prev colibriManifest
		if json.Unmarshal(old, &prev) != nil || !prev.LmgetExport {
			return dir, nil, newErr(ExitFailure, "export", mp+" already exists").
				why("colibri wrote it, not lmget: lmget does not overwrite a download it did not make").
				fix("", "remove the folder, or set LMGET_COLIBRI_DIR to another models directory")
		}
	}
	p, links, err := c.exportDir(plan, dir, true)
	if err != nil {
		return p, links, err
	}
	m := colibriManifest{Repo: plan.Ref.Repo, Revision: plan.Commit, Complete: true, LmgetExport: true,
		Completed: time.Now().Format("2006-01-02T15:04:05")}
	for _, f := range plan.Selected() {
		m.Files = append(m.Files, colibriFile{Path: f.Path, Size: f.Size})
	}
	b, _ := json.MarshalIndent(m, "", " ")
	if err := writeFileAtomic(mp, b); err != nil {
		return dir, links, diskErr(err)
	}
	return dir, append(links, mp), nil
}

// ourSidecar reports whether path is a file lmget wrote next to a model for
// an app (Jan's model.yml, exo's and colibri's manifests) for this plan.
func ourSidecar(p string, plan *Plan) bool {
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	switch filepath.Base(p) {
	case "model.yml":
		return janOwned(b)
	case ".exo-download.json":
		var m exoManifest
		return json.Unmarshal(b, &m) == nil && strings.EqualFold(m.Repo, plan.Ref.Repo)
	case ".colibri-download.json":
		var m colibriManifest
		return json.Unmarshal(b, &m) == nil && m.LmgetExport && strings.EqualFold(m.Repo, plan.Ref.Repo)
	}
	return false
}
