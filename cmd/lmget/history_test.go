package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fabriziosalmi/lmget"
)

func testClient(t *testing.T) *lmget.Client {
	t.Helper()
	t.Setenv("LMGET_HISTORY", "")
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HF_TOKEN_PATH", filepath.Join(t.TempDir(), "none")) // never the user's token
	c, err := lmget.New(lmget.Options{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Found by the 2026-11-18 review: the history kept a signed URL whole.
func TestRecordPullKeepsNoURLCredentials(t *testing.T) {
	c := testClient(t)
	ref, err := lmget.ParseRef("https://example.org/m.gguf?X-Amz-Signature=SECRET&X-Amz-Credential=AKIA")
	if err != nil {
		t.Fatal(err)
	}
	recordPull(c, "get", lmget.Request{Ref: ref, Target: lmget.Target{Kind: "dir"}}, nil, lmget.ExitOK, nil)
	b, err := os.ReadFile(c.HistoryPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "AKIA") {
		t.Fatalf("the history keeps the credential: %s", b)
	}
	if !strings.Contains(string(b), "X-Amz-Signature=REDACTED") {
		t.Fatalf("the redacted URL is not recorded: %s", b)
	}
}

func TestRecordPullSkipsDryRuns(t *testing.T) {
	c := testClient(t)
	ref, _ := lmget.ParseRef("org/m")
	recordPull(c, "get", lmget.Request{Ref: ref, DryRun: true}, nil, lmget.ExitOK, nil)
	if h, _ := c.History(); len(h) != 0 {
		t.Fatalf("a dry run was recorded: %+v", h)
	}
}

// The memory ceiling: just below the smallest variant that failed lately,
// and nothing from a failure older than 7 days or of another nickname.
func TestMemoryCeiling(t *testing.T) {
	a := lmget.Alias{Kind: "chat", Family: "F", Variants: []lmget.AliasVariant{
		{Ref: "hf:o/small", Bytes: 1e9}, {Ref: "hf:o/mid", Bytes: 3e9}, {Ref: "hf:o/big", Bytes: 6e9},
	}}
	now := time.Now()
	run := func(name, variant string, ago time.Duration) lmget.HistoryEntry {
		return lmget.HistoryEntry{Time: now.Add(-ago), Command: "run", Nickname: name, Error: "memory",
			Choice: &lmget.HistoryChoice{Variant: variant}}
	}
	h := []lmget.HistoryEntry{run("q", "hf:o/big", time.Hour), run("q", "hf:o/mid", 2*time.Hour),
		run("q", "hf:o/small", 9*24*time.Hour), run("other", "hf:o/small", time.Hour)}
	got, why := memoryCeiling(h, "q", a, now)
	if got != 3e9-1 || !strings.Contains(why, "o/mid") {
		t.Fatalf("ceiling %d (%s), want just below o/mid", got, why)
	}
	if got, _ := memoryCeiling(nil, "q", a, now); got != 0 {
		t.Fatal("a ceiling with no history")
	}
}

func TestSizeFlagRoundsUp(t *testing.T) {
	for in, want := range map[int64]string{1: "1GB", 1e9: "1GB", 1e9 + 1: "2GB", 5.98e9: "6GB"} {
		if got := sizeFlag(in); got != want {
			t.Errorf("%d: %s, want %s", in, got, want)
		}
	}
}

func TestConfirm(t *testing.T) {
	for in, want := range map[string]bool{"\n": true, "y\n": true, "YES\n": true, "n\n": false, "no\n": false, "maybe\n": false} {
		if got := confirm(strings.NewReader(in), &strings.Builder{}); got != want {
			t.Errorf("%q: %v", in, got)
		}
	}
}
