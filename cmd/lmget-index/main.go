// lmget-index builds the lmget index from a lockfile, and the key list that
// names the keys allowed to sign it. Sign both with the official minisign
// tool: the index with an index key, the key list with the root key.
//
//	lmget-index build -lock lmget.lock -sequence N -node https://... -o index.json
//	lmget-index keys -sequence N -index-key lmget-index.pub [-revoke NUMBER] -o keys.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fabriziosalmi/lmget"
)

type nodes []string

func (n *nodes) String() string     { return fmt.Sprint(*n) }
func (n *nodes) Set(s string) error { *n = append(*n, s); return nil }

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) > 0 && args[0] == "keys" {
		return runKeys(args[1:])
	}
	if len(args) > 0 && args[0] == "aliases" {
		return runAliases(args[1:])
	}
	if len(args) == 0 || args[0] != "build" {
		fmt.Fprint(os.Stderr, "usage: lmget-index build -lock lmget.lock -sequence N [-valid 720h] -node https://... [-node ...] [-o index.json]\n"+
			"       lmget-index keys -sequence N [-valid 8760h] -index-key FILE [-index-key FILE ...] [-revoke NUMBER ...] [-o keys.json]\n"+
			"       lmget-index aliases -in aliases.toml -sequence N [-valid 720h] [-o aliases.json]\n")
		return 2
	}
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	lockPath := fs.String("lock", "lmget.lock", "lockfile listing the models to index")
	seq := fs.Int64("sequence", 0, "sequence number, higher than the published index")
	valid := fs.Duration("valid", 30*24*time.Hour, "how long the index stays valid (at most 60 days: clients refuse longer)")
	out := fs.String("o", "", "output file (default stdout)")
	var ns nodes
	fs.Var(&ns, "node", "https URL of a node (repeatable)")
	revokedPath := fs.String("revoked", "", "file of revoked blobs, one \"<sha256> <reason>\" per line (# comments)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	var revoked []lmget.IndexRevoked
	if *revokedPath != "" {
		b, err := os.ReadFile(*revokedPath)
		if err != nil {
			return fail(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			sha, reason, _ := strings.Cut(line, " ")
			if reason = strings.TrimSpace(reason); reason == "" {
				return fail(fmt.Errorf("%s:%d: a revoked blob needs a reason", *revokedPath, i+1))
			}
			revoked = append(revoked, lmget.IndexRevoked{SHA256: strings.ToLower(sha), Reason: reason})
		}
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
	ix, err := c.BuildIndex(ctx, l, lmget.BuildOptions{Sequence: *seq, Valid: *valid, Nodes: ns, Revoked: revoked})
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
	fmt.Fprintf(os.Stderr, "lmget-index: %d models, %d revoked blobs, sequence %d, valid until %s\nsign it: minisign -S -s <key> -m %s\n",
		len(ix.Models), len(ix.Revoked), ix.Sequence, ix.Expires.Format(time.DateOnly), map[bool]string{true: "<file>", false: *out}[*out == ""])
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

// runKeys writes keys.json: the index keys in force and the revoked key
// numbers, checked as lmget will check them against the built-in root keys.
func runKeys(args []string) int {
	fs := flag.NewFlagSet("keys", flag.ContinueOnError)
	seq := fs.Int64("sequence", 0, "sequence number, higher than the published key list")
	valid := fs.Duration("valid", 365*24*time.Hour, "how long the list stays valid (at most 400 days)")
	out := fs.String("o", "", "output file (default stdout)")
	var keyFiles, revoked nodes
	fs.Var(&keyFiles, "index-key", "public key file (minisign .pub) of a key that signs the index (repeatable)")
	fs.Var(&revoked, "revoke", "key number to revoke, as minisign prints it (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	now := time.Now().UTC().Truncate(time.Second)
	kl := lmget.KeyList{Version: 1, Sequence: *seq, Issued: now, Expires: now.Add(*valid), IndexKeys: []string{}, Revoked: []string{}}
	for _, f := range keyFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			return fail(err)
		}
		_, err = lmget.ParseMinisignKey(string(b))
		if err != nil {
			return fail(fmt.Errorf("%s: %w", f, err))
		}
		var line string
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "untrusted comment:") {
				line = l
			}
		}
		kl.IndexKeys = append(kl.IndexKeys, line)
	}
	kl.Revoked = append(kl.Revoked, revoked...)
	if err := lmget.CheckKeyList(&kl, lmget.RootKeys); err != nil {
		return fail(err)
	}
	b, _ := json.MarshalIndent(kl, "", "  ")
	b = append(b, '\n')
	if *out == "" {
		os.Stdout.Write(b)
	} else if err := os.WriteFile(*out, b, 0o644); err != nil {
		return fail(err)
	}
	var names []string
	for _, s := range kl.IndexKeys {
		k, _ := lmget.ParseMinisignKey(s)
		names = append(names, lmget.KeyNumber(k))
	}
	fmt.Fprintf(os.Stderr, "lmget-index: key list %d: index keys %s, %d revoked, valid until %s\nsign it with the root key: minisign -S -s <root key> -m %s\n",
		kl.Sequence, strings.Join(names, " "), len(kl.Revoked), kl.Expires.Format(time.DateOnly), map[bool]string{true: "<file>", false: *out}[*out == ""])
	return 0
}

// runAliases builds the signed nickname list from the maintainer's
// aliases.toml: every variant is resolved and checked at its source.
func runAliases(args []string) int {
	fs := flag.NewFlagSet("aliases", flag.ContinueOnError)
	in := fs.String("in", "aliases.toml", "the nickname source")
	seq := fs.Int64("sequence", 0, "sequence number, higher than the published list")
	valid := fs.Duration("valid", 30*24*time.Hour, "how long the list stays valid (at most 60 days)")
	out := fs.String("o", "", "output file (default stdout)")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *seq < 1 {
		fmt.Fprint(os.Stderr, "usage: lmget-index aliases -in aliases.toml -sequence N [-valid 720h] [-o aliases.json]\n")
		return 2
	}
	src, err := lmget.ReadAliasSource(*in)
	if err != nil {
		return fail(err)
	}
	c, err := lmget.New(lmget.Options{NoMirror: true})
	if err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	l, err := c.BuildAliases(ctx, src, *seq, *valid, time.Now())
	if err != nil {
		return fail(err)
	}
	b, _ := json.MarshalIndent(l, "", "  ")
	b = append(b, '\n')
	if *out == "" {
		os.Stdout.Write(b)
	} else if err := os.WriteFile(*out, b, 0o644); err != nil {
		return fail(err)
	}
	n := 0
	for _, a := range l.Aliases {
		n += len(a.Variants)
	}
	fmt.Fprintf(os.Stderr, "lmget-index: %d nicknames, %d variants, sequence %d, valid until %s\nsign it: minisign -S -s <key> -m %s\n",
		len(l.Aliases), n, l.Sequence, l.Expires.Format(time.DateOnly), map[bool]string{true: "<file>", false: *out}[*out == ""])
	return 0
}
