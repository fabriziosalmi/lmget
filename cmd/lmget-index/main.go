// lmget-index builds the lmget index from a lockfile. Sign the output with the
// official minisign tool: minisign -S -s <key> -m index.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fabriziosalmi/lmget"
)

type nodes []string

func (n *nodes) String() string     { return fmt.Sprint(*n) }
func (n *nodes) Set(s string) error { *n = append(*n, s); return nil }

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 || args[0] != "build" {
		fmt.Fprint(os.Stderr, "usage: lmget-index build -lock lmget.lock -sequence N [-valid 720h] -node https://... [-node ...] [-o index.json]\n")
		return 2
	}
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	lockPath := fs.String("lock", "lmget.lock", "lockfile listing the models to index")
	seq := fs.Int64("sequence", 0, "sequence number, higher than the published index")
	valid := fs.Duration("valid", 30*24*time.Hour, "how long the index stays valid")
	out := fs.String("o", "", "output file (default stdout)")
	var ns nodes
	fs.Var(&ns, "node", "https URL of a node (repeatable)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	l, err := lmget.ReadLock(*lockPath)
	if err != nil {
		return fail(err)
	}
	c, err := lmget.New(lmget.Options{NoMirror: true})
	if err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ix, err := c.BuildIndex(ctx, l, lmget.BuildOptions{Sequence: *seq, Valid: *valid, Nodes: ns})
	if err != nil {
		return fail(err)
	}
	b, _ := json.MarshalIndent(ix, "", "  ")
	b = append(b, '\n')
	if *out == "" {
		os.Stdout.Write(b)
	} else if err := os.WriteFile(*out, b, 0o644); err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stderr, "lmget-index: %d models, sequence %d, valid until %s\nsign it: minisign -S -s <key> -m %s\n",
		len(ix.Models), ix.Sequence, ix.Expires.Format(time.DateOnly), map[bool]string{true: "<file>", false: *out}[*out == ""])
	return 0
}

func fail(err error) int {
	e := lmget.AsError(err)
	fmt.Fprintf(os.Stderr, "lmget-index: %s\n", lmget.Printable(e.Error()))
	if e.Why != "" {
		fmt.Fprintf(os.Stderr, "lmget-index:   %s\n", lmget.Printable(e.Why))
	}
	return int(e.Code)
}
