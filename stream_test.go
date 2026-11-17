package lmget

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// streamPull pulls org/m through h with 256 KiB chunks and 4 connections and
// returns the client.
func streamPull(t *testing.T, h http.Handler, home string, ctx context.Context) (*Client, error) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, home)
	c.retryBase = time.Millisecond
	_, err := c.Pull(ctx, Request{Ref: Ref{"hf", "org/m", "main", "", ""}, Target: Target{Kind: "none"}}, nil)
	return c, err
}

func checkStreamed(t *testing.T, name string, c *Client, files []fakeFile) {
	t.Helper()
	if c.streamedFiles.Load() == 0 {
		t.Errorf("%s: no file was verified from the streamed hash", name)
	}
	for _, ff := range files {
		if !c.store.Has(sha(ff.data), int64(len(ff.data))) {
			t.Errorf("%s: %s is not in the store with its sha256", name, ff.path)
		}
	}
}

// Issue #1: the hash computed while downloading equals the file's, whatever
// order the chunks complete in, after a resume and after a failed chunk.
func TestTheStreamedHashIsTheFilesHash(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", t.TempDir())

	// out of order: the first chunk of every file arrives last
	f := &fakeHF{files: modelFiles()}
	inner := f.handler()
	slowFirst := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=0-") {
			time.Sleep(300 * time.Millisecond)
		}
		inner.ServeHTTP(w, r)
	})
	c, err := streamPull(t, slowFirst, t.TempDir(), context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkStreamed(t, "out of order", c, f.files)

	// a chunk that fails once is fetched again and hashed in its place
	f2 := &fakeHF{files: modelFiles()}
	inner2 := f2.handler()
	var failed atomic.Bool
	failOnce := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=786432-") && failed.CompareAndSwap(false, true) {
			http.Error(w, "flaky", http.StatusBadGateway)
			return
		}
		inner2.ServeHTTP(w, r)
	})
	c2, err := streamPull(t, failOnce, t.TempDir(), context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !failed.Load() {
		t.Fatal("the failing chunk was never asked")
	}
	checkStreamed(t, "failed chunk", c2, f2.files)

	// resumed: the chunks done before the interruption are hashed from disk
	f3 := &fakeHF{files: modelFiles()}
	home := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	stopAt := func(total int64) {
		if total > 1500<<10 {
			cancel()
		}
	}
	f3.onByte.Store(&stopAt)
	if _, err := streamPull(t, f3.handler(), home, ctx); AsError(err).Code != ExitInterrupted {
		t.Fatalf("the first pull was not interrupted: %v", err)
	}
	f3.onByte.Store(nil)
	c3, err := streamPull(t, f3.handler(), home, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkStreamed(t, "resumed", c3, f3.files)
}

// Issue #1: a chunk whose bytes differ from the published file is still
// refused before the store commit.
func TestACorruptChunkIsRefusedWithTheStreamedHash(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", t.TempDir())
	files := modelFiles()
	f := &fakeHF{files: files}
	inner := f.handler()
	corrupt := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=262144-") {
			rec := httptest.NewRecorder()
			inner.ServeHTTP(rec, r)
			body := rec.Body.Bytes()
			if len(body) > 10 {
				body[10] ^= 0xff
			}
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			w.Write(body)
			return
		}
		inner.ServeHTTP(w, r)
	})
	c, err := streamPull(t, corrupt, t.TempDir(), context.Background())
	if e := AsError(err); err == nil || e.Code != ExitVerify || e.Kind != "verify" {
		t.Fatalf("a corrupt chunk: %v", err)
	}
	if c.store.Has(sha(files[0].data), int64(len(files[0].data))) {
		t.Fatal("the weights entered the store")
	}
	if c.streamedFiles.Load() == 0 {
		t.Fatal("the refusal did not come from the streamed hash")
	}
}
