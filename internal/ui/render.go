// Package ui renders the lmget Event stream: a live terminal view, plain log
// lines for pipes and CI, and JSON lines for integrations.
package ui

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/fabriziosalmi/lmget"
)

// ANSI 16 colors: they follow the user's terminal theme, light or dark.
var (
	sAccent = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	sDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	sBold   = lipgloss.NewStyle().Bold(true)
	sWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	sErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	sLabel  = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Width(10)
)

const check = "✓"

func hb(n int64) string { return lmget.HumanBytes(n) }

func tilde(p string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" && strings.HasPrefix(p, h+string(os.PathSeparator)) {
		return "~" + p[len(h):]
	}
	return p
}

func shortHash(f lmget.File) string {
	switch {
	case f.SHA256 != "":
		return "sha256 " + f.SHA256[:8]
	case f.GitSHA1 != "":
		return "git    " + f.GitSHA1[:8]
	}
	return ""
}

func middleTrunc(s string, max int) string {
	if max < 8 || lipgloss.Width(s) <= max {
		return s
	}
	keep := max - 1
	head := keep / 2
	return s[:head] + "…" + s[len(s)-(keep-head):]
}

func pad(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func padLeft(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}

// renderPlan is the block shown before anything is downloaded. full lists
// every file (lmget info); otherwise the largest files and a summary line.
func renderPlan(p *lmget.Plan, width int, full bool) string {
	var b strings.Builder
	sel := p.Selected()
	head := []string{sBold.Render(middleTrunc(p.Ref.Display(), max(30, width-40))), sDim.Render(p.Commit[:7])}
	if p.Ref.Source == "url" {
		head = head[:1] // a URL has no commit; its id is internal
	}
	if p.License != "" {
		head = append(head, p.License)
	} else if p.Ref.Source == "url" {
		head = append(head, sDim.Render("license unknown"))
	} else {
		head = append(head, sWarn.Render("no license"))
	}
	count := fmt.Sprintf("%d files", len(sel))
	if len(sel) == 1 {
		count = "1 file"
	}
	head = append(head, hb(p.Bytes), count)
	fmt.Fprintf(&b, "\n  %s\n\n", strings.Join(head, "  "))

	files := append([]lmget.File(nil), sel...)
	sort.SliceStable(files, func(i, j int) bool { return files[i].Size > files[j].Size })
	shown := files
	if !full {
		shown = nil
		for _, f := range files {
			if len(shown) < 6 && (f.Size*100 >= p.Bytes || len(shown) < 2) {
				shown = append(shown, f)
			}
		}
	}
	nameW := 0
	for _, f := range shown {
		nameW = max(nameW, lipgloss.Width(f.Path))
	}
	nameW = min(nameW, max(20, width-40))
	for _, f := range shown {
		state := sDim.Render(shortHash(f))
		if f.Cached {
			state = sAccent.Render("cached")
		}
		fmt.Fprintf(&b, "    %s  %s  %s\n", pad(middleTrunc(f.Path, nameW), nameW), padLeft(hb(f.Size), 8), state)
	}
	if rest := len(files) - len(shown); rest > 0 {
		var n int64
		for _, f := range files[len(shown):] {
			n += f.Size
		}
		label := fmt.Sprintf("+ %d more files", rest)
		if rest == 1 {
			label = "+ 1 more file"
		}
		fmt.Fprintf(&b, "    %s  %s\n", pad(sDim.Render(label), nameW), padLeft(hb(n), 8))
	}

	var checks []string
	hasST := false
	for _, f := range sel {
		if strings.HasSuffix(strings.ToLower(f.Path), ".safetensors") {
			hasST = true
		}
	}
	if hasST {
		checks = append(checks, sAccent.Render("safetensors"))
	}
	switch p.HashFrom {
	case lmget.HashFromUser:
		checks = append(checks, sAccent.Render("sha256 pinned by you"))
	case lmget.HashFromServer:
		checks = append(checks, sAccent.Render("sha256 published by the server"))
	case lmget.HashFromFirst:
		checks = append(checks, sWarn.Render("no published hash: pinned on first download"))
	}
	if len(p.Pickle) == 0 {
		checks = append(checks, sAccent.Render("no pickle"))
	} else {
		checks = append(checks, sWarn.Render(fmt.Sprintf("%d pickle files", len(p.Pickle))))
	}
	if p.Ref.Source == "url" {
		// gating is a Hugging Face concept
	} else if p.Gated == "" {
		checks = append(checks, sAccent.Render("not gated"))
	} else {
		checks = append(checks, sWarn.Render("gated ("+p.Gated+")"))
	}
	if cached := p.Bytes - p.ToFetch; cached > 0 {
		checks = append(checks, sAccent.Render(hb(cached)+" cached"))
	}
	if p.DiskFree >= 0 {
		if p.ToFetch > p.DiskFree {
			checks = append(checks, sErr.Render(hb(p.DiskFree)+" free"))
		} else {
			checks = append(checks, sDim.Render(hb(p.DiskFree)+" free"))
		}
	}
	fmt.Fprintf(&b, "\n  %s%s\n", sLabel.Render("checks"), strings.Join(checks, sDim.Render(" · ")))

	skipped := map[string][]string{}
	for _, f := range p.Files {
		if !f.Selected() {
			skipped[f.Skipped] = append(skipped[f.Skipped], f.Path)
		}
	}
	reasons := make([]string, 0, len(skipped))
	for r := range skipped {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		line := fmt.Sprintf("%d files (%s)", len(skipped[r]), r)
		if len(skipped[r]) == 1 {
			line = fmt.Sprintf("%s (%s)", path.Base(skipped[r][0]), r)
		}
		hint := ""
		if strings.HasPrefix(r, "pickle") {
			hint = sDim.Render("  --allow-pickle to include")
		}
		fmt.Fprintf(&b, "  %s%s%s\n", sLabel.Render("skipped"), line, hint)
	}
	return b.String()
}

func renderError(e *lmget.Error) string {
	var b strings.Builder
	label := "error"
	if e.Code == lmget.ExitInterrupted {
		label = "stopped"
	}
	fmt.Fprintf(&b, "\n  %s%s\n", sErr.Width(10).Render(label), e.Msg)
	if e.Why != "" {
		fmt.Fprintf(&b, "  %s%s\n", sLabel.Render(""), sDim.Render(e.Why))
	}
	if e.Cause != "" && e.Cause != e.Msg {
		fmt.Fprintf(&b, "  %s%s\n", sLabel.Render("cause"), sDim.Render(e.Cause))
	}
	for i, f := range e.Fixes {
		l := ""
		if i == 0 {
			l = "fix"
		}
		switch {
		case f.Cmd != "" && f.Note != "":
			fmt.Fprintf(&b, "  %s%s  %s\n", sLabel.Render(l), sBold.Render(f.Cmd), sDim.Render(f.Note))
		case f.Cmd != "":
			fmt.Fprintf(&b, "  %s%s\n", sLabel.Render(l), sBold.Render(f.Cmd))
		default:
			fmt.Fprintf(&b, "  %s%s\n", sLabel.Render(l), f.Note)
		}
	}
	return b.String()
}

func renderSummary(o *lmget.Outcome, t lmget.Target, storeRoot, lock string, width int) string {
	var b strings.Builder
	sel := o.Plan.Selected()
	what := fmt.Sprintf("%d/%d verified", len(sel), len(sel))
	if o.Plan.HashFrom == lmget.HashFromFirst {
		what = sWarn.Render("pinned on first download")
	}
	took := "in " + fmtDur(o.Elapsed)
	if o.Fetched == 0 {
		took = sDim.Render("already complete, nothing downloaded")
	}
	name := o.Plan.Ref.Display()
	if width > 0 {
		name = middleTrunc(name, max(30, width-50))
	}
	fmt.Fprintf(&b, "\n  %s %s  %s  %s  %s\n", sAccent.Render(check), sBold.Render(name), hb(o.Plan.Bytes), what, took)
	switch t.Kind {
	case "hf":
		fmt.Fprintf(&b, "    %s%s\n", sLabel.Render("hf cache"), tilde(o.Path))
		fmt.Fprintf(&b, "    %s%s %s\n", sLabel.Render("load as"), sBold.Render(quote(o.Plan.Ref.Repo)), sDim.Render("with transformers or mlx-lm"))
	case "dir":
		fmt.Fprintf(&b, "    %s%s\n", sLabel.Render("dir"), tilde(o.Path))
	default:
		fmt.Fprintf(&b, "    %s%s\n", sLabel.Render("store"), tilde(storeRoot))
	}
	if lock != "" {
		fmt.Fprintf(&b, "    %s%s %s\n", sLabel.Render("lock"), tilde(lock), sDim.Render("updated"))
	}
	return b.String()
}

func quote(s string) string { return "\"" + s + "\"" }

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func bar(frac float64, w int) string {
	frac = max(0, min(1, frac))
	full := int(frac * float64(w))
	return sAccent.Render(strings.Repeat("━", full)) + sDim.Render(strings.Repeat("━", w-full))
}
