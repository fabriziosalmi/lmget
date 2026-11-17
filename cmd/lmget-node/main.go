// lmget-node mirrors the models of the signed lmget index and serves them by
// sha256. Run "lmget-node mirror" periodically and "lmget-node serve" behind a
// TLS reverse proxy (the index lists https nodes only).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fabriziosalmi/lmget"
	"github.com/fabriziosalmi/lmget/internal/p2p"
)

func main() { os.Exit(run(os.Args[1:])) }

func usage() int {
	fmt.Fprint(os.Stderr, `usage:
  lmget-node mirror               fetch and verify the index, then mirror its models
  lmget-node serve [-listen ADDR] serve the index and its blobs (default 127.0.0.1:8080)
  lmget-node seed [-port N] [-no-dht]
                                  seed the mirrored blobs as BitTorrent v2 torrents

The store is $LMGET_HOME; the index comes from $LMGET_INDEX_URL (default
https://lmget.org/index.json), verified with the index keys in force (the
built-in key, or those of the accepted key list) and $LMGET_INDEX_KEY.
`)
	return 2
}

func run(args []string) int {
	// mirror takes no arguments: one more (a typo, -h) prints the usage
	// instead of starting a download into the store
	if len(args) == 0 || (args[0] == "mirror" && len(args) > 1) {
		return usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c, err := lmget.New(lmget.Options{NoMirror: true})
	if err != nil {
		return fail(err)
	}
	switch args[0] {
	case "mirror":
		ix, err := c.UpdateIndex(ctx)
		if err != nil {
			return fail(err)
		}
		warnExpiry(ix, time.Now()) // every run (6 h) in the last week
		res, err := c.MirrorIndex(ctx, nil)
		if err != nil {
			return fail(err)
		}
		code := 0
		enc := json.NewEncoder(os.Stdout)
		for _, r := range res {
			enc.Encode(r)
			if r.Err != "" {
				code = 1
			}
		}
		return code
	case "seed":
		fs := flag.NewFlagSet("seed", flag.ContinueOnError)
		port := fs.Int("port", 0, "BitTorrent listen port (0: any free port)")
		noDHT := fs.Bool("no-dht", false, "do not announce on the Mainline DHT")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		ix, err := c.Index()
		if err != nil {
			return fail(err)
		}
		dir, err := os.MkdirTemp("", "lmget-node-seed")
		if err != nil {
			return fail(err)
		}
		defer os.RemoveAll(dir)
		s, err := p2p.New(p2p.Config{ListenPort: *port, NoDHT: *noDHT, DataDir: dir})
		if err != nil {
			return fail(err)
		}
		defer s.Close()
		// seed what the current verified index names, and re-read it every
		// minute: blobs a newer index withdraws, or all of them once the index
		// expires, stop being seeded
		last := ""
		for {
			n, errs := p2p.SeedIndex(c.Store(), ix, s)
			msg := fmt.Sprintf("seeding %d blobs on port %d", n, s.Client().LocalPort())
			if ix == nil {
				msg = "no valid index (" + lmget.Printable(lmget.AsError(err).Msg) + "): seeding nothing"
			} else if w := expiryWarning(ix.Expires, time.Now()); w != "" {
				msg += "; " + w // logged when it changes: daily, hourly in the last two days
			}
			if msg != last {
				for _, e := range errs {
					fmt.Fprintf(os.Stderr, "lmget-node: not seeded: %s\n", lmget.Printable(e.Error()))
				}
				fmt.Fprintf(os.Stderr, "lmget-node: %s\n", msg)
				last = msg
			}
			select {
			case <-ctx.Done():
				return 0
			case <-time.After(time.Minute):
			}
			ix, err = c.Index()
		}
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		listen := fs.String("listen", "127.0.0.1:8080", "address to listen on (put a TLS reverse proxy in front)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		ix, err := c.Index()
		if err != nil {
			return fail(err)
		}
		warnExpiry(ix, time.Now())
		go func() { // a long-running serve warns again once a day
			t := time.NewTicker(24 * time.Hour)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if ix, err := c.Index(); err == nil {
						warnExpiry(ix, time.Now())
					}
				}
			}
		}()
		srv := &http.Server{Addr: *listen, Handler: c.NodeHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
		go func() { <-ctx.Done(); srv.Shutdown(context.Background()) }()
		fmt.Fprintf(os.Stderr, "lmget-node: serving on %s\n", *listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fail(err)
		}
		return 0
	}
	return usage()
}

// expiryWarn is how long before the index expires lmget-node starts saying
// so: an expired index stops every node at once.
const expiryWarn = 7 * 24 * time.Hour

// expiryWarning is the warning for an index expiring at expires, or "".
func expiryWarning(expires, now time.Time) string {
	left := expires.Sub(now)
	if left >= expiryWarn {
		return ""
	}
	when := fmt.Sprintf("in %d days", int(math.Round(left.Hours()/24)))
	switch {
	case left <= 0:
		return "warning: the index expired on " + expires.UTC().Format(time.DateOnly) + ": nothing is served until the maintainer publishes a new one"
	case left < 48*time.Hour:
		when = fmt.Sprintf("in %d hours", int(left.Hours()))
	}
	return "warning: the index expires on " + expires.UTC().Format("2006-01-02 15:04 UTC") + " (" + when + "): the maintainer must sign and publish a new one"
}

func warnExpiry(ix *lmget.Index, now time.Time) {
	if w := expiryWarning(ix.Expires, now); w != "" {
		fmt.Fprintf(os.Stderr, "lmget-node: %s\n", w)
	}
}

func fail(err error) int {
	e := lmget.AsError(err)
	fmt.Fprintf(os.Stderr, "lmget-node: %s\n", lmget.Printable(e.Error()))
	return int(e.Code)
}
