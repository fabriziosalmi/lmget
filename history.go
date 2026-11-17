package lmget

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The history is a local record of what lmget did here, so it can advise
// and correct itself: which node was fast, which variant the user chose for
// a nickname. It stays on this machine (lmget sends it nowhere), holds no
// token, no prompt and no path outside the store, and LMGET_HISTORY=off
// turns it off: nothing is written or read then.

// HistoryEntry is one command, as one line of history.jsonl.
type HistoryEntry struct {
	Time      time.Time        `json:"time"`
	Version   string           `json:"version"`
	Command   string           `json:"command"` // get, sync, rm
	Ref       string           `json:"ref,omitempty"`
	Commit    string           `json:"commit,omitempty"`
	Nickname  string           `json:"nickname,omitempty"`
	Choice    *HistoryChoice   `json:"choice,omitempty"`
	Target    string           `json:"target,omitempty"` // the kind only, never a path
	Bytes     int64            `json:"bytes,omitempty"`
	Fetched   int64            `json:"fetched,omitempty"`
	ElapsedMS int64            `json:"elapsed_ms,omitempty"`
	From      map[string]int64 `json:"from,omitempty"` // bytes fetched, by origin: source, modelscope, node:<host>, origin:<url>
	Exit      int              `json:"exit"`
	Error     string           `json:"error,omitempty"` // the error kind
}

// HistoryChoice is what a nickname resolved to and how the user took it.
type HistoryChoice struct {
	Variant string `json:"variant"`
	Format  string `json:"format,omitempty"` // given with --format
	For     int64  `json:"for,omitempty"`    // given with --for
	Answer  string `json:"answer"`           // yes (asked), flag (--yes), no
}

var historyMaxBytes int64 = 1 << 20 // then history.jsonl becomes history.jsonl.1

// HistoryOn reports whether the history is kept: LMGET_HISTORY=off stops it.
func HistoryOn() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LMGET_HISTORY")))
	return v != "off" && v != "0" && v != "false" && v != "no"
}

func (c *Client) historyPath() string { return filepath.Join(c.store.Root(), "history.jsonl") }

// HistoryPath is where the history is kept.
func (c *Client) HistoryPath() string { return c.historyPath() }

// AppendHistory adds one entry. It never fails a command: the history is a
// convenience, so a write that fails is dropped.
func (c *Client) AppendHistory(e HistoryEntry) {
	if !HistoryOn() {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if e.Version == "" {
		e.Version = Version
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	unlock, err := c.store.flock("history.lock", true, true)
	if err != nil {
		return
	}
	defer unlock()
	p := c.historyPath()
	if fi, err := os.Stat(p); err == nil && fi.Size()+int64(len(line)) > historyMaxBytes {
		os.Rename(p, p+".1") // one older file is kept; the one before it goes
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(line, '\n')) // one write per line: two lmget at once never interleave inside one
}

// History returns the entries, oldest first, and how many lines could not be
// read (a crash mid-write, a hand edit): those are skipped, never fatal.
func (c *Client) History() ([]HistoryEntry, int) {
	if !HistoryOn() {
		return nil, 0
	}
	var out []HistoryEntry
	bad := 0
	for _, p := range []string{c.historyPath() + ".1", c.historyPath()} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if len(bytes.TrimSpace(sc.Bytes())) == 0 {
				continue
			}
			var e HistoryEntry
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Time.IsZero() || e.Command == "" {
				bad++
				continue
			}
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, bad
}

// ClearHistory deletes the history.
func (c *Client) ClearHistory() error {
	unlock, err := c.store.flock("history.lock", true, true)
	if err != nil {
		return diskErr(err)
	}
	defer unlock()
	for _, p := range []string{c.historyPath(), c.historyPath() + ".1"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return diskErr(err)
		}
	}
	return nil
}

// FromBytes sums the bytes a pull fetched, by origin. Files already in the
// store before the pull count for nothing.
func FromBytes(p *Plan) map[string]int64 {
	out := map[string]int64{}
	for _, f := range p.Selected() {
		if f.Cached {
			continue
		}
		from := f.From
		if from == "" {
			from = "source"
		}
		out[from] += f.Size
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// minSpeedSample is the smallest pull whose speed says something about a
// node: below it, the time is mostly round trips.
const minSpeedSample = 50 << 20

// NodeSpeeds returns the median speed (bytes per second) measured from each
// index node, over pulls whose fetched bytes all came from that one node.
func NodeSpeeds(h []HistoryEntry) map[string]float64 {
	samples := map[string][]float64{}
	for _, e := range h {
		if e.Exit != 0 || e.ElapsedMS <= 0 || e.Fetched < minSpeedSample || len(e.From) != 1 {
			continue
		}
		for from, n := range e.From {
			if strings.HasPrefix(from, NodeOrigin) && n == e.Fetched {
				samples[strings.TrimPrefix(from, NodeOrigin)] = append(samples[strings.TrimPrefix(from, NodeOrigin)], float64(n)/(float64(e.ElapsedMS)/1000))
			}
		}
	}
	out := map[string]float64{}
	for host, s := range samples {
		sort.Float64s(s)
		out[host] = s[len(s)/2]
	}
	return out
}

// FasterFirst orders hosts by measured speed, fastest first; hosts never
// measured keep their order, after the measured ones.
func FasterFirst(hosts []string, speed map[string]float64) []string {
	out := append([]string(nil), hosts...)
	sort.SliceStable(out, func(i, j int) bool {
		si, iok := speed[out[i]]
		sj, jok := speed[out[j]]
		switch {
		case iok && jok:
			return si > sj
		default:
			return iok && !jok
		}
	})
	return out
}

// LastNicknameChoice returns the most recent accepted pull of a nickname
// that the user steered with --for or --format, or nil when the latest
// accepted one took the defaults (or there is none).
func LastNicknameChoice(h []HistoryEntry, name string) *HistoryEntry {
	for i := len(h) - 1; i >= 0; i-- {
		e := h[i]
		if e.Nickname != name || e.Choice == nil || e.Choice.Answer == "no" || e.Exit != 0 {
			continue
		}
		if e.Choice.For > 0 || e.Choice.Format != "" {
			return &e
		}
		return nil
	}
	return nil
}

// PreferNodes sets the order in which index nodes are asked (see
// Options.NodePreference).
func (c *Client) PreferNodes(hosts []string) { c.opt.NodePreference = hosts }
