// Command lmget gets open models from their sources, verified.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

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
		Short: "Get open models from any source, verified.",
		Long: "lmget resolves a model at its source, shows what it will download, fetches it with\n" +
			"resumable parallel requests, verifies every file against the hash the source publishes,\n" +
			"and links it where your tools look for it (the Hugging Face cache by default).",
		Example: "  lmget Qwen/Qwen3-0.6B\n" +
			"  lmget Qwen/Qwen3-0.6B@main --to dir:./models/qwen3\n" +
			"  lmget info unsloth/Qwen3-8B-GGUF --include '*Q4_K_M*'\n" +
			"  lmget Qwen/Qwen3-0.6B --json | jq -c 'select(.type==\"file_done\")'",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       lmget.Version,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return pull(args[0], f.dryRun)
		},
	}
	get := &cobra.Command{
		Use:   "get <model>",
		Short: "Resolve, download, verify and export a model (same as lmget <model>)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return pull(args[0], f.dryRun) },
	}
	info := &cobra.Command{
		Use:   "info <model>",
		Short: "Show the plan: files, sizes, hashes, license, checks. Downloads nothing",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return pull(args[0], true) },
	}
	for _, c := range []*cobra.Command{root, get, info} {
		fs := c.Flags()
		fs.StringSliceVar(&f.include, "include", nil, "only files matching these globs (path or file name)")
		fs.StringSliceVar(&f.exclude, "exclude", nil, "skip files matching these globs")
		fs.BoolVar(&f.allowPickle, "allow-pickle", false, "keep pickle weights (.bin, .pt, ...) even when safetensors exist")
		if c != info {
			fs.StringVar(&f.to, "to", "hf", "where to link the model: hf, dir:PATH, none")
			fs.BoolVar(&f.dryRun, "dry-run", false, "stop after the plan")
			fs.IntVar(&f.conns, "conns", 8, "parallel connections per file")
		}
	}
	root.PersistentFlags().BoolVar(&f.json, "json", false, "emit one JSON event per line (schema v1)")
	root.PersistentFlags().BoolVar(&f.plain, "plain", false, "plain log lines even on a terminal")
	root.AddCommand(get, info)
	root.SetVersionTemplate("lmget {{.Version}}\n")

	if err := root.Execute(); err != nil {
		e := &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: err.Error(),
			Fixes: []lmget.Fix{{Cmd: "lmget --help"}}}
		printErr(e)
		return e.Code
	}
	return code
}

func doPull(f flags, arg string, dry bool) lmget.ExitCode {
	mode := ui.ModePlain
	switch {
	case f.json:
		mode = ui.ModeJSON
	case !f.plain && os.Getenv("TERM") != "dumb" &&
		term.IsTerminal(int(os.Stdout.Fd())) && term.IsTerminal(int(os.Stderr.Fd())):
		mode = ui.ModeTTY
	}
	fail := func(err error) lmget.ExitCode {
		e := lmget.AsError(err)
		if mode == ui.ModeJSON {
			fmt.Fprintf(os.Stdout, `{"v":%d,"type":"error","error":{"code":%d,"kind":%q,"message":%q}}`+"\n",
				lmget.SchemaVersion, e.Code, e.Kind, e.Msg)
		} else {
			printErr(e)
		}
		return e.Code
	}
	ref, err := lmget.ParseRef(arg)
	if err != nil {
		return fail(err)
	}
	target, err := lmget.ParseTarget(f.to)
	if err != nil {
		return fail(err)
	}
	c, err := lmget.New(lmget.Options{Conns: f.conns})
	if err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	req := lmget.Request{
		Ref:       ref,
		Selection: lmget.Selection{Include: f.include, Exclude: f.exclude, AllowPickle: f.allowPickle},
		Target:    target,
		DryRun:    dry,
	}
	return ui.Run(ctx, mode, c, req, os.Stdout, os.Stderr, resumeCmd())
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
	lipgloss.Fprintf(os.Stderr, "\n  %s%s\n", sErr.Render("error"), e.Msg)
	if e.Why != "" {
		lipgloss.Fprintf(os.Stderr, "  %s%s\n", sLabel.Render(""), sDim.Render(e.Why))
	}
	for i, fx := range e.Fixes {
		l := ""
		if i == 0 {
			l = "fix"
		}
		lipgloss.Fprintf(os.Stderr, "  %s%s  %s\n", sLabel.Render(l), sBold.Render(fx.Cmd), sDim.Render(fx.Note))
	}
	fmt.Fprintln(os.Stderr)
}
