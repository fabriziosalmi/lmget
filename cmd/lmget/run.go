package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/fabriziosalmi/lmget"
	"github.com/fabriziosalmi/lmget/internal/ui"
)

// doRun is lmget run: get the model as lmget get does, link it where an app
// loads it, then ask that app. lmget runs no model itself.
func doRun(f flags, args []string) lmget.ExitCode {
	mode := outputMode(f)
	if len(args) == 0 {
		return fail(mode, &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: "lmget run needs a model and a prompt",
			Fixes: []lmget.Fix{{Cmd: `lmget run qwen "what model are you?"`}}})
	}
	model, prompt := args[0], strings.TrimSpace(strings.Join(args[1:], " "))
	if prompt == "" && !term.IsTerminal(int(os.Stdin.Fd())) {
		b, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
		prompt = strings.TrimSpace(string(b))
	}
	if prompt == "" {
		return fail(mode, &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: "no prompt",
			Why:   "give it after the model, or on stdin",
			Fixes: []lmget.Fix{{Cmd: `lmget run ` + model + ` "what model are you?"`}}})
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if f.endpoint != "" && !f.toChanged { // a nickname too: such servers read the Hugging Face cache
		f.to, f.toChanged = "hf", true
	}

	chooseTo := func(c *lmget.Client, ref lmget.Ref, sel lmget.Selection) (string, error) {
		switch {
		case f.endpoint != "":
			return "hf", nil // vLLM, llama-server and the like read the Hugging Face cache
		case ref.Source == "ollama":
			return "ollama", nil
		case ref.Source != "hf":
			return "", &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: "lmget run asks an app for a Hugging Face or Ollama model",
				Fixes: []lmget.Fix{{Cmd: "lmget get " + ref.Display(), Note: "download it, then load it yourself"}}}
		}
		plan, err := c.Resolve(ctx, ref, sel)
		if err != nil {
			return "", err
		}
		apps := lmget.DetectApps(lmget.DetectMachine())
		for _, a := range apps {
			if a == "ollama" {
				continue // it holds registry models only
			}
			t, err := lmget.ParseTarget(a)
			if err == nil && lmget.TargetAccepts(plan, t) == nil {
				return a, nil
			}
		}
		return "", &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: "no app installed here loads " + ref.Display(),
			Why: "found: " + strings.Join(append(apps, "none")[:max(len(apps), 1)], ", "),
			Fixes: []lmget.Fix{{Cmd: "lmget run " + ref.Display() + " --to lmstudio", Note: "or --to omlx"},
				{Cmd: "lmget run " + ref.Display() + " --endpoint http://127.0.0.1:8000", Note: "an OpenAI-compatible server"}}}
	}
	p, code := pullModel(f, model, false, "get", os.Stderr, chooseTo)
	if code != lmget.ExitOK {
		return code
	}

	record := func(code lmget.ExitCode, err error) lmget.ExitCode {
		e := lmget.HistoryEntry{Command: "run", Ref: p.req.Ref.Redacted().String(), Target: p.req.Target.Kind, Exit: int(code)}
		if p.out != nil && p.out.Plan != nil {
			e.Commit = p.out.Plan.Commit
		}
		if p.nick != nil {
			e.Nickname, e.Choice = p.nick.name, p.nick.choice
		}
		if err != nil {
			e.Error = lmget.AsError(err).Kind
		}
		p.c.AppendHistory(e) // never the prompt, never the answer
		return code
	}
	failRun := func(err error) lmget.ExitCode { return record(fail(mode, err), err) }

	e, err := lmget.EngineFor(p.req.Target.Kind, f.endpoint)
	if err != nil {
		return failRun(err)
	}
	say := func(s string) {
		if mode != ui.ModeJSON {
			fmt.Fprintln(os.Stderr, "lmget: "+lmget.Printable(s))
		}
	}
	if e.Remote() {
		say("the prompt goes to " + e.URL + ", another machine")
	}
	if err := p.c.Identify(ctx, e); err != nil {
		return failRun(err)
	}
	if err := p.c.Reload(ctx, e); err != nil {
		return failRun(err)
	}
	id := f.modelID
	if id == "" {
		runAs := ""
		if p.out != nil {
			runAs = p.out.RunAs
		}
		if id, err = p.c.ModelID(ctx, e, p.out.Plan, runAs); err != nil {
			return failRun(err)
		}
	}
	say(fmt.Sprintf("asking %s at %s (%s)", e.App, e.URL, id))

	var answer strings.Builder
	out, thinking := io.Writer(os.Stdout), io.Writer(os.Stderr)
	if mode == ui.ModeJSON {
		out, thinking = &answer, nil
	}
	st, err := p.c.Ask(ctx, e, id, prompt, f.maxTokens, out, thinking)
	if err != nil {
		return failRun(err)
	}
	if mode == ui.ModeJSON {
		b, _ := json.Marshal(map[string]any{"v": lmget.SchemaVersion, "type": "answer", "time": time.Now().UTC(),
			"app": e.App, "url": e.URL, "model": id, "text": answer.String(),
			"first_token_s": st.FirstToken.Seconds(), "total_s": st.Total.Seconds()})
		fmt.Fprintln(os.Stdout, string(b))
	} else {
		fmt.Fprintln(os.Stdout)
		say(fmt.Sprintf("answered by %s in %.1fs (first words after %.1fs)", e.App, st.Total.Seconds(), st.FirstToken.Seconds()))
	}
	return record(lmget.ExitOK, nil)
}
