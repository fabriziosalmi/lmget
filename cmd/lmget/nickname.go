package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/fabriziosalmi/lmget"
	"github.com/fabriziosalmi/lmget/internal/ui"
)

// aliasRefresh is how old the cached nickname list may be before lmget asks
// for a new one; when that fails, the cached one is used and said so.
const aliasRefresh = 24 * time.Hour

// nickname is what a nickname resolved to.
type nickname struct {
	name    string
	choice  *lmget.HistoryChoice
	ref     lmget.Ref
	include []string // the variant's files, unless the user gave --include
	to      string   // "" when the user's --to (or the default) stands
}

// nickDecision is what lmget chose for a nickname, before anyone is shown
// or asked anything: the user's own nickname, or a variant of the
// project's for this machine.
type nickDecision struct {
	name  string
	local *lmget.Ref // the user's own nickname: nothing to choose
	alias lmget.Alias
	m     lmget.Machine
	apps  []string
	ch    lmget.Choice
	note  string
	hc    *lmget.HistoryChoice
}

// decideNickname makes the choice resolveNickname shows. It prints nothing,
// so lmget mcp, whose stdout is the protocol, uses it too. to is the
// target the user named, "" when they named none.
func decideNickname(c *lmget.Client, name, to, format, forSize string) (*nickDecision, error) {
	local, err := c.LocalAliases()
	if err != nil {
		return nil, err
	}
	if ref, ok := local[name]; ok {
		return &nickDecision{name: name, local: &ref}, nil
	}

	list, note := publicAliases(c)
	if list == nil {
		e := &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: "unknown model " + lmget.Printable(fmt.Sprintf("%q", name)),
			Why: "a model is org/name, ollama:<name>, oci://<image> or an https URL; " + note,
			Fixes: []lmget.Fix{{Cmd: "lmget Qwen/Qwen3-0.6B", Note: "name the repository"},
				{Cmd: "lmget index", Note: "fetch the project's nicknames"}}}
		return nil, e
	}
	a, ok := list.Aliases[name]
	if !ok {
		names := lmget.AliasNames(local, list)
		e := &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: "unknown nickname " + fmt.Sprintf("%q", name),
			Why:   "known nicknames: " + strings.Join(names, ", "),
			Fixes: []lmget.Fix{{Cmd: "lmget Qwen/Qwen3-0.6B", Note: "or name the repository"}}}
		return nil, e
	}

	m := lmget.DetectMachine()
	apps := lmget.DetectApps(m)
	opt := lmget.ChoiceOptions{Format: format, Target: to, DiskFree: c.StoreDiskFree()}
	if forSize != "" {
		if opt.Budget, err = lmget.ParseSize(forSize); err != nil {
			return nil, err
		}
	}
	// what the user chose last time for this nickname, unless they say now
	var remembered string
	h, _ := c.History()
	if forSize == "" && format == "" {
		if last := lmget.LastNicknameChoice(h, name); last != nil {
			opt.Budget, opt.Format = last.Choice.For, last.Choice.Format
			var what []string
			if last.Choice.For > 0 {
				what = append(what, "--for "+lmget.HumanBytes(last.Choice.For))
			}
			if last.Choice.Format != "" {
				what = append(what, "--format "+last.Choice.Format)
			}
			remembered = strings.Join(what, " and ") + " as you chose on " + last.Time.Local().Format(time.DateOnly) + " (history)"
		}
	}
	// the user's own choice, as given now or remembered: the only one the
	// history records as theirs
	userFor, userFormat := opt.Budget, opt.Format
	// a variant an app could not load for memory lately: a ceiling below it,
	// which lowers the budget and never raises it
	var tooBig string
	if forSize == "" {
		opt.Ceiling, tooBig = memoryCeiling(h, name, a, time.Now())
	}
	ch, err := lmget.ChooseVariant(a, m, apps, opt)
	if err != nil && remembered != "" {
		// last time's choice does not fit any more (another target, less
		// disk): it must not lock the user out, so lmget decides afresh
		opt.Budget, opt.Format, remembered = 0, "", ""
		userFor, userFormat = 0, ""
		ch, err = lmget.ChooseVariant(a, m, apps, opt)
	}
	if err != nil {
		return nil, err
	}
	if remembered != "" {
		ch.Why = append(ch.Why, remembered)
	}
	if tooBig != "" {
		ch.Why = append(ch.Why, tooBig)
	}
	hc := &lmget.HistoryChoice{Variant: ch.Variant.Ref, Format: userFormat, For: userFor, Answer: "flag"}
	return &nickDecision{name: name, alias: a, m: m, apps: apps, ch: ch, note: note, hc: hc}, nil
}

// resolved turns a decision into the model to get. include and to stay
// empty when the user gave their own.
func (d *nickDecision) resolved(userInclude bool, userTo bool) (*nickname, error) {
	if d.local != nil {
		return &nickname{name: d.name, ref: *d.local}, nil
	}
	ref, err := lmget.ParseRef(d.ch.Variant.Ref)
	if err != nil {
		return nil, err
	}
	n := &nickname{name: d.name, choice: d.hc, ref: ref}
	if !userInclude {
		n.include = d.ch.Variant.Include
	}
	if !userTo {
		n.to = d.ch.Target
	}
	return n, nil
}

// resolveNickname turns "qwen" into a concrete model: the user's own
// nickname as written, or the project's, choosing the variant for this
// machine. Unless dry, --yes or an interactive yes is needed.
func resolveNickname(f flags, mode ui.Mode, c *lmget.Client, name string, dry bool) (*nickname, error) {
	to := ""
	if f.toChanged {
		to = f.to
	}
	d, err := decideNickname(c, name, to, f.format, f.forSize)
	if err != nil {
		return nil, err
	}
	if d.local != nil {
		say(mode, "nickname", map[string]any{"name": name, "ref": d.local.Display(), "from": c.LocalAliasesPath()},
			fmt.Sprintf("%s = %s (your nickname, %s)", name, d.local.Display(), c.LocalAliasesPath()))
		return d.resolved(len(f.include) > 0, f.toChanged)
	}
	ch, hc := d.ch, d.hc
	showChoice(mode, name, d.alias, d.m, d.apps, ch, d.note)

	if !dry && !f.yes {
		if mode == ui.ModeJSON || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) {
			return nil, &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: "a nickname needs a yes, and nobody can answer here",
				Why: "lmget chose " + ch.Variant.Ref + " for this machine; a script should not depend on that choice",
				Fixes: []lmget.Fix{{Cmd: "lmget " + name + " --yes", Note: "accept the choice"},
					{Cmd: "lmget " + strings.TrimPrefix(ch.Variant.Ref, "hf:") + " --to " + ch.Target, Note: "or name the model itself"}}}
		}
		hc.Answer = "yes"
		if !confirm(os.Stdin, os.Stderr) {
			hc.Answer = "no"
			c.AppendHistory(lmget.HistoryEntry{Command: "get", Nickname: name, Choice: hc, Exit: int(lmget.ExitUsage)})
			return nil, &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: "nothing downloaded",
				Fixes: []lmget.Fix{{Cmd: "lmget " + name + " --for 4GB", Note: "a smaller variant"},
					{Cmd: "lmget " + name + " --format gguf", Note: "another format"}}}
		}
	}
	return d.resolved(len(f.include) > 0, f.toChanged)
}

// publicAliases returns the project's nickname list: the cached one when it
// is recent, else a fresh one, else the cached one with a note.
func publicAliases(c *lmget.Client) (*lmget.AliasList, string) {
	cached, when, cerr := c.CachedAliases()
	if cached != nil && cerr == nil && time.Since(when) < aliasRefresh {
		return cached, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	fresh, err := c.UpdateAliases(ctx)
	switch {
	case err == nil && fresh != nil:
		return fresh, ""
	case err == nil:
		return nil, "the project publishes no nicknames yet"
	case cached != nil && cerr == nil:
		return cached, "could not refresh the nicknames (" + lmget.AsError(err).Msg + "): using the copy from " + when.Format(time.DateOnly)
	case cerr != nil:
		return nil, cerr.Error()
	}
	return nil, "no nickname list: " + lmget.AsError(err).Msg
}

func showChoice(mode ui.Mode, name string, a lmget.Alias, m lmget.Machine, apps []string, ch lmget.Choice, note string) {
	if mode == ui.ModeJSON {
		b, _ := json.Marshal(map[string]any{"v": lmget.SchemaVersion, "type": "nickname", "time": time.Now().UTC(), "name": name,
			"kind": a.Kind, "family": a.Family, "machine": m, "apps": apps, "choice": ch, "note": note})
		fmt.Fprintln(os.Stdout, string(b))
		return
	}
	v := ch.Variant
	w := os.Stderr
	line := func(label, s string) { fmt.Fprintf(w, "lmget: %-9s %s\n", label, lmget.Printable(s)) }
	line(name, fmt.Sprintf("%s, %s", a.Family, a.Kind))
	mach := "memory unknown"
	if m.TotalMem > 0 {
		mach = lmget.HumanBytes(m.TotalMem) + " memory"
		if m.AppleSilicon {
			mach = "Apple Silicon, " + lmget.HumanBytes(m.TotalMem) + " unified memory"
		}
	}
	if m.AvailMem > 0 {
		mach += ", about " + lmget.HumanBytes(m.AvailMem) + " free now"
	}
	if m.GPU != "" {
		mach += ", " + m.GPU + " " + lmget.HumanBytes(m.VRAMTotal)
	}
	if len(apps) > 0 {
		names := make([]string, len(apps))
		for i, a := range apps {
			names[i] = lmget.TargetName(a)
		}
		mach += "; apps: " + strings.Join(names, ", ")
	}
	line("machine", mach)
	q := v.Format
	if v.Quant != "" {
		q += " " + v.Quant
	}
	line("choice", fmt.Sprintf("%s (%s, %s, %s) into %s", strings.TrimPrefix(v.Ref, "hf:"), v.Params, q, lmget.HumanBytes(v.Bytes), lmget.TargetName(ch.Target)))
	line("because", strings.Join(ch.Why, "; "))
	if ch.Bigger != nil {
		line("larger", fmt.Sprintf("%s (%s, %s) needs more memory: --for %s", strings.TrimPrefix(ch.Bigger.Ref, "hf:"), ch.Bigger.Params, lmget.HumanBytes(ch.Bigger.Bytes), sizeFlag(ch.Bigger.Bytes)))
	}
	if ch.Tight {
		line("note", "about "+lmget.HumanBytes(m.AvailMem)+" is free now: close some apps before loading it")
	}
	if note != "" {
		line("note", note)
	}
}

// sizeFlag rounds a size up to a whole GB for a --for suggestion.
func sizeFlag(n int64) string {
	return fmt.Sprintf("%dGB", (n+999_999_999)/1_000_000_000)
}

func confirm(in io.Reader, out io.Writer) bool {
	fmt.Fprint(out, "Proceed? [Y/n] ")
	s, _ := bufio.NewReader(in).ReadString('\n')
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "" || s == "y" || s == "yes"
}

// say prints one informational line, or a JSON event.
func say(mode ui.Mode, typ string, ev map[string]any, text string) {
	if mode == ui.ModeJSON {
		ev["v"], ev["type"], ev["time"] = lmget.SchemaVersion, typ, time.Now().UTC()
		b, _ := json.Marshal(ev)
		fmt.Fprintln(os.Stdout, string(b))
		return
	}
	fmt.Fprintln(os.Stderr, "lmget: "+lmget.Printable(text))
}

// memoryCeiling is the cap the history sets on a nickname: just below the
// smallest of its variants that an app could not load for memory in the
// last 7 days, with the reason to show; 0 when there is none.
func memoryCeiling(h []lmget.HistoryEntry, name string, a lmget.Alias, now time.Time) (int64, string) {
	var ceiling int64
	why := ""
	for ref, when := range lmget.MemoryCeiling(h, name, now) {
		for _, v := range a.Variants {
			if v.Ref == ref && (ceiling == 0 || v.Bytes-1 < ceiling) {
				ceiling = v.Bytes - 1
				why = strings.TrimPrefix(ref, "hf:") + " did not fit in memory on " + when.Local().Format(time.DateOnly) + " (history)"
			}
		}
	}
	return ceiling, why
}
