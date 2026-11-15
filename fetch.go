package lmget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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
}

type counters struct {
	done    atomic.Int64 // bytes of this file on disk (committed + in flight)
	fetched atomic.Int64 // bytes received from the network in this run
}

const stallTimeout = 60 * time.Second

// fetch downloads url into part with parallel ranged requests and resumes
// from state. It does not verify: the caller hashes the finished file.
func (c *Client) fetch(ctx context.Context, f File, part, statePath string, cnt *counters) error {
	st := fetchState{Size: f.Size, Chunk: c.opt.ChunkSize}
	if _, err := os.Stat(part); err != nil {
		os.Remove(statePath) // a state without its partial file describes nothing
	}
	if b, err := os.ReadFile(statePath); err == nil {
		var old fetchState
		if json.Unmarshal(b, &old) == nil && old.Size == f.Size && old.Chunk > 0 {
			st = old
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
		if err := out.Sync(); err != nil {
			return diskErr(err)
		}
		return nil
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var mu sync.Mutex
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
				if err := out.Sync(); err != nil { // data durable before the state says so
					cancel(diskErr(err))
					return
				}
				mu.Lock()
				st.Done[i] = true
				b, _ := json.Marshal(st)
				err := writeFileAtomic(statePath, b)
				mu.Unlock()
				if err != nil {
					cancel(diskErr(err))
					return
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
	for attempt := range 6 {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(1<<(attempt-1)) * 500 * time.Millisecond):
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
	return newErr(ExitNetwork, "network", "download failed after 6 attempts").wrap(last).
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
		c.authorize(req)
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
