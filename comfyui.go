package lmget

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// ComfyUI loads image and video models (Flux, Stable Diffusion, Qwen-Image,
// ...) from one models directory, a folder per kind of file
// (comfy/folder_paths.py): a full checkpoint in checkpoints/, a bare
// diffusion model in diffusion_models/, text encoders, VAEs, LoRAs in their
// own. Workflows name a file by its name in that folder, so lmget links each
// weight file flat into its folder: models/<folder>/<file name>.
//
// The folder of a file comes from, in order:
//
//	--to comfyui:FOLDER   the user says it, for every selected weight file
//	its path              a directory of the repository named as a ComfyUI
//	                      folder, as Comfy-Org lays out its repositories
//	                      (split_files/diffusion_models/flux2_dev_fp8mixed.safetensors)
//	its tensors           a safetensors file at the top of the repository
//	                      whose tensors are named model.diffusion_model.* is
//	                      a full checkpoint: the prefix ComfyUI's own
//	                      checkpoint loader looks for. Read from the stored
//	                      blob, after it is verified.
//
// Anything else lmget does not guess: it asks for the folder. A repository
// in the diffusers layout (model_index.json, a folder per component) is not
// read by folder: its vae/ is diffusers', not ComfyUI's, and ComfyUI loads
// single files.

func init() { appNames["comfyui"] = "ComfyUI" }

// comfyFolders are ComfyUI's model folders that hold weight files, under the
// directory names ComfyUI reads (unet and clip are older names it still reads).
var comfyFolders = map[string]bool{
	"checkpoints": true, "diffusion_models": true, "unet": true, "text_encoders": true, "clip": true,
	"vae": true, "loras": true, "controlnet": true, "t2i_adapter": true, "clip_vision": true,
	"style_models": true, "embeddings": true, "vae_approx": true, "gligen": true, "upscale_models": true,
	"latent_upscale_models": true, "hypernetworks": true, "photomaker": true, "model_patches": true,
	"audio_encoders": true, "background_removal": true, "frame_interpolation": true,
	"geometry_estimation": true, "optical_flow": true, "detection": true,
}

// ComfyFolders lists the folders --to comfyui:FOLDER accepts.
func ComfyFolders() []string {
	out := make([]string, 0, len(comfyFolders))
	for f := range comfyFolders {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// comfyWeight: the extensions ComfyUI lists in its model folders
// (supported_pt_extensions); other files (README, configs) stay in the store.
func comfyWeight(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".safetensors", ".sft", ".ckpt", ".pt", ".pt2", ".bin", ".pth", ".pkl":
		return true
	}
	return false
}

func comfyWeights(plan *Plan) []File {
	var out []File
	for _, f := range plan.Selected() {
		if comfyWeight(f.Path) {
			out = append(out, f)
		}
	}
	return out
}

// comfyPathFolder is the deepest directory of p named as a ComfyUI folder.
func comfyPathFolder(p string) string {
	dirs := strings.Split(path.Dir(p), "/")
	for i := len(dirs) - 1; i >= 0; i-- {
		if comfyFolders[dirs[i]] {
			return dirs[i]
		}
	}
	return ""
}

// comfyByTensors reports whether f's tensors can tell its folder: only a
// safetensors file at the top of the repository, by its header.
func comfyByTensors(f File) bool {
	ext := strings.ToLower(path.Ext(f.Path))
	return !strings.Contains(f.Path, "/") && (ext == ".safetensors" || ext == ".sft")
}

// comfyDiffusers reports a repository in the diffusers layout.
func comfyDiffusers(plan *Plan) bool {
	for _, f := range plan.Files {
		if f.Path == "model_index.json" {
			return true
		}
	}
	return false
}

// comfyFolderOf is the folder f's path names, "" in a diffusers repository.
func comfyFolderOf(plan *Plan, f File) string {
	if comfyDiffusers(plan) {
		return ""
	}
	return comfyPathFolder(f.Path)
}

func comfyAccepts(plan *Plan, t Target) error {
	if plan.Ref.Source != "hf" {
		return newErr(ExitUsage, "usage", "ComfyUI's models directory holds Hugging Face files here").
			fix("--to dir:PATH", "a plain directory")
	}
	w := comfyWeights(plan)
	if len(w) == 0 {
		return newErr(ExitUsage, "usage", "ComfyUI loads weight files (*.safetensors and the like), and this selection has none").
			fix("", "pick an image model repository, e.g. a Comfy-Org one")
	}
	if t.Folder != "" {
		return comfyCollisions(w, func(File) string { return t.Folder })
	}
	for _, f := range w {
		switch {
		case comfyFolderOf(plan, f) != "" || comfyByTensors(f):
		case comfyDiffusers(plan) && strings.Contains(f.Path, "/"):
			e := newErr(ExitUsage, "usage", f.Path+" is part of this repository's diffusers layout, which ComfyUI does not load file by file").
				why("model_index.json says so: each folder is one component in diffusers' format")
			for _, r := range w {
				if !strings.Contains(r.Path, "/") {
					e = e.fix("--include '"+r.Path+"'", "a single file at the top of the repository, the kind ComfyUI loads")
				}
			}
			return e.fix("", "or a Comfy-Org repository of the same model")
		default:
			return comfyAskFolder(plan, f, "its path names no ComfyUI folder, and only a safetensors file at the top of the repository can tell by its tensors")
		}
	}
	// what the paths decide collides before anything is downloaded; what the
	// tensors decide is checked again at export
	var known []File
	for _, f := range w {
		if comfyFolderOf(plan, f) != "" {
			known = append(known, f)
		}
	}
	return comfyCollisions(known, func(f File) string { return comfyFolderOf(plan, f) })
}

// comfyAskFolder is the error that asks the user for f's folder.
func comfyAskFolder(plan *Plan, f File, why string) error {
	base := path.Base(f.Path)
	return newErr(ExitUsage, "usage", "lmget cannot tell which ComfyUI folder "+f.Path+" goes in").
		why("%s", why).
		fix("lmget "+plan.Ref.Repo+" --include '"+base+"' --to comfyui:FOLDER", "the folder the model's page names: "+
			"diffusion_models (a diffusion model alone, such as Flux), checkpoints, text_encoders, vae, loras, ...")
}

// comfyCollisions refuses two files that one name in one folder would hold,
// as a filesystem that folds case and normalizes Unicode sees names.
func comfyCollisions(files []File, folder func(File) string) error {
	seen := map[string]string{}
	for _, f := range files {
		k := folder(f) + "/" + strings.ToLower(norm.NFC.String(path.Base(f.Path)))
		if prev, ok := seen[k]; ok {
			return newErr(ExitUsage, "usage", fmt.Sprintf("%s and %s would both be %s in ComfyUI's %s folder", prev, f.Path, path.Base(f.Path), folder(f))).
				why("ComfyUI names a model by its file name in its folder").
				fix("--include PATTERN", "select one of them")
		}
		seen[k] = f.Path
	}
	return nil
}

// safetensorsKeys reads the tensor names of a safetensors file.
func safetensorsKeys(p string) ([]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var nb [8]byte
	if _, err := io.ReadFull(f, nb[:]); err != nil {
		return nil, errors.New("too short for a safetensors header")
	}
	n := binary.LittleEndian.Uint64(nb[:])
	if n > stMaxHeader || n > uint64(fi.Size()-8) {
		return nil, errors.New("the safetensors header is too long")
	}
	hdr := make([]byte, n)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(hdr, &m); err != nil {
		return nil, errors.New("the safetensors header is not a JSON object")
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "__metadata__" {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// comfyCheckpoint reports whether a stored safetensors blob is a full
// checkpoint: tensors under model.diffusion_model., as ComfyUI's checkpoint
// loader finds the diffusion model in one.
func comfyCheckpoint(blob string) (bool, error) {
	keys, err := safetensorsKeys(blob)
	if err != nil {
		return false, err
	}
	for _, k := range keys {
		if strings.HasPrefix(k, "model.diffusion_model.") {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) exportComfyUI(plan *Plan, t Target) (string, []string, error) {
	w := comfyWeights(plan)
	// every folder first: nothing is linked unless every file has one
	folders := map[string]string{}
	for _, f := range w {
		switch {
		case t.Folder != "":
			folders[f.Path] = t.Folder
		case comfyFolderOf(plan, f) != "":
			folders[f.Path] = comfyFolderOf(plan, f)
		case comfyByTensors(f):
			ckpt, err := comfyCheckpoint(c.store.BlobPath(f.SHA256))
			if err != nil {
				return "", nil, newErr(ExitFailure, "export", "cannot read the tensors of "+f.Path).wrap(err)
			}
			if !ckpt {
				return "", nil, comfyAskFolder(plan, f, "it is not a full checkpoint (no model.diffusion_model.* tensors); it is in the store, so the command below downloads nothing")
			}
			folders[f.Path] = "checkpoints"
		default: // comfyAccepts refused it before the download
			return "", nil, comfyAskFolder(plan, f, "its path names no ComfyUI folder")
		}
	}
	if err := comfyCollisions(w, func(f File) string { return folders[f.Path] }); err != nil {
		return "", nil, err
	}
	var links []string
	own := c.ownLinkCheck()
	for _, f := range w {
		dir := filepath.Join(t.Path, folders[f.Path])
		dst := filepath.Join(dir, path.Base(f.Path))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return t.Path, links, diskErr(err)
		}
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
				continue // the user's own copy of the same bytes: left as it is
			default:
				return t.Path, links, newErr(ExitFailure, "export", dst+" already exists").
					why("lmget does not overwrite files it did not create; ComfyUI names a model by its file").
					fix("", "remove it, or set LMGET_COMFYUI_DIR to another models directory")
			}
		}
		if err := linkOrSymlink(src, dst); err != nil {
			return t.Path, links, diskErr(err)
		}
		links = append(links, dst)
	}
	return t.Path, links, nil
}

// comfyModels finds ComfyUI's models directory when LMGET_COMFYUI_DIR is not
// set: ComfyUI has no fixed place, it runs from wherever it is installed.
// lmget looks where the two installers put it, and uses what exists:
//
//	ComfyUI Desktop    basePath in <app data>/ComfyUI/config.json (macOS,
//	                   Windows), + /models
//	comfy-cli          its default workspace, ~/Documents/comfy/ComfyUI on
//	                   macOS and Windows, ~/comfy/ComfyUI on Linux, + /models
func comfyModels(home string) (string, error) {
	var tried []string
	if cfg := comfyDesktopConfig(home); cfg != "" {
		base, err := comfyBasePath(cfg)
		switch {
		case err != nil:
			return "", newErr(ExitUsage, "usage", "cannot read ComfyUI's base path from "+cfg).
				why("%s", err.Error()).fix("", "set LMGET_COMFYUI_DIR to ComfyUI's models directory")
		case base != "":
			if !filepath.IsAbs(base) {
				return "", newErr(ExitUsage, "usage", "ComfyUI's base path in "+cfg+" is not absolute: "+quote(base)).
					fix("", "set LMGET_COMFYUI_DIR to ComfyUI's models directory")
			}
			m := filepath.Join(filepath.Clean(base), "models")
			if isDir(m) {
				return m, nil
			}
			tried = append(tried, m)
		}
	}
	ws := filepath.Join(home, "comfy", "ComfyUI", "models")
	if runtime.GOOS != "linux" {
		ws = filepath.Join(home, "Documents", "comfy", "ComfyUI", "models")
	}
	if isDir(ws) {
		return ws, nil
	}
	tried = append(tried, ws)
	return "", newErr(ExitUsage, "usage", "lmget cannot find ComfyUI's models directory").
		why("it looked in %s", strings.Join(tried, " and ")).
		fix("", "set LMGET_COMFYUI_DIR to ComfyUI's models directory (the folder that holds checkpoints/ and vae/)")
}

// comfyDesktopConfig is ComfyUI Desktop's config.json, "" off macOS and
// Windows (Desktop keeps no such file on Linux).
func comfyDesktopConfig(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "ComfyUI", "config.json")
	case "windows":
		if a := os.Getenv("APPDATA"); a != "" {
			return filepath.Join(a, "ComfyUI", "config.json")
		}
		return filepath.Join(home, "AppData", "Roaming", "ComfyUI", "config.json")
	}
	return ""
}

// comfyBasePath reads basePath from Desktop's config.json, "" when the file
// or the key is absent. Only that key is decoded.
func comfyBasePath(p string) (string, error) {
	body, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var s struct {
		BasePath string `json:"basePath"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return "", errors.New("not valid JSON")
	}
	return s.BasePath, nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
