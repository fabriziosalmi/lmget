package lmget

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
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
	var l Lock
	md, err := toml.DecodeFile(path, &l)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
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
		where := fmt.Sprintf("model %d (%s)", i+1, m.Ref)
		ref, err := ParseRef(m.Ref)
		if err != nil {
			return nil, lockErr(path, where+": "+AsError(err).Msg)
		}
		if seen[ref.String()] {
			return nil, lockErr(path, where+": listed twice")
		}
		seen[ref.String()] = true
		if !hex40.MatchString(m.Commit) {
			return nil, lockErr(path, where+": commit is not a 40-character hex id")
		}
		if err := lockTargetSyntax(m.To); err != nil {
			return nil, lockErr(path, where+": "+err.Error())
		}
		if len(m.Files) == 0 {
			return nil, lockErr(path, where+": no files")
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
	m := LockModel{
		Ref: p.Ref.String(), Commit: p.Commit, To: to, License: p.License,
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
	case to == "hf" || to == "none":
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
	cur, rest := filepath.Clean(dst), ""
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
		return err
	}
	real := filepath.Join(realCur, rest)
	if real != realBase && !strings.HasPrefix(real, realBase+string(os.PathSeparator)) {
		return fmt.Errorf("target resolves to %s, outside the project %s", real, realBase)
	}
	return nil
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
	r := Request{Ref: ref, Target: t, Selection: Selection{Include: m.Include, Exclude: m.Exclude, AllowPickle: m.AllowPickle}}
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
	if all {
		p := &Plan{Ref: r.Ref, Commit: pin.Commit, License: pin.License}
		for _, f := range pin.Files {
			p.Files = append(p.Files, File{Path: f.Path, Size: f.Size, SHA256: f.SHA256, GitSHA1: f.GitSHA1, Cached: true})
		}
		p.recount(c.store)
		return p, nil
	}
	at := r.Ref
	at.Revision = pin.Commit
	p, err := c.Resolve(ctx, at, r.Selection)
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

// LockPathFor returns the lockfile path in dir.
func LockPathFor(dir string) string { return filepath.Join(dir, DefaultLockName) }
