package lmget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// fetchState is persisted after every completed chunk: a killed process
// loses at most the chunks that were in flight.
type fetchState struct {
	Size  int64  `json:"size"`
	Chunk int64  `json:"chunk"`
	Done  []bool `json:"done"`
	// Origin wrote these chunks ("src:<host>", "modelscope", "node:<host>"):
	// a fetch from another origin starts again, so bytes an untrusted origin
	// left are never completed by another one and blamed on it
	Origin string `json:"origin,omitempty"`
}

type counters struct {
	done    atomic.Int64 // bytes of this file on disk (committed + in flight)
	fetched atomic.Int64 // bytes received from the network in this run
}

const stallTimeout = 60 * time.Second

// fetch downloads url into part with parallel ranged requests and resumes
// from state. It does not verify: the caller hashes the finished file.
func (c *Client) fetch(ctx context.Context, f File, part, statePath string, cnt *counters) error {
	return c.fetchHashed(ctx, f, part, statePath, cnt, nil)
}

// checkpointEvery and checkpointBytes bound how much of a download an
// interruption can cost: chunks are made durable and recorded at least
// that often. syncFile is the sync; tests count it.
var (
	checkpointEvery       = time.Second
	checkpointBytes int64 = 256 << 20
	syncFile              = func(f *os.File) error { return f.Sync() }
)

// fetchHashed is fetch that also hashes the file in chunk order while later
// chunks still arrive, when dg is not nil: issue #1. dg is complete only when
// every byte of the file went through the hash; the caller still compares it
// with the published hashes before the store commit, and falls back to
// reading the whole file again otherwise.
func (c *Client) fetchHashed(ctx context.Context, f File, part, statePath string, cnt *counters, dg *digest) error {
	// the origin is the second origin's name, or the source's host: a URL
	// server claiming a Hugging Face file's sha256 must not leave chunks a
	// later Hugging Face download completes
	origin := f.From
	if origin == "" {
		origin = "src:" + hostOf(f.url)
	}
	st := fetchState{Size: f.Size, Chunk: c.opt.ChunkSize, Origin: origin}
	if _, err := os.Stat(part); err != nil {
		os.Remove(statePath) // a state without its partial file describes nothing
	}
	reused := false
	if b, err := os.ReadFile(statePath); err == nil {
		var old fetchState
		if json.Unmarshal(b, &old) == nil && old.Size == f.Size && old.Chunk > 0 && old.Origin == origin {
			st, reused = old, true
		}
	}
	// a state this fetch does not resume (another origin's, or any state for a
	// stream that cannot resume) goes before the first byte is written: its
	// done chunks would otherwise describe bytes this origin overwrites, and a
	// later fetch from the state's origin would complete them (audit run 6)
	if !reused || f.noRanges {
		if err := os.Remove(statePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return diskErr(err)
		}
	}
	n := int((f.Size + st.Chunk - 1) / st.Chunk)
	if len(st.Done) != n {
		st.Done = make([]bool, n)
	}
	out, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return diskErr(err)
	}
	defer out.Close()
	if err := out.Truncate(f.Size); err != nil {
		return diskErr(err)
	}
	for i, d := range st.Done {
		if d {
			cnt.done.Add(min(st.Chunk, f.Size-int64(i)*st.Chunk))
		}
	}
	if f.Size == 0 {
		return nil
	}
	if f.noRanges { // no resume possible: one sequential stream from the start
		cnt.done.Store(0)
		if _, err := c.fetchRange(ctx, f, out, 0, f.Size, cnt, false); err != nil {
			return err
		}
		if err := syncFile(out); err != nil {
			return diskErr(err)
		}
		return nil
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var hashed chan int // completed chunk indexes, for the in-order hash
	var hashDone chan bool
	if dg != nil {
		hashed = make(chan int, n)
		hashDone = make(chan bool, 1)
		already := append([]bool(nil), st.Done...) // resumed chunks, hashed first
		go func() { hashDone <- dg.stream(out, f, st.Chunk, already, hashed) }()
	}
	var mu sync.Mutex
	var pending []int // chunks written but not yet durable: the state does not list them
	var pendingBytes int64
	last := time.Now()
	// checkpoint (mu held) makes the written chunks durable, then records
	// them: the state never lists a chunk the disk might not hold. Once a
	// second or every checkpointBytes, not per chunk: on macOS a sync flushes
	// the drive's cache, and one per 16 MiB chunk cost 40% of the speed.
	checkpoint := func() error {
		if len(pending) == 0 {
			return nil
		}
		if err := syncFile(out); err != nil {
			return diskErr(err)
		}
		for _, i := range pending {
			st.Done[i] = true
		}
		b, _ := json.Marshal(st)
		if err := writeFileAtomic(statePath, b); err != nil {
			return diskErr(err)
		}
		pending, pendingBytes, last = pending[:0], 0, time.Now()
		return nil
	}
	todo := make(chan int)
	var wg sync.WaitGroup
	conns := c.opt.Conns
	if int64(conns) > int64(n) {
		conns = n
	}
	for range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range todo {
				off := int64(i) * st.Chunk
				size := min(st.Chunk, f.Size-off)
				if err := c.fetchChunk(ctx, f, out, off, size, cnt); err != nil {
					cancel(err)
					return
				}
				mu.Lock()
				pending, pendingBytes = append(pending, i), pendingBytes+size
				var err error
				if pendingBytes >= checkpointBytes || time.Since(last) >= checkpointEvery {
					err = checkpoint()
				}
				mu.Unlock()
				if err != nil {
					cancel(err)
					return
				}
				if hashed != nil {
					hashed <- i // buffered for every chunk: never blocks
				}
			}
		}()
	}
feed:
	for i, d := range st.Done {
		if d {
			continue
		}
		select {
		case todo <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(todo)
	wg.Wait()
	// the last chunks, in every case: a finished file is durable before the
	// store takes it, and an interrupted one keeps all its progress
	mu.Lock()
	if err := checkpoint(); err != nil {
		cancel(err)
	}
	mu.Unlock()
	if hashed != nil {
		close(hashed)
		<-hashDone // the hash holds the file open for reading: wait for it in every case
	}
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if ctx.Err() != nil {
		return context.Canceled
	}
	return nil
}

// fetchChunk retries one range with backoff. Auth and not-found answers are
// final; network errors and 5xx are retried.
func (c *Client) fetchChunk(ctx context.Context, f File, out *os.File, off, size int64, cnt *counters) error {
	var last error
	attempts := f.attempts
	if attempts <= 0 {
		attempts = 6
	}
	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(1<<(attempt-1)) * c.retryBase):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		got, err := c.fetchRange(ctx, f, out, off, size, cnt, true)
		if err == nil {
			return nil
		}
		cnt.done.Add(-got) // these bytes will be written again
		var e *Error
		if errors.As(err, &e) && (e.Code != ExitNetwork || e.Kind == "changed") {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		last = err
	}
	return newErr(ExitNetwork, "network", fmt.Sprintf("download failed after %d attempts", attempts)).wrap(last).
		fix("", "run the same command again: verified chunks are kept")
}

func (c *Client) fetchRange(ctx context.Context, f File, out *os.File, off, size int64, cnt *counters, ranged bool) (int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
		if err != nil {
			return nil, err
		}
		switch {
		case f.ociRepo != "" || strings.HasPrefix(f.From, UserOrigin) || strings.HasPrefix(f.From, NodeOrigin):
			// registry blobs, user origins and index nodes: never the HF token,
			// even when their URL happens to sit under the HF endpoint
			req.Header.Set("User-Agent", c.opt.UserAgent)
		default:
			c.authorize(req)
		}
		if ranged {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+size-1))
			if f.ifRange != "" {
				req.Header.Set("If-Range", f.ifRange)
			}
		}
		return req, nil
	}
	var resp *http.Response
	var err error
	if f.ociRepo != "" { // a registry blob: with a pull token, renewed when it expires
		resp, err = c.ociDo(ctx, f.ociRepo, build)
	} else {
		var req *http.Request
		if req, err = build(); err != nil {
			return 0, err
		}
		resp, err = c.http.Do(req)
	}
	if err != nil {
		if le, ok := err.(*Error); ok {
			return 0, le
		}
		return 0, newErr(ExitNetwork, "network", "request failed").wrap(err)
	}
	defer resp.Body.Close()
	if ranged && resp.StatusCode != http.StatusPartialContent {
		if resp.StatusCode == http.StatusOK && off == 0 && resp.ContentLength == size {
			// server ignored Range but the body is exactly the chunk
		} else if resp.StatusCode == http.StatusOK && f.ifRange != "" {
			return 0, newErr(ExitNetwork, "changed", "the file changed on the server during the download").
				why("its validator no longer matches %s", f.ifRange).
				fix("", "run the same command again: it starts over with the new version")
		} else if resp.StatusCode == http.StatusOK || resp.StatusCode >= 500 || resp.StatusCode == 429 {
			return 0, newErr(ExitNetwork, "network", "unexpected "+printable(resp.Status)+" for a ranged request")
		} else {
			return 0, fileStatus(resp)
		}
	} else if !ranged && resp.StatusCode != http.StatusOK {
		return 0, fileStatus(resp)
	}
	// stall watchdog: no bytes for stallTimeout cancels this attempt
	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()
	w := io.NewOffsetWriter(out, off)
	buf := make([]byte, 256<<10)
	var got int64
	for got < size {
		k, rerr := resp.Body.Read(buf)
		if k > 0 {
			k = int(min(int64(k), size-got))
			if _, err := w.Write(buf[:k]); err != nil {
				return got, diskErr(err)
			}
			got += int64(k)
			cnt.done.Add(int64(k))
			cnt.fetched.Add(int64(k))
			stall.Reset(stallTimeout)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return got, newErr(ExitNetwork, "network", "connection dropped").wrap(rerr)
		}
	}
	if got != size {
		return got, newErr(ExitNetwork, "network", fmt.Sprintf("short read: %d of %d bytes", got, size))
	}
	return got, nil
}

func fileStatus(resp *http.Response) error {
	switch resp.StatusCode {
	case 401, 403:
		return newErr(ExitAuth, "auth", "the source refused the download ("+printable(resp.Status)+")").
			fix("export HF_TOKEN=...", "gated or private files need a token with access")
	case 404, 410:
		return newErr(ExitNotFound, "not_found", "the file is no longer at the source ("+printable(resp.Status)+")")
	}
	return newErr(ExitNetwork, "network", "unexpected "+printable(resp.Status))
}

func diskErr(err error) error {
	return newErr(ExitDisk, "disk", "cannot write to disk").wrap(err)
}
