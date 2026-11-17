package lmget

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func historyClient(t *testing.T) *Client {
	t.Helper()
	t.Setenv("LMGET_HISTORY", "")
	c, err := New(Options{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHistoryRoundTripSkipsBadLinesAndRotates(t *testing.T) {
	c := historyClient(t)
	c.AppendHistory(HistoryEntry{Command: "get", Ref: "hf:org/a", Exit: 0})
	c.AppendHistory(HistoryEntry{Command: "rm", Ref: "hf:org/a"})
	f, _ := os.OpenFile(c.HistoryPath(), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("{not json\n{\"time\":\"2026-11-17T00:00:00Z\"}\n") // a torn write, an entry with no command
	f.Close()
	h, bad := c.History()
	if len(h) != 2 || bad != 2 || h[0].Ref != "hf:org/a" || h[1].Command != "rm" || h[0].Version == "" {
		t.Fatalf("entries %+v, unreadable %d", h, bad)
	}
	if fi, _ := os.Stat(c.HistoryPath()); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("history mode %v, want 0600", fi.Mode().Perm())
	}

	old := historyMaxBytes
	historyMaxBytes = 2000
	defer func() { historyMaxBytes = old }()
	for i := range 60 {
		c.AppendHistory(HistoryEntry{Command: "get", Ref: fmt.Sprintf("hf:org/m%d", i)})
	}
	if _, err := os.Stat(c.HistoryPath() + ".1"); err != nil {
		t.Fatal("the history did not rotate")
	}
	if fi, _ := os.Stat(c.HistoryPath()); fi.Size() > historyMaxBytes {
		t.Fatalf("history.jsonl is %d bytes, over the cap", fi.Size())
	}
	h, _ = c.History()
	if h[len(h)-1].Ref != "hf:org/m59" {
		t.Fatal("the newest entry is not last")
	}
	if err := c.ClearHistory(); err != nil {
		t.Fatal(err)
	}
	if h, _ := c.History(); len(h) != 0 {
		t.Fatal("clear left entries")
	}
}

func TestHistoryOff(t *testing.T) {
	c := historyClient(t)
	c.AppendHistory(HistoryEntry{Command: "get", Ref: "hf:org/a"})
	t.Setenv("LMGET_HISTORY", "off")
	c.AppendHistory(HistoryEntry{Command: "get", Ref: "hf:org/b"})
	if h, _ := c.History(); h != nil {
		t.Fatal("history read while off")
	}
	t.Setenv("LMGET_HISTORY", "")
	if h, _ := c.History(); len(h) != 1 {
		t.Fatalf("an entry was written while off: %+v", h)
	}
}

// Two lmget at once: every line stays whole.
func TestHistoryConcurrentAppends(t *testing.T) {
	c := historyClient(t)
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.AppendHistory(HistoryEntry{Command: "get", Ref: "hf:org/" + strings.Repeat("x", 200+i)})
		}()
	}
	wg.Wait()
	if h, bad := c.History(); len(h) != 40 || bad != 0 {
		t.Fatalf("%d entries, %d unreadable", len(h), bad)
	}
}

func TestNodeSpeedsAndOrder(t *testing.T) {
	at := func(from string, mb, ms int64, exit int) HistoryEntry {
		return HistoryEntry{Time: time.Now(), Command: "get", Fetched: mb << 20, ElapsedMS: ms, Exit: exit, From: map[string]int64{from: mb << 20}}
	}
	h := []HistoryEntry{
		at("node:node1.lmget.org", 600, 80_000, 0),  // 7.9 MB/s
		at("node:node1.lmget.org", 600, 100_000, 0), // 6.3
		at("node:node1.lmget.org", 600, 90_000, 0),  // 7.0: the median
		at("node:node2.lmget.org", 600, 300_000, 0), // 2.1
		at("node:node2.lmget.org", 10, 100, 0),      // too small to count
		at("node:node2.lmget.org", 600, 1_000, 6),   // failed: does not count
		at("source", 600, 10_000, 0),                // not a node
	}
	mixed := at("node:node2.lmget.org", 600, 1_000, 0)
	mixed.From["source"] = 1
	h = append(h, mixed) // bytes from two origins: says nothing about one node
	sp := NodeSpeeds(h)
	if len(sp) != 2 || sp["node2.lmget.org"] > 3e6 || sp["node1.lmget.org"] < 6.5e6 || sp["node1.lmget.org"] > 7.5e6 {
		t.Fatalf("speeds %v", sp)
	}
	order := FasterFirst([]string{"node2.lmget.org", "never.example", "node1.lmget.org"}, sp)
	if strings.Join(order, ",") != "node1.lmget.org,node2.lmget.org,never.example" {
		t.Fatalf("order %v", order)
	}
}

func TestLastNicknameChoice(t *testing.T) {
	e := func(answer string, forB int64, format string, exit int) HistoryEntry {
		return HistoryEntry{Time: time.Now(), Command: "get", Nickname: "qwen", Exit: exit,
			Choice: &HistoryChoice{Variant: "hf:x/y", For: forB, Format: format, Answer: answer}}
	}
	if c := LastNicknameChoice([]HistoryEntry{e("yes", 4e9, "", 0)}, "qwen"); c == nil || c.Choice.For != 4e9 {
		t.Fatal("a steered choice is not remembered")
	}
	if c := LastNicknameChoice([]HistoryEntry{e("yes", 4e9, "", 0), e("flag", 0, "", 0)}, "qwen"); c != nil {
		t.Fatal("a later choice with the defaults did not reset the memory")
	}
	if c := LastNicknameChoice([]HistoryEntry{e("yes", 4e9, "", 0), e("no", 1e9, "", 2)}, "qwen"); c == nil || c.Choice.For != 4e9 {
		t.Fatal("a declined proposal changed the memory")
	}
	// "no" alone, whatever the exit code recorded with it
	if c := LastNicknameChoice([]HistoryEntry{e("yes", 4e9, "", 0), e("no", 1e9, "", 0)}, "qwen"); c == nil || c.Choice.For != 4e9 {
		t.Fatal("a declined proposal is remembered")
	}
	if c := LastNicknameChoice([]HistoryEntry{e("yes", 4e9, "gguf", 6)}, "qwen"); c != nil {
		t.Fatal("a failed pull is remembered")
	}
	if c := LastNicknameChoice([]HistoryEntry{e("yes", 4e9, "", 0)}, "gemma"); c != nil {
		t.Fatal("another nickname's choice leaked")
	}
}

// The history's measured speeds decide which node is asked first.
func TestNodePreferenceOrdersTheNodes(t *testing.T) {
	m := setupM3(t, "https://slow.example")
	u := m.client(t, t.TempDir())
	if _, err := u.UpdateIndex(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := File{Path: m.files[0].path, Size: int64(len(m.files[0].data)), SHA256: sha(m.files[0].data)}
	commit := m.lock.Models[0].Commit
	first := func() string { return u.nodeFiles(hfRef("org/m"), commit, f)[0].From }
	if first() != NodeOrigin+"slow.example" {
		t.Fatalf("without a preference the index order is kept: %s", first())
	}
	u.PreferNodes([]string{hostOf(m.node.URL)})
	if first() != NodeOrigin+hostOf(m.node.URL) {
		t.Fatalf("the preferred node is not asked first: %s", first())
	}
}
