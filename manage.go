package lmget

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Installed is one model at one commit, as recorded in the store.
type Installed struct {
	Ref      Ref            `json:"ref"`
	Commit   string         `json:"commit"`
	Bytes    int64          `json:"bytes"`
	Files    int            `json:"files"`
	Missing  int            `json:"missing"` // files whose blob is not in the store
	Exports  []ExportRecord `json:"exports,omitempty"`
	Updated  time.Time      `json:"updated"`
	manifest string
}

// StoreStats describes the store on disk.
type StoreStats struct {
	Root         string `json:"root"`
	Blobs        int    `json:"blobs"`
	Bytes        int64  `json:"bytes"`
	Partial      int    `json:"partial"` // downloads in progress, resumable
	PartialBytes int64  `json:"partial_bytes"`
	DiskFree     int64  `json:"disk_free"`
}

// List returns the models recorded in the store, sorted by repository.
func (c *Client) List() ([]Installed, error) {
	ms, _, err := c.store.Manifests()
	if err != nil {
		return nil, err
	}
	var out []Installed
	for path, m := range ms {
		in := Installed{Ref: m.Plan.Ref, Commit: m.Plan.Commit, Exports: m.Exports, Updated: m.Updated, manifest: path}
		if in.Ref.Source == "hf" { // an Ollama model keeps its tag: rm works by tag
			in.Ref.Revision = "main"
		}
		in.Ref.File, in.Ref.SHA256 = "", ""
		for _, f := range m.Plan.Selected() {
			in.Bytes += f.Size
			in.Files++
			if !c.store.Has(f.SHA256, f.Size) {
				in.Missing++
			}
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ref.Repo != out[j].Ref.Repo {
			return strings.ToLower(out[i].Ref.Repo) < strings.ToLower(out[j].Ref.Repo)
		}
		return out[i].Updated.After(out[j].Updated)
	})
	return out, nil
}

// Stats walks the store.
func (c *Client) Stats() StoreStats {
	st := StoreStats{Root: c.store.Root(), DiskFree: diskFree(c.store.Root())}
	entries, _ := os.ReadDir(filepath.Join(c.store.Root(), "blobs", "sha256"))
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() {
			st.Blobs++
			st.Bytes += fi.Size()
		}
	}
	tmp, _ := os.ReadDir(filepath.Join(c.store.Root(), "tmp"))
	for _, e := range tmp {
		if strings.HasSuffix(e.Name(), ".part") {
			st.Partial++
			if fi, err := e.Info(); err == nil {
				st.PartialBytes += allocated(fi)
			}
		}
	}
	return st
}

// RemoveResult reports what Remove did.
type RemoveResult struct {
	Models []string `json:"models"` // repo@commit removed
	Links  int      `json:"links"`  // export links removed
	Blobs  int      `json:"blobs"`  // blobs deleted from the store
	Freed  int64    `json:"freed"`  // bytes released on disk
	Held   int64    `json:"held"`   // bytes of deleted blobs still held by other hard links
}

// Remove deletes a model from the store: the export links lmget created
// (only those still pointing at lmget's data), its manifests, then every
// blob no other model references. Revision "main" removes every recorded
// commit of the repository; otherwise it must be a commit or a prefix of one.
func (c *Client) Remove(ref Ref) (*RemoveResult, error) {
	unlock, err := c.store.flock(".lock", true, false) // no pull may run meanwhile
	if err != nil {
		return nil, err
	}
	defer unlock()
	all, err := c.List()
	if err != nil {
		return nil, err
	}
	var hit []Installed
	for _, in := range all {
		if in.Ref.Source != ref.Source {
			continue
		}
		if ref.Source == "url" { // by its id: the stored URL has its query redacted
			if in.Commit == urlID(ref.Repo) {
				hit = append(hit, in)
			}
			continue
		}
		if !strings.EqualFold(in.Ref.Repo, ref.Repo) {
			continue
		}
		byTag := (ref.Source == "ollama" || ref.Source == "oci") && in.Ref.Revision == ref.Revision
		if byTag || ref.Revision == "main" || ref.Revision == "" || (len(ref.Revision) >= 7 && strings.HasPrefix(in.Commit, ref.Revision)) {
			hit = append(hit, in)
		}
	}
	if len(hit) == 0 {
		return nil, newErr(ExitNotFound, "not_found", ref.Display()+" is not in the store").
			fix("lmget ls", "list what is stored")
	}
	// links that models not being removed still record: an export directory,
	// the HF cache and Ollama's blobs directory are shared, and a link two
	// records list belongs to both
	removing := map[string]bool{}
	for _, in := range hit {
		removing[in.manifest] = true
	}
	keep := map[string]bool{}
	for _, in := range all {
		if removing[in.manifest] {
			continue
		}
		m, err := readManifest(in.manifest)
		if err != nil {
			return nil, err
		}
		for _, ex := range m.Exports {
			for _, l := range ex.Links {
				keep[filepath.Clean(l)] = true
			}
		}
	}
	res := &RemoveResult{}
	for _, in := range hit {
		m, err := readManifest(in.manifest)
		if err != nil {
			return nil, err
		}
		owned := c.blobInodes(m.Plan)
		var failed []string
		for _, ex := range m.Exports {
			n, f := c.unlinkExport(ex, owned, m.Plan, keep)
			res.Links += n
			failed = append(failed, f...)
		}
		if len(failed) > 0 { // keep the manifest: it is the only record of these links
			return nil, newErr(ExitDisk, "disk", fmt.Sprintf("could not remove %d export links of %s", len(failed), in.Ref.Display())).
				why("for example %s; the model stays recorded so nothing is forgotten", failed[0]).
				fix("", "fix the permissions and run lmget rm again")
		}
		if err := os.Remove(in.manifest); err != nil {
			return nil, diskErr(err)
		}
		removeEmptyUp(filepath.Dir(in.manifest), filepath.Join(c.store.Root(), "refs"))
		name := in.Ref.Repo
		if in.Ref.Source == "ollama" || in.Ref.Source == "oci" {
			name = in.Ref.Display()
		}
		res.Models = append(res.Models, name+"@"+in.Commit[:7])
	}
	gc, err := c.gc()
	if err != nil {
		return nil, err
	}
	res.Blobs, res.Freed, res.Held = gc.Blobs, gc.Freed, gc.Held
	return res, nil
}

type inode struct{ dev, ino uint64 }

func (c *Client) blobInodes(p *Plan) map[inode]bool {
	out := map[inode]bool{}
	for _, f := range p.Selected() {
		if !hex64.MatchString(f.SHA256) {
			continue
		}
		if fi, err := os.Stat(c.store.BlobPath(f.SHA256)); err == nil {
			if in, ok := inodeOf(c.store.BlobPath(f.SHA256), fi); ok {
				out[in] = true
			}
		}
	}
	return out
}

// unlinkExport removes the recorded links that are still lmget's: a hard
// link to one of the model's blobs, or a symlink into the store or into the
// cache's own blobs directory. Anything else (replaced or edited by the user,
// or by another tool) is left alone, and so is a link another record lists
// (keep). Blob links in a shared, digest-named directory (the HF cache's
// blobs, Ollama's blobs) go last, and only when nothing left there names
// them: no remaining snapshot symlink, no remaining Ollama tag file.
func (c *Client) unlinkExport(ex ExportRecord, owned map[inode]bool, plan *Plan, keep map[string]bool) (int, []string) {
	var failed []string
	sep := string(os.PathSeparator)
	storeBlobs := filepath.Join(c.store.Root(), "blobs", "sha256") + sep
	// top: the export's own directory. stop: empty directories are pruned up
	// to here and never above (the HF cache root, or the export directory).
	var top, stop, repoBlobs, sharedBlobs string
	switch ex.Kind {
	case "hf": // ex.Path is <cache>/models--org--name/snapshots/<commit>
		top = filepath.Dir(filepath.Dir(ex.Path))
		stop = filepath.Dir(top)
		repoBlobs = filepath.Join(top, "blobs") + sep
		sharedBlobs = repoBlobs
	case "ollama": // ex.Path is Ollama's models directory
		top, stop = ex.Path, ex.Path
		sharedBlobs = filepath.Join(ex.Path, "blobs") + sep
	default:
		top, stop = ex.Path, ex.Path
	}
	var own, blobs []string
	for _, l := range ex.Links {
		if sharedBlobs != "" && strings.HasPrefix(filepath.Clean(l), sharedBlobs) {
			blobs = append(blobs, l)
		} else {
			own = append(own, l)
		}
	}
	n := 0
	remove := func(l string, inUse map[string]bool) {
		if keep[filepath.Clean(l)] || inUse[filepath.Base(l)] {
			return // another model still uses it
		}
		fi, err := os.Lstat(l)
		if err != nil {
			return
		}
		mine := false
		if fi.Mode()&os.ModeSymlink != 0 {
			if t, err := os.Readlink(l); err == nil {
				if !filepath.IsAbs(t) {
					t = filepath.Join(filepath.Dir(l), t)
				}
				t = filepath.Clean(t)
				mine = strings.HasPrefix(t, storeBlobs) || (ex.Kind == "hf" && strings.HasPrefix(t, repoBlobs))
			}
		} else if in, ok := inodeOf(l, fi); ok && fi.Mode().IsRegular() {
			mine = owned[in] || c.store.storedCopy(l, fi) // a recorded link, or lmget's copy (Windows)
			if !mine && ex.Kind == "ollama" && plan.Ref.Source == "ollama" {
				// the tag file is a copy of the manifest, whose sha256 is the commit
				mine = hasContent(l, fi.Size(), plan.Commit)
			}
		}
		if !mine && fi.Mode().IsRegular() && ourSidecar(l, plan) { // Jan's model.yml, exo's manifest
			mine = true
		}
		if !mine {
			return
		}
		if err := os.Remove(l); err != nil && !os.IsNotExist(err) {
			failed = append(failed, l)
			return
		}
		n++
		removeEmptyUp(filepath.Dir(l), stop)
	}
	for _, l := range own {
		remove(l, nil)
	}
	var inUse map[string]bool
	switch ex.Kind {
	case "hf":
		inUse = snapshotTargets(filepath.Join(top, "snapshots"))
	case "ollama":
		inUse = ollamaTagBlobs(filepath.Join(ex.Path, "manifests"))
	}
	for _, l := range blobs {
		remove(l, inUse)
	}
	if ex.Kind == "hf" { // refs/<revision> pointing at a snapshot that no longer exists
		commit := filepath.Base(ex.Path)
		if _, err := os.Stat(ex.Path); os.IsNotExist(err) {
			refs := filepath.Join(top, "refs")
			filepath.WalkDir(refs, func(p string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == commit {
						os.Remove(p)
						removeEmptyUp(filepath.Dir(p), top)
					}
				}
				return nil
			})
			removeEmptyUp(top, stop)
		}
	}
	return n, failed
}

// snapshotTargets returns the blob names the snapshot symlinks left in an HF
// repository directory point to.
func snapshotTargets(snapshots string) map[string]bool {
	out := map[string]bool{}
	filepath.WalkDir(snapshots, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type()&os.ModeSymlink != 0 {
			if t, err := os.Readlink(p); err == nil {
				out[filepath.Base(t)] = true
			}
		}
		return nil
	})
	return out
}

// ollamaTagBlobs returns the blob file names (sha256-<hex>) that the tag
// files left in Ollama's manifests directory name, whoever wrote them.
func ollamaTagBlobs(manifests string) map[string]bool {
	out := map[string]bool{}
	filepath.WalkDir(manifests, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if fi, err := d.Info(); err != nil || fi.Size() > ollamaMaxManifest {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var m ollamaManifest
		if json.Unmarshal(b, &m) != nil {
			return nil
		}
		for _, l := range append([]ollamaDescriptor{m.Config}, m.Layers...) {
			if mm := ollamaDigestRe.FindStringSubmatch(l.Digest); mm != nil {
				out["sha256-"+mm[1]] = true
			}
		}
		return nil
	})
	return out
}

// removeEmptyUp removes dir and its parents while they are empty, stopping
// at (and never removing) stop.
func removeEmptyUp(dir, stop string) {
	dir, stop = filepath.Clean(dir), filepath.Clean(stop)
	for dir != stop && strings.HasPrefix(dir, stop+string(os.PathSeparator)) {
		if os.Remove(dir) != nil { // fails when not empty
			return
		}
		dir = filepath.Dir(dir)
	}
}

type gcResult struct {
	Blobs int
	Freed int64
	Held  int64
}

// gc deletes blobs that no manifest references, and their git index entries.
func (c *Client) gc() (gcResult, error) {
	var r gcResult
	ms, bad, err := c.store.Manifests()
	if err != nil {
		return r, err
	}
	if len(bad) > 0 {
		return r, newErr(ExitFailure, "store", fmt.Sprintf("%d manifests cannot be read: no blob was deleted", len(bad))).
			why("an unreadable manifest still owns its files, e.g. %s", bad[0]).
			fix("lmget doctor", "lists them")
	}
	keep := map[string]bool{}
	for _, m := range ms {
		for _, f := range m.Plan.Selected() {
			keep[f.SHA256] = true
			if f.GitSHA1 != "" { // downloaded before the manifest learned its sha256
				if s, ok := c.store.LookupGit(f.GitSHA1); ok {
					keep[s] = true
				}
			}
		}
	}
	dir := filepath.Join(c.store.Root(), "blobs", "sha256")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return r, err
	}
	gone := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !hex64.MatchString(name) || keep[name] {
			continue
		}
		p := filepath.Join(dir, name)
		fi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		links := linkCount(p, fi)
		if err := os.Remove(p); err != nil {
			return r, diskErr(err)
		}
		r.Blobs++
		gone[name] = true
		if links <= 1 {
			r.Freed += fi.Size()
		} else {
			r.Held += fi.Size()
		}
	}
	idx := filepath.Join(c.store.Root(), "index", "git-sha1")
	ents, _ := os.ReadDir(idx)
	for _, e := range ents {
		if b, err := os.ReadFile(filepath.Join(idx, e.Name())); err == nil && gone[string(b)] {
			os.Remove(filepath.Join(idx, e.Name()))
		}
	}
	return r, nil
}
