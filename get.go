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
	"time"
)

// Request is one pull: resolve, fetch, verify, export.
type Request struct {
	Ref       Ref
	Selection Selection
	Target    Target
	DryRun    bool       // stop after the plan
	Pin       *LockModel // reproduce this locked entry: its commit, files and hashes
}

// Outcome is the result of a successful pull.
type Outcome struct {
	Plan    *Plan
	Path    string // export path ("" for dry runs or Target none)
	Fetched int64  // bytes received from the network in this run
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
	e.Ref = &r.Ref
	emit(e)
	var plan *Plan
	var err error
	if r.Pin != nil {
		plan, err = c.resolvePinned(ctx, r)
	} else {
		plan, err = c.Resolve(ctx, r.Ref, r.Selection)
	}
	if err != nil {
		return nil, err
	}
	e = ev(EvPlan)
	e.Plan = plan
	emit(e)
	out := &Outcome{Plan: plan}
	if r.DryRun {
		out.Elapsed = time.Since(t0)
		return out, nil
	}
	if len(plan.Selected()) == 0 {
		return nil, newErr(ExitUsage, "usage", "no files selected").
			fix("lmget info "+r.Ref.Display(), "list the files, then adjust --include / --exclude")
	}
	// keep a margin so a full disk never ends half way through
	if margin := int64(512 << 20); plan.DiskFree >= 0 && plan.ToFetch+margin > plan.DiskFree {
		return nil, newErr(ExitDisk, "disk", "not enough free space").
			why("need %s, %s free at %s", HumanBytes(plan.ToFetch+margin), HumanBytes(plan.DiskFree), c.store.Root()).
			fix("", "free space, or set LMGET_HOME to a larger disk")
	}
	for i := range plan.Files {
		f := &plan.Files[i]
		if !f.Selected() {
			continue
		}
		if f.Cached {
			e := ev(EvFileDone)
			e.File, e.SHA256, e.Cached, e.Total = f.Path, f.SHA256, true, f.Size
			emit(e)
			continue
		}
		n, err := c.getFile(ctx, f, emit)
		out.Fetched += n
		if err != nil {
			return nil, err
		}
	}
	if err := c.store.saveManifest(plan, nil); err != nil {
		return nil, diskErr(err)
	}
	if r.Target.Kind != "" && r.Target.Kind != "none" {
		p, err := c.Export(plan, r.Target)
		if err != nil {
			return nil, err
		}
		out.Path = p
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

func (c *Client) getFile(ctx context.Context, f *File, emit func(Event)) (int64, error) {
	key := f.SHA256
	if key == "" {
		key = "git-" + f.GitSHA1
	}
	part, state := c.store.tmpPaths(key)
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
	err := c.fetch(ctx, *f, part, state, &cnt)
	close(stop)
	<-ticked
	if err != nil {
		return cnt.fetched.Load(), err
	}
	sum, err := c.verify(part, f)
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
	e.File, e.SHA256, e.Total, e.Bytes = f.Path, sum, f.Size, f.Size
	emit(e)
	return cnt.fetched.Load(), nil
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
