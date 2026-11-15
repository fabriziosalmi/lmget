package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/fabriziosalmi/lmget"
)

type Mode int

const (
	ModeTTY Mode = iota
	ModePlain
	ModeJSON
)

// Run executes a pull and renders it. It returns the process exit code.
// resume is the command line to suggest after an interruption.
func Run(ctx context.Context, mode Mode, c *lmget.Client, req lmget.Request, stdout, stderr io.Writer, resume string) lmget.ExitCode {
	switch mode {
	case ModeJSON:
		return runJSON(ctx, c, req, stdout)
	case ModePlain:
		return runPlain(ctx, c, req, stdout, stderr, resume)
	}
	return runTTY(ctx, c, req, stdout, resume)
}

func withResume(e *lmget.Error, resume string) *lmget.Error {
	if e.Code == lmget.ExitInterrupted && resume != "" {
		e.Fixes = []lmget.Fix{{Cmd: resume, Note: "resumes where it stopped"}}
	}
	return e
}

// ---------------------------------------------------------------- json

func runJSON(ctx context.Context, c *lmget.Client, req lmget.Request, w io.Writer) lmget.ExitCode {
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	last := map[string]time.Time{}
	emit := func(e lmget.Event) {
		mu.Lock()
		defer mu.Unlock()
		if e.Type == lmget.EvProgress {
			if time.Since(last[e.File]) < 250*time.Millisecond {
				return
			}
			last[e.File] = time.Now()
		}
		enc.Encode(e)
	}
	if _, err := c.Pull(ctx, req, emit); err != nil {
		le := lmget.AsError(err)
		emit(lmget.Event{V: lmget.SchemaVersion, Type: lmget.EvError, Time: time.Now().UTC(), Error: le})
		return le.Code
	}
	return lmget.ExitOK
}

// ---------------------------------------------------------------- plain

// runPlain writes stable log lines to stderr and the result path to stdout,
// so `dir=$(lmget Org/Name)` works in scripts.
func runPlain(ctx context.Context, c *lmget.Client, req lmget.Request, stdout, stderr io.Writer, resume string) lmget.ExitCode {
	logf := func(format string, a ...any) { fmt.Fprintf(stderr, "lmget: "+format+"\n", a...) }
	nextPct := map[string]int64{}
	t0 := time.Now()
	emit := func(e lmget.Event) {
		switch e.Type {
		case lmget.EvResolve:
			logf("resolve %s", e.Ref)
		case lmget.EvPlan:
			p := e.Plan
			logf("plan %s@%s license=%s files=%d bytes=%d to_fetch=%d", p.Ref.Display(), p.Commit, orNone(p.License), len(p.Selected()), p.Bytes, p.ToFetch)
			skipped := map[string]int{}
			var reasons []string
			for _, f := range p.Files {
				if !f.Selected() {
					if skipped[f.Skipped] == 0 {
						reasons = append(reasons, f.Skipped)
					}
					skipped[f.Skipped]++
				}
			}
			for _, r := range reasons {
				logf("skip %d files (%s)", skipped[r], r)
			}
			if len(p.Pickle) > 0 {
				logf("warning: %d pickle files selected: %s", len(p.Pickle), strings.Join(p.Pickle, ", "))
			}
			if req.DryRun {
				for _, f := range p.Selected() {
					h := f.SHA256
					if h == "" {
						h = "git:" + f.GitSHA1
					}
					fmt.Fprintf(stdout, "%d\t%s\t%s\n", f.Size, h, f.Path)
				}
			}
		case lmget.EvFileStart:
			t0 = time.Now()
			if e.Total >= 100<<20 {
				nextPct[e.File] = 10
			}
		case lmget.EvProgress:
			if n, ok := nextPct[e.File]; ok && e.Total > 0 && e.Bytes*100/e.Total >= n {
				rate := float64(e.Bytes) / time.Since(t0).Seconds()
				logf("progress %s %d%% %s/%s %s/s", e.File, e.Bytes*100/e.Total, hb(e.Bytes), hb(e.Total), hb(int64(rate)))
				nextPct[e.File] = (e.Bytes*100/e.Total/10 + 1) * 10
			}
		case lmget.EvFileDone:
			if e.Cached {
				logf("cached %s sha256=%s", e.File, e.SHA256)
			} else {
				logf("verified %s sha256=%s", e.File, e.SHA256)
			}
		case lmget.EvExport:
			logf("export %s %s", e.Target, e.Path)
		case lmget.EvDone:
			logf("done bytes=%d fetched=%d elapsed=%.1fs", e.Total, e.Fetched, e.Elapsed)
		}
	}
	out, err := c.Pull(ctx, req, emit)
	if err != nil {
		le := withResume(lmget.AsError(err), resume)
		logf("error: %s", le.Msg)
		if le.Why != "" {
			logf("  %s", le.Why)
		}
		for _, f := range le.Fixes {
			logf("  fix: %s", strings.TrimSpace(f.Cmd+"  "+f.Note))
		}
		return le.Code
	}
	if out.Path != "" {
		fmt.Fprintln(stdout, out.Path)
	}
	return lmget.ExitOK
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// ---------------------------------------------------------------- tty

type eventMsg lmget.Event
type finishedMsg struct {
	out *lmget.Outcome
	err error
}
type tickMsg time.Time

type model struct {
	req     lmget.Request
	store   string
	cancel  context.CancelFunc
	resume  string
	width   int
	frame   int
	plan    *lmget.Plan
	file    string
	fBytes  int64
	fTotal  int64
	doneB   int64 // bytes of files finished in this run (fetched, not cached)
	nDone   int
	rate    float64
	lastB   int64
	lastT   time.Time
	stopped bool
	final   bool
	code    lmget.ExitCode
	pending []string // lines to print above the live view, flushed on tick only
	nameW   int      // column width for file names, from the plan
}

// flush prints queued lines. All printing goes through here, from Update, so
// output order is the event order; the last flush is one Sequence with Quit.
func (m *model) flush() tea.Cmd {
	if len(m.pending) == 0 {
		if m.final {
			return tea.Quit
		}
		return nil
	}
	p := tea.Println(strings.Join(m.pending, "\n"))
	m.pending = nil
	if m.final {
		return tea.Sequence(p, tea.Quit)
	}
	return p
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func tick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) Init() tea.Cmd { return tick() }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" && !m.stopped {
			m.stopped = true
			m.cancel()
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tickMsg:
		m.frame++
		now := time.Time(msg)
		total := m.doneB + m.fBytes
		if m.lastT.IsZero() {
			m.lastT, m.lastB = now, total
		} else if dt := now.Sub(m.lastT).Seconds(); dt >= 0.5 {
			inst := float64(total-m.lastB) / dt
			if m.rate == 0 {
				m.rate = inst
			} else {
				m.rate = 0.3*inst + 0.7*m.rate
			}
			m.lastT, m.lastB = now, total
		}
		if m.final {
			return m, m.flush()
		}
		return m, tea.Batch(m.flush(), tick())
	case eventMsg:
		e := lmget.Event(msg)
		switch e.Type {
		case lmget.EvPlan:
			m.plan = e.Plan
			for _, f := range e.Plan.Selected() {
				m.nameW = max(m.nameW, min(32, lipgloss.Width(f.Path)))
			}
			if !m.req.DryRun { // dry runs print the full plan at the end
				m.pending = append(m.pending, strings.TrimRight(renderPlan(e.Plan, m.width, false), "\n")+"\n")
			}
		case lmget.EvFileStart:
			m.file, m.fBytes, m.fTotal = e.File, 0, e.Total
		case lmget.EvProgress:
			m.fBytes = e.Bytes
		case lmget.EvFileDone:
			m.nDone++
			if !e.Cached {
				m.doneB += e.Total
				m.fBytes, m.file = 0, ""
				m.pending = append(m.pending, fmt.Sprintf("  %s %s  %s  %s", sAccent.Render(check), pad(middleTrunc(e.File, 32), m.nameW), padLeft(hb(e.Total), 8), sDim.Render("sha256 "+e.SHA256[:8]+".."+e.SHA256[60:])))
			}
		}
	case finishedMsg:
		m.final = true // the next tick flushes and quits
		switch {
		case msg.err != nil:
			le := withResume(lmget.AsError(msg.err), m.resume)
			m.code = le.Code
			m.pending = append(m.pending, strings.TrimRight(renderError(le), "\n")+"\n")
		case m.req.DryRun:
			m.pending = append(m.pending, strings.TrimRight(renderPlan(msg.out.Plan, m.width, true), "\n")+"\n")
		default:
			m.pending = append(m.pending, strings.TrimRight(renderSummary(msg.out, m.req.Target, m.store), "\n")+"\n")
		}
	}
	return m, nil
}

func (m *model) View() tea.View {
	if m.final {
		return tea.NewView("")
	}
	sp := sAccent.Render(spinner[m.frame%len(spinner)])
	if m.plan == nil {
		return tea.NewView(fmt.Sprintf("  %s resolving %s", sp, m.req.Ref.Display()))
	}
	if m.req.DryRun {
		return tea.NewView("")
	}
	var b strings.Builder
	if m.file != "" && m.fTotal > 0 {
		frac := float64(m.fBytes) / float64(m.fTotal)
		pre := fmt.Sprintf("  %s %s  ", sp, pad(middleTrunc(m.file, 32), m.nameW))
		post := fmt.Sprintf("  %3.0f%%  %s / %s", frac*100, hb(m.fBytes), hb(m.fTotal))
		w := 30
		if m.width > 0 { // fit the line: a wrapped line breaks the live redraw
			w = max(8, min(40, m.width-lipgloss.Width(pre)-lipgloss.Width(post)-1))
		}
		fmt.Fprintf(&b, "%s%s%s\n", pre, bar(frac, w), post)
	} else {
		fmt.Fprintf(&b, "  %s\n", sp)
	}
	sel := len(m.plan.Selected())
	have := m.doneB + m.fBytes
	eta := ""
	if m.rate > 0 && m.plan.ToFetch > have {
		eta = "  eta " + fmtDur(time.Duration(float64(m.plan.ToFetch-have)/m.rate*float64(time.Second)))
	}
	fmt.Fprintf(&b, "    %s", sDim.Render(fmt.Sprintf("%d/%d files  %s / %s  %s/s%s", m.nDone, sel, hb(have), hb(m.plan.ToFetch), hb(int64(m.rate)), eta)))
	if m.stopped {
		fmt.Fprintf(&b, "\n    %s", sWarn.Render("stopping, verified progress is kept"))
	}
	return tea.NewView(b.String())
}

func runTTY(ctx context.Context, c *lmget.Client, req lmget.Request, out io.Writer, resume string) lmget.ExitCode {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := &model{req: req, store: c.Store().Root(), cancel: cancel, resume: resume}
	p := tea.NewProgram(m, tea.WithOutput(out), tea.WithContext(context.Background()))
	go func() {
		o, err := c.Pull(ctx, req, func(e lmget.Event) { p.Send(eventMsg(e)) })
		p.Send(finishedMsg{out: o, err: err})
	}()
	if _, err := p.Run(); err != nil {
		cancel()
		return lmget.ExitFailure
	}
	return m.code
}
