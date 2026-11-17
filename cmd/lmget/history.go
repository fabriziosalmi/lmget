package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fabriziosalmi/lmget"
	"github.com/fabriziosalmi/lmget/internal/ui"
)

// recordPull writes one pull to the history: what was asked, what came, from
// where, how fast, and how it ended.
func recordPull(c *lmget.Client, command string, req lmget.Request, out *lmget.Outcome, code lmget.ExitCode, nick *nickname) {
	if req.DryRun {
		return // a look, not a use
	}
	e := lmget.HistoryEntry{Command: command, Ref: req.Ref.String(), Target: req.Target.Kind, Exit: int(code)}
	if nick != nil {
		e.Nickname, e.Choice = nick.name, nick.choice
	}
	if out != nil && out.Plan != nil {
		e.Commit, e.Bytes, e.Fetched = out.Plan.Commit, out.Plan.Bytes, out.Fetched
		e.ElapsedMS = out.Elapsed.Milliseconds()
		e.From = lmget.FromBytes(out.Plan)
	}
	c.AppendHistory(e)
}

// preferFasterNodes orders the index nodes by the speed the history
// measured, so the next fallback asks the fastest one first.
func preferFasterNodes(c *lmget.Client) {
	h, _ := c.History()
	speed := lmget.NodeSpeeds(h)
	if len(speed) == 0 {
		return
	}
	hosts := make([]string, 0, len(speed))
	for host := range speed {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	c.PreferNodes(lmget.FasterFirst(hosts, speed))
}

// hintState is what lmget remembers about its hints, next to the history.
type hintState struct {
	Shown int       `json:"shown"`
	Last  time.Time `json:"last"`
}

const (
	staleAge     = 30 * 24 * time.Hour
	staleMin     = 1_000_000_000 // 1 GB: below it, not worth a line
	hintEvery    = 24 * time.Hour
	hintMaxShown = 3 // shown three times and ignored (no rm since): not shown again
)

// staleHint prints, at most once a day, one line about models nobody pulled
// for 30 days. Three showings with no lmget rm in between, and it stops:
// a hint that is ignored is noise.
func staleHint(c *lmget.Client, mode ui.Mode) {
	if mode == ui.ModeJSON || !lmget.HistoryOn() {
		return
	}
	statePath := filepath.Join(filepath.Dir(c.HistoryPath()), "hints.json")
	var st map[string]hintState
	if b, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(b, &st)
	}
	if st == nil {
		st = map[string]hintState{}
	}
	s := st["stale"]
	h, _ := c.History()
	for _, e := range h { // an rm after the last showing: the hint was heard
		if e.Command == "rm" && e.Time.After(s.Last) {
			s.Shown = 0
		}
	}
	if s.Shown >= hintMaxShown || time.Since(s.Last) < hintEvery {
		return
	}
	models, err := c.List()
	if err != nil {
		return
	}
	var n int
	var bytes int64
	var oldest string
	var oldestAt time.Time
	for _, m := range models {
		if time.Since(m.Updated) < staleAge {
			continue
		}
		n++
		bytes += m.Bytes
		if oldest == "" || m.Updated.Before(oldestAt) {
			oldest, oldestAt = m.Ref.Display(), m.Updated
		}
	}
	if bytes < staleMin {
		return
	}
	what := fmt.Sprintf("%d models (%s) were", n, lmget.HumanBytes(bytes))
	if n == 1 {
		what = fmt.Sprintf("1 model (%s) was", lmget.HumanBytes(bytes))
	}
	fmt.Fprintf(os.Stderr, "lmget: hint: %s last pulled more than 30 days ago, the oldest %s on %s: lmget ls, then lmget rm <model>\n",
		what, lmget.Printable(oldest), oldestAt.Format(time.DateOnly))
	s.Shown++
	s.Last = time.Now()
	st["stale"] = s
	if b, err := json.Marshal(st); err == nil {
		os.WriteFile(statePath, b, 0o600)
	}
}

func doHistory(f flags, args []string) lmget.ExitCode {
	mode := outputMode(f)
	c, err := lmget.New(lmget.Options{})
	if err != nil {
		return fail(mode, err)
	}
	if len(args) == 1 && args[0] == "clear" {
		if err := c.ClearHistory(); err != nil {
			return fail(mode, err)
		}
		os.Remove(filepath.Join(filepath.Dir(c.HistoryPath()), "hints.json"))
		fmt.Fprintln(os.Stderr, "lmget: history cleared")
		return lmget.ExitOK
	}
	if len(args) > 0 {
		return fail(mode, &lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: "lmget history takes no argument but clear",
			Fixes: []lmget.Fix{{Cmd: "lmget history"}, {Cmd: "lmget history clear"}}})
	}
	if !lmget.HistoryOn() {
		fmt.Fprintln(os.Stderr, "lmget: the history is off (LMGET_HISTORY=off): nothing is recorded or read")
		return lmget.ExitOK
	}
	h, bad := c.History()
	if mode == ui.ModeJSON {
		b, _ := json.Marshal(map[string]any{"v": lmget.SchemaVersion, "type": "history", "time": time.Now().UTC(),
			"path": c.HistoryPath(), "entries": h, "unreadable": bad})
		fmt.Fprintln(os.Stdout, string(b))
		return lmget.ExitOK
	}
	start := 0
	if len(h) > 20 {
		start = len(h) - 20
	}
	for _, e := range h[start:] {
		what := strings.TrimPrefix(e.Ref, "hf:")
		if e.Nickname != "" {
			what = e.Nickname + " -> " + what
			if e.Choice != nil && e.Choice.Answer == "no" {
				what = e.Nickname + " (declined " + strings.TrimPrefix(e.Choice.Variant, "hf:") + ")"
			}
		}
		result := "ok"
		if e.Exit != 0 {
			result = fmt.Sprintf("exit %d", e.Exit)
		}
		detail := ""
		if e.Fetched > 0 && e.ElapsedMS > 0 {
			detail = fmt.Sprintf("%s in %.1fs", lmget.HumanBytes(e.Fetched), float64(e.ElapsedMS)/1000)
			for from := range e.From {
				if from != "source" {
					detail += " from " + from
				}
			}
		}
		fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%s\t%s\n", e.Time.Local().Format("2006-01-02 15:04"), e.Command, lmget.Printable(what), result, detail)
	}
	speeds := lmget.NodeSpeeds(h)
	for host, bps := range speeds {
		fmt.Fprintf(os.Stderr, "lmget: node %s measured at %s/s (median of the pulls served by it alone)\n", host, lmget.HumanBytes(int64(bps)))
	}
	fmt.Fprintf(os.Stderr, "lmget: %d entries in %s", len(h), c.HistoryPath())
	if bad > 0 {
		fmt.Fprintf(os.Stderr, ", %d unreadable lines skipped", bad)
	}
	fmt.Fprintln(os.Stderr, "; LMGET_HISTORY=off turns it off, lmget history clear deletes it")
	return lmget.ExitOK
}
