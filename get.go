package lmget

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Request is one pull: resolve, fetch, verify, export.
type Request struct {
	Ref       Ref
	Selection Selection
	Target    Target
	DryRun    bool       // stop after the plan
	Pin       *LockModel // reproduce this locked entry: its commit, files and hashes
	// License is the license the lockfile recorded for this model. When it is
	// set and the source now declares another one (or none), the pull stops
	// before downloading, unless AcceptLicenseChange is set.
	License             string
	AcceptLicenseChange bool
	// Licenses are the licenses the project's policy allows (lmget.lock
	// [policy]); Options.Licenses applies too. A model must be allowed by
	// both, before anything is downloaded.
	Licenses []string
}

// Outcome is the result of a successful pull.
type Outcome struct {
	Plan    *Plan
	Path    string // export path ("" for dry runs or Target none)
	Fetched int64  // bytes received from the network in this run
	RunAs   string // Ollama export: the name for ollama run
	Elapsed time.Duration
}

// Pull runs a Request and reports every step through emit. emit is called
// from one goroutine at a time and must not block for long.
func (c *Client) Pull(ctx context.Context, r Request, emit func(Event)) (*Outcome, error) {
	if emit == nil {
		emit = func(Event) {}
	}
	t0 := time.Now()
	e := ev(EvResolve)
	shown := r.Ref.Redacted() // events are logged and piped: no signed-URL credentials
	e.Ref = &shown
	emit(e)
	var plan *Plan
	var err error
	if r.Pin != nil {
		plan, err = c.resolvePinned(ctx, r)
	} else {
		plan, err = c.Resolve(ctx, r.Ref, r.Selection)
		if err == nil {
			plan.fresh = true
		}
		if err != nil && sourceDown(err) {
			c.ensureIndex(ctx)
			if _, m := c.indexEntry(r.Ref, ""); m != nil {
				plan, err = c.planFromIndex(r.Ref, m, r.Selection)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	revoked, err := c.revokedBlobs()
	if err != nil {
		return nil, err
	}
	// info: whether it fits this machine, before the plan is shown; never for
	// a plan the checks below refuse, which downloads nothing at all
	// colibri streams a model's experts from the disk and keeps only the
	// dense part in memory: the whole weights are not what it needs
	if r.DryRun && r.Target.Kind != "colibri" && c.plannedFilesAllowed(plan, revoked, r.Licenses) {
		plan.Memory = c.estimateMemory(ctx, plan, revoked)
	}
	e = ev(EvPlan)
	snapshot := *plan // the pull goes on updating plan; listeners get their own copy
	snapshot.Files = append([]File(nil), plan.Files...)
	snapshot.Ref = snapshot.Ref.Redacted()
	e.Plan = &snapshot
	emit(e)
	out := &Outcome{Plan: plan}
	// before the dry run returns: lmget info says it too
	for _, f := range plan.Selected() {
		if err := revokedErr(revoked, f.Path, f.SHA256); err != nil {
			return nil, err
		}
	}
	if err := c.licensePolicy(plan, r.Licenses); err != nil {
		return nil, err
	}
	if r.DryRun {
		out.Elapsed = time.Since(t0)
		return out, nil
	}
	if r.License != "" && plan.License != lockedLicense(r.License) && !r.AcceptLicenseChange {
		now := plan.License
		if now == "" {
			now = "no license"
		}
		return nil, newErr(ExitVerify, "license", "the license changed: "+quote(r.License)+" in the lockfile, "+quote(now)+" at the source").
			why("a new revision may come under terms that do not allow what the old one did").
			fix("", "read the new license, then run the same command with --accept-license-change to record it")
	}
	if len(plan.Files) == 0 { // not a selection matter: there is nothing there
		return nil, newErr(ExitNotFound, "not_found", r.Ref.Display()+" has no files at "+short(plan.Commit)).
			why("the repository exists but is empty at that revision").
			fix("", "check the name, or pick another revision with @<branch|tag|commit>")
	}
	if len(plan.Selected()) == 0 {
		return nil, newErr(ExitUsage, "usage", "no files selected").
			fix("lmget info "+r.Ref.Display(), "list the files, then adjust --include / --exclude")
	}
	// before a single byte: a target that cannot hold the model wastes none
	if err := TargetAccepts(plan, r.Target); err != nil {
		return nil, err
	}
	// two files one name on this filesystem: the second would overwrite or
	// shadow the first where the model is exported
	if root := targetRoot(r.Target); root != "" {
		if pairs := nameCollisions(plan); len(pairs) > 0 && caseFolds(root) {
			return nil, newErr(ExitUsage, "usage", "two files of "+r.Ref.Display()+" would be one file here: "+
				quote(pairs[0][0])+" and "+quote(pairs[0][1])).
				why("the filesystem at %s does not tell names apart by case (or Unicode form)", root).
				fix("--exclude '"+GlobEscape(pairs[0][1])+"'", "skip one of them").
				fix("--to none", "or keep the model in the store only")
		}
	}
	// keep a margin so a full disk never ends half way through
	if margin := int64(512 << 20); plan.DiskFree >= 0 && plan.ToFetch+margin > plan.DiskFree {
		return nil, newErr(ExitDisk, "disk", "not enough free space").
			why("need %s, %s free at %s", HumanBytes(plan.ToFetch+margin), HumanBytes(plan.DiskFree), c.store.Root()).
			fix("", "free space, or set LMGET_HOME to a larger disk")
	}
	unlock, err := c.store.flock(".lock", false, true) // shared: rm and verify wait for us
	if err != nil {
		return nil, diskErr(err)
	}
	defer unlock()
	// what the store holds was read before the lock: an rm may have run since
	plan.recount(c.store)
	// recorded before downloading: gc must not reclaim the files of a pull
	// that is interrupted half way
	if err := c.store.saveManifest(plan, nil, nil); err != nil {
		return nil, diskErr(err)
	}
	all := make(map[string]bool, len(plan.Files))
	for _, f := range plan.Files {
		all[f.Path] = true
	}
	for i := range plan.Files {
		f := &plan.Files[i]
		if !f.Selected() {
			continue
		}
		if f.Cached {
			if err := c.checkStored(f, isPickle(f.Path, all)); err != nil {
				return nil, err
			}
			e := ev(EvFileDone)
			e.File, e.SHA256, e.Cached, e.Total = f.Path, f.SHA256, true, f.Size
			emit(e)
			continue
		}
		n, err := c.getFile(ctx, plan.Ref, plan.Commit, f, isPickle(f.Path, all), revoked, emit)
		out.Fetched += n
		if err != nil {
			return nil, err
		}
	}
	// every selected file now matches the source (or the URL's own download):
	// record the bindings. An offline plan built from a lock adds none.
	var verified map[string]string
	if !plan.offline {
		verified = map[string]string{}
		for _, f := range plan.Selected() {
			verified[f.Path] = f.SHA256
		}
	}
	if err := c.store.saveManifest(plan, nil, verified); err != nil {
		return nil, diskErr(err)
	}
	if r.Target.Kind != "" && r.Target.Kind != "none" {
		p, err := c.export2(plan, r.Target)
		if err != nil {
			return nil, err
		}
		out.Path = p
		if r.Target.Kind == "ollama" {
			out.RunAs = c.OllamaName(plan.Ref)
		}
		if plan.Ref.Source == "url" && r.Target.Kind == "dir" && len(plan.Files) == 1 {
			out.Path = filepath.Join(p, plan.Files[0].Path) // a URL is one file: report the file
		}
		e := ev(EvExport)
		e.Target, e.Path = r.Target.Kind, p
		emit(e)
	}
	out.Elapsed = time.Since(t0)
	e = ev(EvDone)
	e.Fetched, e.Elapsed, e.Path, e.Total = out.Fetched, out.Elapsed.Seconds(), out.Path, plan.Bytes
	emit(e)
	return out, nil
}

func (c *Client) getFile(ctx context.Context, ref Ref, commit string, f *File, declaredPickle bool, revoked map[string]string, emit func(Event)) (int64, error) {
	key := f.SHA256
	switch {
	case key != "":
	case f.GitSHA1 != "":
		key = "git-" + f.GitSHA1
	default:
		key = f.tmpKey // direct URL without a known hash
	}
	part, state := c.store.tmpPaths(key)
	// one process per file: a second pull of the same file waits, then finds it stored
	unlock, err := c.store.flock("tmp/"+key+".lock", true, true)
	if err != nil {
		return 0, diskErr(err)
	}
	// the lock file stays: removing it would let a waiting process and a new
	// one lock two different inodes and download at the same time
	defer unlock()
	if f.SHA256 == "" && f.GitSHA1 != "" {
		if s, ok := c.store.LookupGit(f.GitSHA1); ok {
			f.SHA256 = s
		}
	}
	if err := revokedErr(revoked, f.Path, f.SHA256); err != nil { // a git id the store already mapped
		return 0, err
	}
	if f.SHA256 != "" && !f.mustFetch && c.store.Has(f.SHA256, f.Size) {
		if err := c.checkStored(f, declaredPickle); err != nil {
			return 0, err
		}
		f.Cached = true
		e := ev(EvFileDone)
		e.File, e.SHA256, e.Cached, e.Total = f.Path, f.SHA256, true, f.Size
		emit(e)
		return 0, nil
	}
	e := ev(EvFileStart)
	e.File, e.Total = f.Path, f.Size
	emit(e)

	var cnt counters
	stop := make(chan struct{})
	ticked := make(chan struct{})
	go func() { // progress at 10 Hz
		defer close(ticked)
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				e := ev(EvProgress)
				e.File, e.Bytes, e.Total = f.Path, cnt.done.Load(), f.Size
				emit(e)
			}
		}
	}()
	var sum string       // set when the bytes were verified on arrival
	var streamed *digest // the hash computed while the source or ModelScope downloaded (issue #1)
	if f.data != nil {
		err = writeFileAtomic(part, f.data)
		cnt.done.Store(int64(len(f.data)))
	} else {
		// first the user's origins; a plan from the index has no source URL
		err = noOrigin()
		if of, s, ok := c.fromOrigins(ctx, *f, part, state, &cnt); ok {
			f.From, sum, err = of.From, s, nil
		} else if f.url != "" {
			cnt.done.Store(0)
			dg := &digest{}
			if err = c.fetchHashed(ctx, *f, part, state, &cnt, dg); err == nil {
				streamed = dg
			}
		}
		if err != nil && c.canMirror(ref, err) {
			if mf, ok := c.mirrorFile(ctx, ref, *f); ok {
				cnt.done.Store(0) // fetch counts the chunks already on disk again
				dg := &digest{}
				if merr := c.fetchHashed(ctx, mf, part, state, &cnt, dg); merr == nil {
					f.From, err, streamed = mf.From, nil, dg
				}
			}
		}
		if err != nil && sourceDown(err) { // last: the nodes of the signed index
			c.ensureIndex(ctx)
			for _, nf := range c.nodeFiles(ref, commit, *f) {
				if c.nodeIsDown(nf.From) {
					continue
				}
				cnt.done.Store(0)
				if nerr := c.fetch(ctx, nf, part, state, &cnt); nerr != nil {
					if AsError(nerr).Code == ExitNetwork && ctx.Err() == nil {
						c.markNodeDown(nf.From) // unreachable or failing: not for every file again
					}
					continue // a 404 is this file only: the node may not have mirrored it yet
				}
				s, verr := c.verify(part, &nf)
				if verr != nil {
					os.Remove(part) // this node served other bytes: try the next one
					os.Remove(state)
					if AsError(verr).Code == ExitVerify { // not a local disk error
						c.markNodeDown(nf.From)
					}
					continue
				}
				f.SHA256, f.From, sum, err = nf.SHA256, nf.From, s, nil
				break
			}
		}
	}
	close(stop)
	<-ticked
	if err != nil {
		return cnt.fetched.Load(), err
	}
	if sum == "" && streamed != nil && streamed.complete {
		sum, err = streamed.check(part, f)
		c.streamedFiles.Add(1)
	} else if sum == "" {
		sum, err = c.verify(part, f)
	}
	if err == nil { // a file known by its git id only: its sha256 is known now
		err = revokedErr(revoked, f.Path, sum)
	}
	if err == nil {
		err = checkFormat(part, f.Path, declaredPickle)
	}
	if le, ok := err.(*Error); ok && f.From != "" && le.Kind == "verify" {
		le.fix("", "these bytes came from "+f.From+", which serves something other than what Hugging Face publishes")
	}
	if err != nil {
		os.Remove(part)
		os.Remove(state)
		return cnt.fetched.Load(), err
	}
	if err := c.store.commit(part, sum, f.GitSHA1); err != nil {
		return cnt.fetched.Load(), diskErr(err)
	}
	os.Remove(state)
	f.SHA256, f.Cached = sum, true
	e = ev(EvFileDone)
	e.File, e.SHA256, e.Total, e.Bytes, e.From = f.Path, sum, f.Size, f.Size, f.From
	emit(e)
	return cnt.fetched.Load(), nil
}

// checkStored runs the content check on a blob already in the store, under
// the name this plan gives it: bytes that entered the store as model.pkl must
// not be exported as model.safetensors (audit before publishing). The blob
// stays: another model may use it under a name that says what it is.
func (c *Client) checkStored(f *File, declaredPickle bool) error {
	err := checkFormat(c.store.BlobPath(f.SHA256), f.Path, declaredPickle)
	if e, ok := err.(*Error); ok && e.Kind == "format" {
		e.Fixes = []Fix{{Note: "these bytes are already in the store under another name; nothing was exported"}}
	}
	return err
}

// verify hashes the finished file once: sha256 always, and the git blob id
// when that is the only hash the source publishes.
func (c *Client) verify(part string, f *File) (string, error) {
	fh, err := os.Open(part)
	if err != nil {
		return "", diskErr(err)
	}
	defer fh.Close()
	s256 := sha256.New()
	var git hash.Hash
	w := io.Writer(s256)
	if f.GitSHA1 != "" {
		git = sha1.New()
		fmt.Fprintf(git, "blob %d\x00", f.Size)
		w = io.MultiWriter(s256, git)
	}
	n, err := io.Copy(w, fh)
	if err != nil {
		return "", diskErr(err)
	}
	sum := hex.EncodeToString(s256.Sum(nil))
	bad := func(want, got, algo string) error {
		return newErr(ExitVerify, "verify", "verification failed for "+f.Path).
			why("%s published %s, received bytes hash to %s (%s)", "the source", short(want), short(got), algo).
			fix("", "the partial file was discarded: run the same command again").
			fix("", "if it fails again, the source is serving different bytes than it publishes")
	}
	if n != f.Size {
		return "", bad(fmt.Sprint(f.Size), fmt.Sprint(n), "size")
	}
	if f.SHA256 != "" && sum != f.SHA256 {
		return "", bad(f.SHA256, sum, "sha256")
	}
	if git != nil {
		if g := hex.EncodeToString(git.Sum(nil)); g != f.GitSHA1 {
			return "", bad(f.GitSHA1, g, "git-sha1")
		}
	}
	return sum, nil
}

func short(h string) string {
	if len(h) > 16 {
		return h[:8] + ".." + h[len(h)-4:]
	}
	return h
}

// HumanBytes formats decimal units, as disks and downloads are sold.
func HumanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	unitName := "kMGTP"[exp : exp+1]
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f %sB", v, unitName)
	case v >= 10:
		return fmt.Sprintf("%.1f %sB", v, unitName)
	}
	return fmt.Sprintf("%.2f %sB", v, unitName)
}
