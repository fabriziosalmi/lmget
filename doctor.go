package lmget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Check is one line of lmget doctor.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn, fail
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// DoctorOptions selects the slow checks.
type DoctorOptions struct {
	Verify   bool   // re-hash every blob in the store
	LockPath string // lockfile to check, if it exists
}

// Doctor inspects the store, the export targets, the network and the token.
// It changes nothing, except that --verify moves corrupt blobs to
// <store>/quarantine so the next get or sync fetches them again.
func (c *Client) Doctor(ctx context.Context, o DoctorOptions) []Check {
	var cs []Check
	add := func(name, status, detail, fix string) {
		cs = append(cs, Check{Name: name, Status: status, Detail: detail, Fix: fix})
	}
	add("lmget", "ok", fmt.Sprintf("%s %s/%s", Version, runtime.GOOS, runtime.GOARCH), "")

	// store
	st := c.Stats()
	probe := filepath.Join(c.store.Root(), "tmp", fmt.Sprintf(".doctor-%d", os.Getpid()))
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		add("store", "fail", st.Root+" is not writable", "check permissions, or set LMGET_HOME")
	} else {
		os.Remove(probe)
		add("store", "ok", fmt.Sprintf("%s  %s in %d blobs, %s free", st.Root, HumanBytes(st.Bytes), st.Blobs, HumanBytes(st.DiskFree)), "")
	}
	if st.Partial > 0 {
		add("partial", "warn", fmt.Sprintf("%d interrupted downloads (%s on disk)", st.Partial, HumanBytes(st.PartialBytes)),
			"run the same lmget command again to resume")
	}
	if _, bad, err := c.store.Manifests(); err == nil && len(bad) > 0 {
		add("manifests", "fail", fmt.Sprintf("%d manifests cannot be read, e.g. %s", len(bad), bad[0]),
			"lmget rm refuses to delete blobs until they are fixed or removed by hand")
	}
	if list, err := c.List(); err == nil {
		missing := 0
		for _, in := range list {
			if in.Missing > 0 {
				missing++
			}
		}
		if missing > 0 {
			add("models", "warn", fmt.Sprintf("%d of %d models have files missing from the store", missing, len(list)), "lmget ls shows which; lmget get <model> restores them")
		} else {
			add("models", "ok", fmt.Sprintf("%d models, all files present", len(list)), "")
		}
	}
	// blobs the signed index revoked
	revoked, rerr := c.revokedBlobs()
	if rerr != nil {
		add("revoked", "fail", AsError(rerr).Msg, "lmget index")
	}
	if len(revoked) > 0 {
		var hits []string
		models := map[string]bool{}
		for sha := range revoked {
			if _, err := os.Stat(c.store.BlobPath(sha)); err == nil {
				hits = append(hits, short(sha))
			}
		}
		if ms, _, err := c.store.Manifests(); err == nil {
			for _, m := range ms {
				for _, f := range m.Plan.Selected() {
					if _, ok := revoked[f.SHA256]; ok {
						models[m.Plan.Ref.Display()] = true
					}
				}
			}
		}
		if len(hits) > 0 || len(models) > 0 {
			var names []string
			for n := range models {
				names = append(names, n)
			}
			sort.Strings(names)
			add("revoked", "fail", fmt.Sprintf("%d blobs in the store are revoked by the index; models using them: %s", len(hits), strings.Join(names, ", ")),
				"lmget rm <model> for each of them")
		} else {
			add("revoked", "ok", fmt.Sprintf("none of the %d blobs the index revokes is in the store", len(revoked)), "")
		}
	}
	if o.Verify {
		unlock, lerr := c.store.flock(".lock", true, false)
		if lerr != nil {
			add("verify", "fail", AsError(lerr).Msg, "run doctor --verify when no download is running")
			return cs
		}
		defer unlock()
		bad, n, stuck, err := c.verifyBlobs(ctx)
		switch {
		case err != nil:
			add("verify", "fail", err.Error(), "")
		case len(stuck) > 0:
			add("verify", "fail", fmt.Sprintf("%d corrupt blobs quarantined, but %d links to them could not be removed, e.g. %s", len(bad), len(stuck), stuck[0]), "remove those links by hand: they serve corrupt bytes")
		case len(bad) > 0:
			add("verify", "fail", fmt.Sprintf("%d of %d blobs were corrupt and moved to quarantine", len(bad), n), "lmget sync, or lmget get <model>, downloads them again")
		default:
			add("verify", "ok", fmt.Sprintf("%d blobs re-hashed, all match", n), "")
		}
	}

	// export target
	hf := HFCacheDir()
	if err := os.MkdirAll(hf, 0o755); err != nil {
		add("hf cache", "fail", hf+" cannot be created", "set HF_HUB_CACHE to a writable directory, or use --to dir:PATH")
	} else if sameDevice(hf, c.store.Root()) {
		add("hf cache", "ok", hf+"  same disk as the store: exports are hard links", "")
	} else {
		add("hf cache", "warn", hf+"  on another disk: exports are symlinks into the store", "keep the store and the cache on one disk, or set LMGET_HOME next to the cache")
	}

	// network and token
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.opt.HFEndpoint+"/api/whoami-v2", nil)
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		add("network", "fail", c.opt.HFEndpoint+" unreachable: "+err.Error(), "check the connection, a proxy (HTTPS_PROXY) or HF_ENDPOINT")
	} else {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		add("network", "ok", fmt.Sprintf("%s answered in %d ms", c.opt.HFEndpoint, time.Since(start).Milliseconds()), "")
		switch {
		case strings.HasPrefix(c.tokenSource, "unreadable:"):
			add("token", "fail", "token file "+strings.TrimPrefix(c.tokenSource, "unreadable:")+" exists but cannot be read", "fix its permissions, or export HF_TOKEN")
		case c.opt.HFToken == "":
			add("token", "ok", "none: public models only", "gated or private models need HF_TOKEN")
		case resp.StatusCode == 200:
			add("token", "ok", "from "+c.tokenSource+", accepted", "")
		case resp.StatusCode == 401:
			add("token", "fail", "from "+c.tokenSource+", rejected by "+c.opt.HFEndpoint, "create a new read token at https://huggingface.co/settings/tokens")
		default:
			add("token", "warn", fmt.Sprintf("from %s, could not be checked (%s)", c.tokenSource, resp.Status), "")
		}
	}

	// lockfile
	if o.LockPath != "" {
		l, err := ReadLock(o.LockPath)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			add("lock", "fail", AsError(err).Msg, "")
		default:
			stored := 0
			for _, m := range l.Models {
				ok := true
				for _, f := range m.Files {
					if !c.store.Has(f.SHA256, f.Size) {
						ok = false
						break
					}
				}
				if ok {
					stored++
				}
			}
			if stored == len(l.Models) {
				add("lock", "ok", fmt.Sprintf("%s: %d models, all in the store", o.LockPath, len(l.Models)), "")
			} else {
				add("lock", "warn", fmt.Sprintf("%s: %d of %d models in the store", o.LockPath, stored, len(l.Models)), "lmget sync")
			}
		}
	}
	return cs
}

// verifyBlobs re-hashes every blob; a blob whose content no longer matches
// its name is moved to <store>/quarantine.
func (c *Client) verifyBlobs(ctx context.Context) (bad []string, n int, stuck []string, err error) {
	dir := filepath.Join(c.store.Root(), "blobs", "sha256")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, nil, err
	}
	// export links recorded by lmget: a corrupt blob's hard links share its
	// bytes, so they go too and the next export recreates them
	var links []string
	if ms, _, err := c.store.Manifests(); err == nil {
		for _, m := range ms {
			for _, ex := range m.Exports {
				links = append(links, ex.Links...)
			}
		}
	}
	buf := make([]byte, 1<<20)
	for _, e := range entries {
		if ctx.Err() != nil {
			return bad, n, stuck, ctx.Err()
		}
		name := e.Name()
		if !hex64.MatchString(name) {
			continue
		}
		n++
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return bad, n, stuck, err
		}
		h := sha256.New()
		_, err = io.CopyBuffer(h, f, buf)
		f.Close()
		if err != nil {
			return bad, n, stuck, err
		}
		if hex.EncodeToString(h.Sum(nil)) != name {
			if fi, err := os.Stat(filepath.Join(dir, name)); err == nil {
				if bad, ok := inodeOf(filepath.Join(dir, name), fi); ok {
					for _, l := range links {
						if li, err := os.Lstat(l); err == nil && li.Mode().IsRegular() {
							if in, ok := inodeOf(l, li); ok && in == bad {
								if err := os.Remove(l); err != nil {
									stuck = append(stuck, l)
								}
							}
						}
					}
				}
			}
			q := filepath.Join(c.store.Root(), "quarantine")
			os.MkdirAll(q, 0o755)
			if err := os.Rename(filepath.Join(dir, name), filepath.Join(q, name)); err != nil {
				return bad, n, stuck, err
			}
			bad = append(bad, name)
		}
	}
	return bad, n, stuck, nil
}
