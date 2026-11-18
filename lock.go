package lmget

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// LockVersion is the lmget.lock format version.
const LockVersion = 1

// DefaultLockName is the lockfile lmget looks for in the current directory.
const DefaultLockName = "lmget.lock"

// LockFile pins one file of a model by size and sha256.
type LockFile struct {
	Path    string `toml:"path" json:"path"`
	Size    int64  `toml:"size" json:"size"`
	SHA256  string `toml:"sha256" json:"sha256"`
	GitSHA1 string `toml:"git_sha1,omitempty" json:"git_sha1,omitempty"`
}

// LockModel pins one model: the reference asked for, the commit it resolved
// to, how it was selected and exported, and every file.
type LockModel struct {
	Ref         string     `toml:"ref" json:"ref"`
	Commit      string     `toml:"commit" json:"commit"`
	To          string     `toml:"to" json:"to"`
	License     string     `toml:"license,omitempty" json:"license,omitempty"`
	Include     []string   `toml:"include,omitempty" json:"include,omitempty"`
	Exclude     []string   `toml:"exclude,omitempty" json:"exclude,omitempty"`
	AllowPickle bool       `toml:"allow_pickle,omitempty" json:"allow_pickle,omitempty"`
	Files       []LockFile `toml:"file" json:"files"`
}

// Lock is the content of lmget.lock.
type Lock struct {
	Version int         `toml:"version"`
	Models  []LockModel `toml:"model"`
	path    string
}

const lockHeader = `# lmget.lock: the models of this project, pinned by commit and sha256.
# Written by lmget. Reproduce with "lmget sync", move to newer revisions
# with "lmget sync --update".

`

func lockErr(path, msg string) *Error {
	return newErr(ExitUsage, "lock", path+": "+msg).
		fix("", "fix the file, or delete it and run lmget get <model> --lock")
}

// NewLock returns an empty lock that will be written to path.
func NewLock(path string) *Lock { return &Lock{Version: LockVersion, path: path} }

func (l *Lock) Path() string { return l.path }

// ReadLock reads and validates a lockfile. Every value that later becomes a
// path or a URL is checked here, so a hand-edited lock cannot point outside
// the store or the target. A missing file returns an error matching
// fs.ErrNotExist.
func ReadLock(path string) (*Lock, error) {
	// one open, without following a symlink, then checks on that descriptor:
	// nothing can be swapped between the check and the read
	f, err := openNoFollow(path)
	if err != nil {
		if errors.Is(err, errIsSymlink) {
			return nil, lockErr(path, "is a symlink; lmget only reads and writes a regular lockfile")
		}
		return nil, err // fs.ErrNotExist passes through for callers
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 64<<20 {
		return nil, lockErr(path, "is not a regular file of a sensible size")
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var l Lock
	md, err := toml.Decode(string(b), &l)
	if err != nil {
		return nil, lockErr(path, "not valid TOML").wrap(err)
	}
	l.path = path
	if l.Version != LockVersion {
		return nil, lockErr(path, fmt.Sprintf("version %d is not supported (this lmget reads version %d)", l.Version, LockVersion))
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, lockErr(path, "unknown key "+quote(und[0].String()))
	}
	seen := map[string]bool{}
	for i, m := range l.Models {
		where := fmt.Sprintf("model %d (%s)", i+1, printable(m.Ref))
		ref, err := ParseRef(m.Ref)
		if err != nil {
			return nil, lockErr(path, where+": "+AsError(err).Msg)
		}
		if ref.SHA256 != "" || ref.File != "" {
			// lmget writes canonical refs; an expected hash belongs in [[model.file]]
			return nil, lockErr(path, where+": a ref in the lock carries no #sha256= fragment or file path")
		}
		if seen[ref.String()] {
			return nil, lockErr(path, where+": listed twice")
		}
		seen[ref.String()] = true
		if !validCommit(ref, m.Commit) {
			return nil, lockErr(path, where+": commit is not a valid id (40 hex characters, 64 for Ollama)")
		}
		if err := lockTargetSyntax(m.To); err != nil {
			return nil, lockErr(path, where+": "+err.Error())
		}
		if len(m.Files) == 0 {
			return nil, lockErr(path, where+": no files")
		}
		if printable(m.License) != m.License {
			return nil, lockErr(path, where+": control characters in license")
		}
		if ref.Source == "url" {
			// a URL's commit is its own id, and a URL is one file: anything else
			// would let a lock write into another model's record
			if m.Commit != urlID(ref.Repo) || len(m.Files) != 1 {
				return nil, lockErr(path, where+": a URL entry needs commit "+urlID(ref.Repo)+" and exactly one file")
			}
		}
		if (ref.Source == "ollama" || ref.Source == "oci") && !hasManifestFile(m) {
			// the manifest is the model's identity: its file must be the commit
			return nil, lockErr(path, where+": an Ollama entry needs the file "+OllamaManifestFile+" with sha256 equal to the commit")
		}
		files := map[string]bool{}
		for _, f := range m.Files {
			switch {
			case !isLocalSlash(f.Path) || files[f.Path]:
				return nil, lockErr(path, where+": bad or duplicate file path "+quote(f.Path))
			case !hex64.MatchString(f.SHA256):
				return nil, lockErr(path, where+": "+f.Path+": sha256 is not 64 lowercase hex characters")
			case f.GitSHA1 != "" && !hex40.MatchString(f.GitSHA1):
				return nil, lockErr(path, where+": "+f.Path+": git_sha1 is not 40 lowercase hex characters")
			case f.Size < 0:
				return nil, lockErr(path, where+": "+f.Path+": negative size")
			}
			files[f.Path] = true
		}
	}
	return &l, nil
}

// Write saves the lock atomically, models sorted by ref and files by path,
// so the file diffs cleanly.
func (l *Lock) Write() error {
	if fi, err := os.Lstat(l.path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return lockErr(l.path, "is a symlink; lmget only reads and writes a regular lockfile")
	}
	sort.Slice(l.Models, func(i, j int) bool { return l.Models[i].Ref < l.Models[j].Ref })
	for i := range l.Models {
		fs := l.Models[i].Files
		sort.Slice(fs, func(a, b int) bool { return fs[a].Path < fs[b].Path })
	}
	var b bytes.Buffer
	b.WriteString(lockHeader)
	enc := toml.NewEncoder(&b)
	enc.Indent = ""
	if err := enc.Encode(l); err != nil {
		return err
	}
	if err := writeFileAtomic(l.path, b.Bytes()); err != nil {
		return newErr(ExitDisk, "disk", "cannot write "+l.path).wrap(err)
	}
	return nil
}

// UpdateLock runs fn on the lockfile at path while holding an exclusive
// advisory lock on its directory, and writes the lock when fn reports a
// change. The directory, not the file, is locked: Write replaces the file by
// rename, so a lock on its inode would not exclude the next writer. With
// create, a missing lockfile starts empty. Every read-modify-write of
// lmget.lock goes through here, so two lmget runs cannot lose each other's
// updates.
func UpdateLock(path string, create bool, fn func(*Lock) (bool, error)) (bool, error) {
	unlock, err := lockDir(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	defer unlock()
	l, err := ReadLock(path)
	if create && errors.Is(err, fs.ErrNotExist) {
		l, err = NewLock(path), nil
	}
	if err != nil {
		return false, err
	}
	changed, err := fn(l)
	if err != nil || !changed {
		return false, err
	}
	return true, l.Write()
}

// UpdateLockEntry records the pull that reproduced an entry of the lock,
// which read as before when the sync started. If another lmget changed that
// entry meanwhile, its newer version is kept and nothing is written. It
// reports whether the lockfile was rewritten.
func UpdateLockEntry(path string, before LockModel, p *Plan, sel Selection, t Target) (bool, error) {
	return UpdateLock(path, false, func(cur *Lock) (bool, error) {
		if now := cur.Find(p.Ref); now == nil || !reflect.DeepEqual(*now, before) {
			return false, nil
		}
		if err := cur.Upsert(p, sel, t); err != nil {
			return false, err
		}
		return !reflect.DeepEqual(*cur.Find(p.Ref), before), nil
	})
}

// Find returns the entry for ref (same source, repo and revision), or nil.
func (l *Lock) Find(ref Ref) *LockModel {
	for i := range l.Models {
		if r, err := ParseRef(l.Models[i].Ref); err == nil && r.String() == ref.String() {
			return &l.Models[i]
		}
	}
	return nil
}

// Upsert records a completed pull. Every selected file must have its sha256.
func (l *Lock) Upsert(p *Plan, sel Selection, t Target) error {
	if err := Lockable(p.Ref); err != nil {
		return err
	}
	to, err := l.targetString(t)
	if err != nil {
		return err
	}
	lic := p.License
	if lic == "" && p.Ref.Source == "hf" {
		lic = LicenseNone // recorded, so a license that appears later is a change too
	}
	m := LockModel{
		Ref: p.Ref.String(), Commit: p.Commit, To: to, License: lic,
		Include: sel.Include, Exclude: sel.Exclude, AllowPickle: sel.AllowPickle,
	}
	for _, f := range p.Selected() {
		if !hex64.MatchString(f.SHA256) {
			return fmt.Errorf("lock: %s has no sha256 yet", f.Path)
		}
		m.Files = append(m.Files, LockFile{Path: f.Path, Size: f.Size, SHA256: f.SHA256, GitSHA1: f.GitSHA1})
	}
	if cur := l.Find(p.Ref); cur != nil {
		*cur = m
	} else {
		l.Models = append(l.Models, m)
	}
	return nil
}

// Lockable reports whether ref may be written to a lockfile. A URL with a
// query string is refused: signed URLs carry credentials there, and a
// lockfile ends up in git.
func Lockable(ref Ref) error {
	if ref.Source == "url" && strings.Contains(ref.Repo, "?") {
		return newErr(ExitUsage, "usage", "this URL has a query string and is not written to the lock").
			why("signed URLs carry credentials in the query, and lmget.lock is meant to be committed").
			fix("", "download it without --lock, or use a URL without a query string")
	}
	return nil
}

// lockTargetSyntax accepts hf, none, and dir: targets that are relative
// and stay inside the lockfile's directory. A lockfile arrives with a cloned
// repository: it must not be able to send files anywhere else.
func lockTargetSyntax(to string) error {
	switch {
	case to == "hf" || to == "none" || to == "ollama" || to == "omlx" || to == "lmstudio":
		return nil
	case strings.HasPrefix(to, "dir:"):
		p := strings.TrimPrefix(to[4:], "./")
		if p == "." || p == "" {
			return nil
		}
		if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || !isLocalSlash(path.Clean(p)) {
			return fmt.Errorf("target %q is outside the project: a lock may only name directories inside its own", to)
		}
		return nil
	}
	return fmt.Errorf("unknown target %q", to)
}

// within checks that dst, with symlinks resolved as far as it exists, is
// base or below it.
func within(base, dst string) error {
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return err
	}
	real, err := resolveExisting(dst)
	if err != nil {
		return err
	}
	if real != realBase && !strings.HasPrefix(real, realBase+string(os.PathSeparator)) {
		return fmt.Errorf("target resolves to %s, outside the project %s", real, realBase)
	}
	return nil
}

// resolveExisting resolves symlinks in the longest existing prefix of p and
// appends the part that does not exist yet.
func resolveExisting(p string) (string, error) {
	cur, rest := filepath.Clean(p), ""
	for {
		if _, err := os.Lstat(cur); err == nil {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
	realCur, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return "", err
	}
	return filepath.Join(realCur, rest), nil
}

// targetString stores a directory target relative to the lockfile. Targets
// outside the lockfile's directory cannot be locked.
func (l *Lock) targetString(t Target) (string, error) {
	if t.Kind != "dir" {
		if t.Kind == "" {
			return "hf", nil
		}
		return t.Kind, nil
	}
	base, err := filepath.Abs(filepath.Dir(l.path))
	if err != nil {
		return "", err
	}
	if err := within(base, t.Path); err != nil {
		return "", lockTargetErr(t.Path)
	}
	rel, err := filepath.Rel(base, t.Path)
	if err != nil || !(rel == "." || filepath.IsLocal(rel)) {
		return "", lockTargetErr(t.Path)
	}
	if rel == "." {
		return "dir:.", nil
	}
	return "dir:./" + filepath.ToSlash(rel), nil
}

func lockTargetErr(p string) error {
	return newErr(ExitUsage, "usage", p+" is outside the project and cannot be recorded in the lock").
		why("a lock may only name directories inside its own, so a cloned lock cannot write elsewhere").
		fix("", "export inside the project, or download without --lock")
}

// LockableTarget reports whether t can be recorded in the lock at lockPath.
func LockableTarget(lockPath string, t Target) error {
	_, err := NewLock(lockPath).targetString(t)
	return err
}

// Request builds the pull that reproduces m. With update, the revision is
// resolved again instead of using the pinned commit.
func (l *Lock) Request(m *LockModel, update bool) (Request, error) {
	ref, err := ParseRef(m.Ref)
	if err != nil {
		return Request{}, err
	}
	if err := lockTargetSyntax(m.To); err != nil {
		return Request{}, lockErr(l.path, m.Ref+": "+err.Error())
	}
	to := m.To
	if strings.HasPrefix(to, "dir:") {
		base, err := filepath.Abs(filepath.Dir(l.path))
		if err != nil {
			return Request{}, err
		}
		dst := filepath.Join(base, filepath.FromSlash(to[4:]))
		if err := within(base, dst); err != nil {
			return Request{}, lockErr(l.path, m.Ref+": "+err.Error())
		}
		to = "dir:" + dst
	}
	t, err := ParseTarget(to)
	if err != nil {
		return Request{}, err
	}
	r := Request{Ref: ref, Target: t, Selection: Selection{Include: m.Include, Exclude: m.Exclude, AllowPickle: m.AllowPickle},
		License: m.License}
	if !update {
		r.Pin = m
	}
	return r, nil
}

// resolvePinned builds the plan of a locked entry. When every file is in the
// store it does not touch the network. Otherwise it resolves the pinned
// commit and requires the source to match the lock exactly.
func (c *Client) resolvePinned(ctx context.Context, r Request) (*Plan, error) {
	pin := r.Pin
	all := true
	for _, f := range pin.Files {
		if !c.store.Has(f.SHA256, f.Size) {
			all = false
			break
		}
	}
	// Offline only when a pull that checked the source has already bound
	// these exact paths to these exact hashes for this repository and commit.
	// A lockfile alone must not be able to re-label stored bytes as another
	// model (sec-audit finding offline-plan-trusts-lock-repo-commit-file-mapping).
	if all {
		if m, err := c.store.loadManifest(r.Ref, pin.Commit); err == nil && boundAll(c.store, m, pin.Files) {
			p := &Plan{Ref: r.Ref, Commit: pin.Commit, License: lockedLicense(pin.License), offline: true}
			for _, f := range pin.Files {
				p.Files = append(p.Files, File{Path: f.Path, Size: f.Size, SHA256: f.SHA256, GitSHA1: f.GitSHA1, Cached: true})
			}
			p.recount(c.store)
			return p, nil
		}
	}
	var p *Plan
	var err error
	if r.Ref.Source == "ollama" {
		p, err = c.resolveOllamaPinned(ctx, r.Ref, pin.Commit, r.Selection)
	} else {
		at := r.Ref
		at.Revision = pin.Commit
		p, err = c.Resolve(ctx, at, r.Selection)
	}
	if err != nil && sourceDown(err) { // the signed index stands in for an unreachable source
		c.ensureIndex(ctx)
		if _, m := c.indexEntry(r.Ref, pin.Commit); m != nil {
			p, err = c.planFromIndex(r.Ref, m, r.Selection)
		}
	}
	if err != nil {
		return nil, err
	}
	p.Ref = r.Ref
	drift := func(format string, a ...any) error {
		return newErr(ExitVerify, "lock", "the source does not match the lock for "+r.Ref.Display()).
			why(format, a...).
			fix("lmget sync --update "+r.Ref.Display(), "accept what the source publishes now")
	}
	want := map[string]LockFile{}
	for _, f := range pin.Files {
		want[f.Path] = f
	}
	for i := range p.Files {
		f := &p.Files[i]
		if !f.Selected() {
			continue
		}
		lf, ok := want[f.Path]
		switch {
		case !ok:
			return nil, drift("%s is selected at commit %s but not in the lock", f.Path, pin.Commit[:7])
		case f.Size != lf.Size:
			return nil, drift("%s: the source says %d bytes, the lock %d", f.Path, f.Size, lf.Size)
		case f.SHA256 != "" && f.SHA256 != lf.SHA256:
			return nil, drift("%s: the source publishes sha256 %s, the lock has %s", f.Path, short(f.SHA256), short(lf.SHA256))
		case f.GitSHA1 != "" && lf.GitSHA1 != "" && f.GitSHA1 != lf.GitSHA1:
			return nil, drift("%s: the source publishes git id %s, the lock has %s", f.Path, short(f.GitSHA1), short(lf.GitSHA1))
		}
		if f.GitSHA1 != "" {
			// the source names this file only by git id: the lock's sha256 is
			// trusted only through a verified download of that git id
			// (a known git id already filled f.SHA256 in Resolve, and a
			// different locked sha256 was refused as drift above)
			if _, ok := c.store.LookupGit(f.GitSHA1); !ok {
				f.mustFetch = true
			}
		}
		if r.Ref.Source == "url" {
			// whatever the hash came from (server, #sha256= fragment, first
			// download), the stored blob stands for this URL only when a real
			// download of it verified that binding before
			if m, err := c.store.loadManifest(r.Ref, pin.Commit); err != nil || m.Verified[f.Path] != lf.SHA256 {
				f.mustFetch = true
			}
		}
		// verify will now check the lock's sha256 too, also for git-only files
		f.SHA256 = lf.SHA256
		delete(want, f.Path)
	}
	for path := range want {
		return nil, drift("%s is in the lock but not selected at commit %s", path, pin.Commit[:7])
	}
	p.recount(c.store)
	return p, nil
}

// boundAll reports whether every locked file is a verified binding of m, and
// a locked git id (used as the blob name in the HF cache) maps to the same
// sha256 through the index that only verified downloads write.
func boundAll(st *Store, m *Manifest, files []LockFile) bool {
	if m == nil || len(m.Verified) == 0 {
		return false
	}
	for _, f := range files {
		if m.Verified[f.Path] != f.SHA256 {
			return false
		}
		if f.GitSHA1 != "" {
			if s, ok := st.LookupGit(f.GitSHA1); !ok || s != f.SHA256 {
				return false
			}
		}
	}
	return true
}

func hasManifestFile(m LockModel) bool {
	for _, f := range m.Files {
		if f.Path == OllamaManifestFile && f.SHA256 == m.Commit {
			return true
		}
	}
	return false
}

// LockPathFor returns the lockfile path in dir.
func LockPathFor(dir string) string { return filepath.Join(dir, DefaultLockName) }

// LicenseNone is what a lock records for a model whose source declares no
// license: an empty field means "not recorded" (a lock written before it),
// which is not compared.
const LicenseNone = "none"

// lockedLicense is a lock's license as the source would declare it.
func lockedLicense(s string) string {
	if s == LicenseNone {
		return ""
	}
	return s
}
