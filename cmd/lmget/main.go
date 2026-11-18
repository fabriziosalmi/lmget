// Command lmget gets open models from their sources, verified.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/fabriziosalmi/lmget"
	"github.com/fabriziosalmi/lmget/internal/ui"
)

type flags struct {
	to          string
	include     []string
	exclude     []string
	allowPickle bool
	dryRun      bool
	json        bool
	plain       bool
	conns       int
	noMirror    bool
	lock        bool
	lockfile    string
	update      bool
	acceptLic   bool // --accept-license-change
	origins     []string
	verify      bool
	toChanged   bool   // --to given explicitly
	yes         bool   // accept a nickname's choice
	forSize     string // --for: memory a nickname's model may use
	format      string // --format: a nickname's format
	endpoint    string // run: an OpenAI-compatible server
	modelID     string // run: the name the app knows the model by
	maxTokens   int    // run: the longest answer
}

func main() { os.Exit(int(run())) }

func run() lmget.ExitCode {
	var f flags
	code := lmget.ExitOK
	pull := func(arg string, dry bool) error {
		code = doPull(f, arg, dry)
		return nil
	}

	root := &cobra.Command{
		Use:   "lmget <model>",
		Short: "Get open models from their sources, verified.",
		Long: "lmget resolves a model at its source, shows what it will download, fetches it with\n" +
			"resumable parallel requests, verifies every file against the hash the source publishes,\n" +
			"and links it where your tools look for it (the Hugging Face cache by default).",
		Example: "  lmget Qwen/Qwen3-0.6B\n" +
			"  lmget Qwen/Qwen3-0.6B@main --to dir:./models/qwen3\n" +
			"  lmget info unsloth/Qwen3-8B-GGUF --include '*Q4_K_M*'\n" +
			"  lmget ollama:smollm2:135m              # into Ollama, ready for ollama run\n" +
			"  lmget Qwen/Qwen3-0.6B --lock          # record it in ./lmget.lock\n" +
			"  lmget sync                            # reproduce ./lmget.lock exactly\n" +
			"  lmget Qwen/Qwen3-0.6B --json | jq -c 'select(.type==\"file_done\")'",
		Args:          oneModel("lmget", "Qwen/Qwen3-0.6B", true),
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       lmget.Version,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			f.toChanged = cmd.Flags().Changed("to")
			return pull(args[0], f.dryRun)
		},
	}
	get := &cobra.Command{
		Use:   "get <model>",
		Short: "Resolve, download, verify and export a model (same as lmget <model>)",
		Args:  oneModel("lmget get", "Qwen/Qwen3-0.6B", false),
		RunE: func(cmd *cobra.Command, args []string) error {
			f.toChanged = cmd.Flags().Changed("to")
			return pull(args[0], f.dryRun)
		},
	}
	info := &cobra.Command{
		Use:   "info <model>",
		Short: "Show the plan: files, sizes, hashes, license, checks. Downloads nothing",
		Args:  oneModel("lmget info", "Qwen/Qwen3-0.6B", false),
		RunE:  func(cmd *cobra.Command, args []string) error { return pull(args[0], true) },
	}
	sync := &cobra.Command{
		Use:   "sync [model...]",
		Short: "Reproduce lmget.lock: the pinned commits, verified against the locked sha256",
		Long: "sync downloads what lmget.lock pins, at the locked commit, and checks every file\n" +
			"against the sha256 in the lock. When everything is already in the store it does not\n" +
			"use the network. With --update it resolves each revision again and rewrites the lock.",
		RunE: func(cmd *cobra.Command, args []string) error {
			code = doSync(f, args)
			return nil
		},
	}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List the models in the store, their size and where they are exported",
		Args:  noArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { code = doLs(f); return nil },
	}
	rm := &cobra.Command{
		Use:   "rm <model>",
		Short: "Remove a model: its export links, then every blob no other model uses",
		Long: "rm removes the links lmget created for the model (only while they still point at\n" +
			"lmget's data), forgets the model, and deletes the blobs no other model references.\n" +
			"<model>@<commit> removes one commit; without it, every stored commit of the repository.",
		Args: oneModel("lmget rm", "", false),
		RunE: func(cmd *cobra.Command, args []string) error { code = doRm(f, args[0]); return nil },
	}
	doctor := &cobra.Command{
		Use:   "doctor",
		Short: "Check the store, the export targets, the network and the token",
		Args:  noArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { code = doDoctor(f); return nil },
	}
	index := &cobra.Command{
		Use:   "index",
		Short: "Fetch the signed index of mirrored models, verify it and show it",
		Long: "The index lists the models the project mirrors, with every file's sha256 and the\n" +
			"nodes that serve them, signed with minisign. lmget verifies the signature, refuses\n" +
			"an older or expired index, and keeps the verified copy in the store.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error { code = doIndex(f); return nil },
	}
	doctor.Flags().BoolVar(&f.verify, "verify", false, "re-hash every blob; corrupt ones are moved to quarantine")
	for _, c := range []*cobra.Command{root, get, info} {
		fs := c.Flags()
		fs.StringSliceVar(&f.include, "include", nil, "only files matching these globs (path or file name)")
		fs.StringSliceVar(&f.exclude, "exclude", nil, "skip files matching these globs")
		fs.BoolVar(&f.allowPickle, "allow-pickle", false, "keep pickle weights (.bin, .pt, ...) even when safetensors exist")
		fs.StringVar(&f.forSize, "for", "", "with a nickname: memory the model may use, e.g. 8GB (default: measured here)")
		fs.StringVar(&f.format, "format", "", "with a nickname: safetensors, mlx, gguf or ollama (default: what the app loads)")
		if c != info {
			fs.BoolVarP(&f.yes, "yes", "y", false, "with a nickname: accept the proposed model without asking")
		}
		if c != info {
			fs.StringVar(&f.to, "to", "hf", "where to link the model: hf, ollama, omlx, lmstudio, jan, localai, exo, dir:PATH, none (Ollama models default to ollama)")
			fs.BoolVar(&f.dryRun, "dry-run", false, "stop after the plan")
			fs.BoolVar(&f.lock, "lock", false, "record the result in the lockfile, creating it if needed")
		}
	}
	for _, c := range []*cobra.Command{root, get, sync} {
		c.Flags().IntVar(&f.conns, "conns", 8, "parallel connections per file")
		c.Flags().BoolVar(&f.noMirror, "no-mirror", false, "never fall back to ModelScope when Hugging Face fails")
		c.Flags().StringSliceVar(&f.origins, "origin", nil, "ask this blob server first (URL; repeatable; default $LMGET_ORIGINS)")
		c.Flags().BoolVar(&f.acceptLic, "accept-license-change", false, "go on when the license differs from the one in the lockfile, and record the new one")
	}
	sync.Flags().BoolVar(&f.update, "update", false, "resolve revisions again and rewrite the lock")
	root.PersistentFlags().BoolVar(&f.json, "json", false, "emit one JSON event per line (schema v1)")
	root.PersistentFlags().BoolVar(&f.plain, "plain", false, "plain log lines even on a terminal")
	root.PersistentFlags().StringVar(&f.lockfile, "lockfile", lmget.DefaultLockName, "lockfile path")
	history := &cobra.Command{
		Use:   "history [clear]",
		Short: "Show what lmget did here, or clear it (LMGET_HISTORY=off turns it off)",
		Long: "lmget keeps a local record of its pulls (model, commit, bytes, origins, speed, how\n" +
			"it ended, and how you took a nickname's proposal) in history.jsonl in the store, so it\n" +
			"can order the index nodes by measured speed and remember your choice for a nickname.\n" +
			"It holds no token, prompt or path outside the store, and goes nowhere.",
		RunE: func(cmd *cobra.Command, args []string) error { code = doHistory(f, args); return nil },
	}
	run := &cobra.Command{
		Use:   "run <model> [prompt...]",
		Short: "Get a model, link it into the app that runs it, and ask it (lmget runs no model itself)",
		Long: "run does what lmget get does, links the model where oMLX, LM Studio or Ollama loads\n" +
			"it (or uses --endpoint, an OpenAI-compatible server), and sends the prompt to that\n" +
			"running app. The answer goes to stdout. The prompt can also come on stdin.",
		Example: "  lmget run qwen \"what model are you?\"\n" +
			"  lmget run ollama:smollm2:135m \"hello\"\n" +
			"  echo \"summarize: ...\" | lmget run qwen --yes",
		RunE: func(cmd *cobra.Command, args []string) error {
			f.toChanged = cmd.Flags().Changed("to")
			code = doRun(f, args)
			return nil
		},
	}
	rf := run.Flags()
	rf.StringVar(&f.to, "to", "hf", "the app to run it in: omlx, lmstudio, ollama (default: an installed one that loads it)")
	rf.StringVar(&f.endpoint, "endpoint", "", "an OpenAI-compatible server to ask instead (vLLM, llama-server); the model goes to the Hugging Face cache")
	rf.StringVar(&f.modelID, "model-id", "", "the name the app knows the model by (default: found in its model list)")
	rf.IntVar(&f.maxTokens, "max-tokens", 512, "the longest answer, in tokens")
	rf.BoolVarP(&f.yes, "yes", "y", false, "with a nickname: accept the proposed model without asking")
	rf.StringVar(&f.forSize, "for", "", "with a nickname: memory the model may use, e.g. 8GB")
	rf.StringVar(&f.format, "format", "", "with a nickname: safetensors, mlx, gguf or ollama")
	rf.IntVar(&f.conns, "conns", 8, "parallel connections per file")
	mcp := &cobra.Command{
		Use:   "mcp",
		Short: "Serve lmget to an AI agent over the Model Context Protocol (stdin and stdout)",
		Long: "mcp speaks the Model Context Protocol on stdin and stdout, for agents such as Claude Code,\n" +
			"Cursor or Zed. Its tools: info (the plan, read only), list (the store, read only) and get,\n" +
			"which downloads only when called with confirm: true and links only into the apps and the\n" +
			"Hugging Face cache, never into a folder an agent picks.",
		Example: "  claude mcp add lmget -- lmget mcp",
		Args:    noArgs,
		RunE:    func(cmd *cobra.Command, args []string) error { code = doMCP(); return nil },
	}
	root.AddCommand(get, info, sync, ls, rm, doctor, index, history, run, mcp)
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: err.Error(),
			Fixes: []lmget.Fix{{Cmd: c.CommandPath() + " --help"}}}
	})
	root.SetVersionTemplate("lmget {{.Version}}\n")

	if err := root.Execute(); err != nil {
		e := &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: err.Error(),
			Fixes: []lmget.Fix{{Cmd: "lmget --help"}}}
		errors.As(err, &e) // an argument error already says what to do
		printErr(e)
		return e.Code
	}
	return code
}

// oneModel checks that a command got exactly one model (or none, when
// optional: lmget alone shows the help). With more, the usual cause is a
// glob the shell expanded because it was not quoted.
func oneModel(cmd, example string, optional bool) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		switch {
		case len(args) == 1, len(args) == 0 && optional:
			return nil
		case len(args) == 0:
			e := &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: cmd + " needs a model",
				Why: "a Hugging Face repository (org/name), ollama:<name>, oci://<image> or an https URL"}
			if example != "" {
				e.Fixes = append(e.Fixes, lmget.Fix{Cmd: cmd + " " + example})
			} else {
				e.Why = "a model in the store, as lmget ls lists it"
				e.Fixes = append(e.Fixes, lmget.Fix{Cmd: "lmget ls", Note: "the models in the store"})
			}
			e.Fixes = append(e.Fixes, lmget.Fix{Cmd: cmd + " --help"})
			return e
		}
		return &lmget.Error{Code: lmget.ExitUsage, Kind: "usage",
			Msg:   fmt.Sprintf("%s takes one model, got %d: %s", cmd, len(args), strings.Join(args, " ")),
			Why:   "one model per command; an unquoted glob is expanded by the shell before lmget sees it",
			Fixes: []lmget.Fix{{Cmd: cmd + " org/name --include '*Q4_K_M*'", Note: "quote globs"}}}
	}
}

// noArgs is cobra.NoArgs with an lmget error.
func noArgs(c *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return &lmget.Error{Code: lmget.ExitUsage, Kind: "usage",
		Msg:   fmt.Sprintf("lmget %s takes no arguments, got %s", c.Name(), strings.Join(args, " ")),
		Fixes: []lmget.Fix{{Cmd: "lmget " + c.Name() + " --help"}}}
}

func outputMode(f flags) ui.Mode {
	switch {
	case f.json:
		return ui.ModeJSON
	case !f.plain && os.Getenv("TERM") != "dumb" &&
		term.IsTerminal(int(os.Stdout.Fd())) && term.IsTerminal(int(os.Stderr.Fd())):
		return ui.ModeTTY
	}
	return ui.ModePlain
}

func fail(mode ui.Mode, err error) lmget.ExitCode {
	e := lmget.AsError(err)
	if mode == ui.ModeJSON {
		b, _ := json.Marshal(map[string]any{"v": lmget.SchemaVersion, "type": "error", "time": time.Now().UTC(), "error": e})
		fmt.Fprintln(os.Stdout, string(b))
	} else {
		printErr(e)
	}
	return e.Code
}

func doPull(f flags, arg string, dry bool) lmget.ExitCode {
	_, code := pullModel(f, arg, dry, "get", os.Stdout, nil)
	return code
}

// pulled is a pull that went through: what run needs to ask the model.
type pulled struct {
	c    *lmget.Client
	out  *lmget.Outcome
	req  lmget.Request
	nick *nickname
}

// pullModel is lmget get: nickname, target, lock, pull, history. chooseTo,
// when set, picks the target of a model whose --to was not given (run needs
// an app, not the Hugging Face cache). The path and the plan rows go to
// stdout, which run points elsewhere: its stdout is the model's answer.
func pullModel(f flags, arg string, dry bool, command string, stdout io.Writer,
	chooseTo func(*lmget.Client, lmget.Ref, lmget.Selection) (string, error)) (*pulled, lmget.ExitCode) {
	mode := outputMode(f)
	fail := func(mode ui.Mode, err error) (*pulled, lmget.ExitCode) { return nil, fail(mode, err) }
	var nick *nickname
	if lmget.IsAliasName(arg) {
		c, err := lmget.New(lmget.Options{})
		if err != nil {
			return fail(mode, err)
		}
		if nick, err = resolveNickname(f, mode, c, arg, dry); err != nil {
			return fail(mode, err)
		}
		if nick.include != nil {
			f.include = nick.include
		}
		if nick.to != "" {
			f.to, f.toChanged = nick.to, true
		}
		arg = nick.ref.String()
	} else if f.forSize != "" || f.format != "" || f.yes {
		return fail(mode, &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: "--for, --format and --yes go with a nickname",
			Why: arg + " names the model itself: lmget downloads exactly that"})
	}
	ref, err := lmget.ParseRef(arg)
	if err != nil {
		return fail(mode, err)
	}
	to := f.to
	switch {
	case ref.Source == "url" && !f.toChanged:
		to = "dir:." // a single file: next to the user, like curl -O
	case ref.Source == "ollama" && !f.toChanged:
		to = "ollama" // where ollama run looks
	case ref.Source == "oci" && !f.toChanged:
		to = "dir:./" + path.Base(ref.Repo) // no runtime reads OCI models from a cache lmget knows
	}
	sel := lmget.Selection{Include: f.include, Exclude: f.exclude, AllowPickle: f.allowPickle}
	if ref.File != "" { // .../resolve/<rev>/<file>: that file only, recorded as a selection
		sel.Include = []string{lmget.GlobEscape(ref.File)}
	}
	if chooseTo != nil && !f.toChanged {
		c, err := lmget.New(lmget.Options{})
		if err != nil {
			return fail(mode, err)
		}
		if to, err = chooseTo(c, ref, sel); err != nil {
			return fail(mode, err)
		}
	}
	target, err := lmget.ParseTarget(to)
	if err != nil {
		return fail(mode, err)
	}
	_, statErr := os.Stat(f.lockfile)
	locking := !dry && (f.lock || statErr == nil)
	if locking {
		if err := lmget.Lockable(ref); err != nil {
			return fail(mode, err)
		}
		if err := lmget.LockableTarget(f.lockfile, target); err != nil {
			return fail(mode, err)
		}
	}
	c, err := lmget.New(lmget.Options{Conns: f.conns, NoMirror: f.noMirror, Origins: f.origins})
	if err != nil {
		return fail(mode, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	preferFasterNodes(c)
	req := lmget.Request{Ref: ref, Selection: sel, Target: target, DryRun: dry, AcceptLicenseChange: f.acceptLic}
	if locking && statErr == nil { // a model already in the lockfile keeps the license recorded there
		l, err := lmget.ReadLock(f.lockfile)
		if err != nil {
			return fail(mode, err)
		}
		if m := l.Find(ref); m != nil {
			req.License = m.License
		}
	}
	opts := ui.Opts{Resume: resumeCmd()}
	// lmget keeps an existing lockfile current, like go get with go.mod.
	if locking {
		opts.After = func(o *lmget.Outcome) (string, error) {
			_, err := lmget.UpdateLock(f.lockfile, true, func(l *lmget.Lock) (bool, error) {
				return true, l.Upsert(o.Plan, sel, target)
			})
			if err != nil {
				return "", err
			}
			return f.lockfile, nil
		}
	}
	out, code := ui.Run(ctx, mode, c, req, stdout, os.Stderr, opts)
	recordPull(c, command, req, out, code, nick)
	if code == lmget.ExitOK && !dry && command == "get" {
		staleHint(c, mode)
	}
	return &pulled{c: c, out: out, req: req, nick: nick}, code
}

func doSync(f flags, args []string) lmget.ExitCode {
	mode := outputMode(f)
	l, err := lmget.ReadLock(f.lockfile)
	if errors.Is(err, fs.ErrNotExist) {
		return fail(mode, &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: "no " + f.lockfile + " here",
			Fixes: []lmget.Fix{{Cmd: "lmget get <model> --lock", Note: "creates it"}}})
	}
	if err != nil {
		return fail(mode, err)
	}
	var models []*lmget.LockModel
	if len(args) == 0 {
		for i := range l.Models {
			models = append(models, &l.Models[i])
		}
	}
	for _, a := range args {
		ref, err := lmget.ParseRef(a)
		if err != nil {
			return fail(mode, err)
		}
		m := l.Find(ref)
		if m == nil {
			return fail(mode, &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: ref.Display() + " is not in " + f.lockfile,
				Fixes: []lmget.Fix{{Cmd: "lmget get " + ref.Display() + " --lock", Note: "adds it"}}})
		}
		models = append(models, m)
	}
	if len(models) == 0 {
		return fail(mode, &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: f.lockfile + " lists no models"})
	}
	c, err := lmget.New(lmget.Options{Conns: f.conns, NoMirror: f.noMirror, Origins: f.origins})
	if err != nil {
		return fail(mode, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	preferFasterNodes(c)
	if mode == ui.ModeTTY {
		verb := "sync"
		if f.update {
			verb = "update"
		}
		lipgloss.Fprintf(os.Stderr, "\n  %s %s  %s\n", sBold.Render(verb), f.lockfile, sDim.Render(fmt.Sprintf("%d models", len(models))))
	}
	for _, m := range models {
		req, err := l.Request(m, f.update)
		if err != nil {
			return fail(mode, err)
		}
		req.AcceptLicenseChange = f.acceptLic
		before := *m
		opts := ui.Opts{Resume: resumeCmd(), After: func(o *lmget.Outcome) (string, error) {
			// another lmget may have changed this entry while we downloaded:
			// then its version is newer than the one this sync started from
			wrote, err := lmget.UpdateLockEntry(f.lockfile, before, o.Plan, req.Selection, req.Target)
			if err != nil || !wrote {
				return "", err
			}
			return f.lockfile, nil
		}}
		out, code := ui.Run(ctx, mode, c, req, os.Stdout, os.Stderr, opts)
		recordPull(c, "sync", req, out, code, nil)
		if code != lmget.ExitOK {
			return code
		}
	}
	return lmget.ExitOK
}

func doLs(f flags) lmget.ExitCode {
	mode := outputMode(f)
	c, err := lmget.New(lmget.Options{})
	if err != nil {
		return fail(mode, err)
	}
	models, err := c.List()
	if err != nil {
		return fail(mode, err)
	}
	ui.List(mode, os.Stdout, models, c.Stats())
	return lmget.ExitOK
}

func doRm(f flags, arg string) lmget.ExitCode {
	mode := outputMode(f)
	ref, err := lmget.ParseRef(arg)
	if err != nil {
		return fail(mode, err)
	}
	c, err := lmget.New(lmget.Options{})
	if err != nil {
		return fail(mode, err)
	}
	res, err := c.Remove(ref)
	if err != nil {
		return fail(mode, err)
	}
	c.AppendHistory(lmget.HistoryEntry{Command: "rm", Ref: ref.Redacted().String()})
	note := ""
	if l, err := lmget.ReadLock(f.lockfile); err == nil {
		for _, m := range l.Models {
			if r, err := lmget.ParseRef(m.Ref); err == nil && strings.EqualFold(r.Repo, ref.Repo) {
				note = "still listed in " + f.lockfile + ": lmget sync will download it again"
			}
		}
	}
	ui.Removed(mode, os.Stdout, res, note)
	return lmget.ExitOK
}

func doDoctor(f flags) lmget.ExitCode {
	mode := outputMode(f)
	c, err := lmget.New(lmget.Options{})
	if err != nil {
		return fail(mode, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return ui.Doctor(mode, os.Stdout, c.Doctor(ctx, lmget.DoctorOptions{Verify: f.verify, LockPath: f.lockfile}))
}

func doIndex(f flags) lmget.ExitCode {
	mode := outputMode(f)
	c, err := lmget.New(lmget.Options{})
	if err != nil {
		return fail(mode, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ix, err := c.UpdateIndex(ctx)
	if err != nil {
		return fail(mode, err)
	}
	ks, _ := c.IndexKeyState() // the index just verified: the state cannot fail now
	ui.IndexSummary(mode, os.Stdout, ix, ks)
	// the nicknames travel with the index; a project without them is fine
	if al, err := c.UpdateAliases(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "lmget: nicknames not updated: "+lmget.Printable(lmget.AsError(err).Msg))
	} else if al != nil && mode != ui.ModeJSON {
		fmt.Fprintf(os.Stderr, "lmget: %d nicknames, sequence %d, valid until %s\n", len(al.Aliases), al.Sequence, al.Expires.UTC().Format("2006-01-02"))
	}
	return lmget.ExitOK
}

func resumeCmd() string {
	args := make([]string, 0, len(os.Args))
	args = append(args, "lmget")
	for _, a := range os.Args[1:] {
		if strings.ContainsAny(a, " *?'\"") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		args = append(args, a)
	}
	return strings.Join(args, " ")
}

var (
	sErr   = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true).Width(10)
	sLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Width(10)
	sDim   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	sBold  = lipgloss.NewStyle().Bold(true)
)

func printErr(e *lmget.Error) {
	p := lmget.Printable // messages can quote a lockfile or a source
	lipgloss.Fprintf(os.Stderr, "\n  %s%s\n", sErr.Render("error"), p(e.Msg))
	if e.Why != "" {
		lipgloss.Fprintf(os.Stderr, "  %s%s\n", sLabel.Render(""), sDim.Render(p(e.Why)))
	}
	if e.Cause != "" && e.Cause != e.Msg {
		lipgloss.Fprintf(os.Stderr, "  %s%s\n", sLabel.Render("cause"), sDim.Render(p(e.Cause)))
	}
	for i, fx := range e.Fixes {
		l := ""
		if i == 0 {
			l = "fix"
		}
		lipgloss.Fprintf(os.Stderr, "  %s%s  %s\n", sLabel.Render(l), sBold.Render(p(fx.Cmd)), sDim.Render(p(fx.Note)))
	}
	fmt.Fprintln(os.Stderr)
}
