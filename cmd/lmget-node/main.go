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

The store is $LMGET_HOME; the index comes from $LMGET_INDEX_URL, verified with the
built-in key or $LMGET_INDEX_KEY.
`)
	return 2
}

func run(args []string) int {
	if len(args) == 0 {
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
		if _, err := c.UpdateIndex(ctx); err != nil {
			return fail(err)
		}
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
		n, errs := p2p.SeedIndex(c.Store(), ix, s)
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "lmget-node: not seeded: %s\n", lmget.Printable(e.Error()))
		}
		fmt.Fprintf(os.Stderr, "lmget-node: seeding %d blobs on port %d\n", n, s.Client().LocalPort())
		<-ctx.Done()
		return 0
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		listen := fs.String("listen", "127.0.0.1:8080", "address to listen on (put a TLS reverse proxy in front)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if _, err := c.Index(); err != nil {
			return fail(err)
		}
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

func fail(err error) int {
	e := lmget.AsError(err)
	fmt.Fprintf(os.Stderr, "lmget-node: %s\n", lmget.Printable(e.Error()))
	return int(e.Code)
}
