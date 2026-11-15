package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fabriziosalmi/lmget"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func samplePlan() *lmget.Plan {
	return &lmget.Plan{
		Ref: lmget.Ref{Source: "hf", Repo: "org/m", Revision: "main"}, Commit: commit, License: "apache-2.0",
		Files: []lmget.File{
			{Path: "model.safetensors", Size: 3_000_000, SHA256: strings.Repeat("a", 64)},
			{Path: "config.json", Size: 100, GitSHA1: strings.Repeat("b", 40)},
			{Path: "pytorch_model.bin", Size: 3_000_000, SHA256: strings.Repeat("c", 64), Skipped: "pickle, safetensors available"},
		},
		Bytes: 3_000_100, ToFetch: 3_000_100, DiskFree: 1 << 40,
	}
}

func ev(t lmget.EventType) lmget.Event { return lmget.Event{V: 1, Type: t, Time: time.Now()} }

// The live model receives the event stream in order and queues lines in the
// same order; the final flush is the only place that quits.
func TestModelQueuesOutputInEventOrder(t *testing.T) {
	m := &model{req: lmget.Request{Ref: lmget.Ref{Source: "hf", Repo: "org/m", Revision: "main"}, Target: lmget.Target{Kind: "hf"}},
		cancel: func() {}, width: 100}
	p := samplePlan()
	e := ev(lmget.EvPlan)
	e.Plan = p
	m.Update(eventMsg(e))
	e = ev(lmget.EvFileStart)
	e.File, e.Total = "model.safetensors", 3_000_000
	m.Update(eventMsg(e))
	e = ev(lmget.EvProgress)
	e.File, e.Bytes, e.Total = "model.safetensors", 1_500_000, 3_000_000
	m.Update(eventMsg(e))
	if v := m.View().Content; !strings.Contains(v, "50%") || !strings.Contains(v, "model.safetensors") {
		t.Fatalf("live view during download:\n%s", v)
	}
	e = ev(lmget.EvFileDone)
	e.File, e.SHA256, e.Total = "model.safetensors", strings.Repeat("a", 64), 3_000_000
	m.Update(eventMsg(e))
	m.Update(finishedMsg{out: &lmget.Outcome{Plan: p, Path: "/x/snapshots/" + commit, Fetched: 3_000_100, Elapsed: 2 * time.Second}, lock: "lmget.lock"})
	if !m.final || m.View().Content != "" {
		t.Fatal("after finishing, the live view must be empty")
	}
	out := ansi.ReplaceAllString(strings.Join(m.pending, "\n"), "")
	order := []string{"org/m", "checks", "skipped", "✓ model.safetensors", "2/2 verified", "hf cache", "lock"}
	at := 0
	for _, want := range order {
		i := strings.Index(out[at:], want)
		if i < 0 {
			t.Fatalf("%q missing or out of order in:\n%s", want, out)
		}
		at += i
	}
	if cmd := m.flush(); cmd == nil {
		t.Fatal("the final flush must print and quit")
	}
}

func TestModelErrorShowsWhyCauseAndResume(t *testing.T) {
	m := &model{req: lmget.Request{Ref: lmget.Ref{Source: "hf", Repo: "org/m", Revision: "main"}}, cancel: func() {}, resume: "lmget org/m"}
	err := &lmget.Error{Code: lmget.ExitDisk, Kind: "disk", Msg: "cannot write to disk", Cause: "write /x: no space left on device"}
	m.Update(finishedMsg{err: err})
	out := strings.Join(m.pending, "\n")
	if m.code != lmget.ExitDisk || !strings.Contains(out, "no space left on device") {
		t.Fatalf("code %d, output:\n%s", m.code, out)
	}
	m2 := &model{req: m.req, cancel: func() {}, resume: "lmget org/m"}
	m2.Update(finishedMsg{err: context.Canceled})
	if out := strings.Join(m2.pending, "\n"); m2.code != lmget.ExitInterrupted || !strings.Contains(out, "lmget org/m") {
		t.Fatalf("interrupt: code %d, output:\n%s", m2.code, out)
	}
}

func TestRenderPlanForAURLHasNoCommitAndNamesTheHashSource(t *testing.T) {
	p := &lmget.Plan{Ref: lmget.Ref{Source: "url", Repo: "https://example.org/m.gguf"}, Commit: commit,
		HashFrom: lmget.HashFromFirst, Files: []lmget.File{{Path: "m.gguf", Size: 10}}, Bytes: 10, DiskFree: -1}
	out := renderPlan(p, 100, false)
	if strings.Contains(out, commit[:7]) || !strings.Contains(out, "pinned on first download") || !strings.Contains(out, "license unknown") {
		t.Fatalf("URL plan:\n%s", out)
	}
}

// The JSON writer for list/doctor emits one object with the schema version.
func TestWriteJSONCarriesSchemaVersion(t *testing.T) {
	var b bytes.Buffer
	Doctor(ModeJSON, &b, []lmget.Check{{Name: "store", Status: "fail", Detail: "x"}})
	var got map[string]any
	if err := json.Unmarshal(b.Bytes(), &got); err != nil || got["v"] != float64(lmget.SchemaVersion) || got["type"] != "doctor" {
		t.Fatalf("got %s (%v)", b.String(), err)
	}
	if code := Doctor(ModePlain, &bytes.Buffer{}, []lmget.Check{{Name: "a", Status: "fail"}}); code != lmget.ExitFailure {
		t.Fatalf("a failed check must exit %d, got %d", lmget.ExitFailure, code)
	}
}

// Text from a lockfile or a source must not reach the terminal as control
// sequences: only lipgloss's own SGR color codes may appear.
func TestRenderedTextCarriesNoInjectedSequences(t *testing.T) {
	evil := "x\x1b]0;PWNED\x07\x1b[2J\x1b]52;c;ZXZpbA==\x07y"
	err := &lmget.Error{Code: lmget.ExitUsage, Msg: "bad " + evil, Why: evil, Cause: evil,
		Fixes: []lmget.Fix{{Cmd: evil, Note: evil}}}
	p := samplePlan()
	p.License, p.Gated = evil, evil
	for name, out := range map[string]string{
		"error": renderError(err),
		"plan":  renderPlan(p, 100, true),
	} {
		rest := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(out, "")
		if strings.ContainsAny(rest, "\x1b\x07") {
			t.Errorf("%s output still carries control sequences: %q", name, rest)
		}
		if !strings.Contains(rest, "PWNED") {
			t.Errorf("%s output lost the visible text", name)
		}
	}
}

func TestSummaryOfAnOllamaExportSaysHowToRunIt(t *testing.T) {
	p := &lmget.Plan{Ref: lmget.Ref{Source: "ollama", Repo: "library/smollm2", Revision: "135m"}, Commit: strings.Repeat("9", 64),
		Files: []lmget.File{{Path: "model.gguf", Size: 10}}, Bytes: 10, DiskFree: -1}
	o := &lmget.Outcome{Plan: p, Path: "/x/.ollama/models", Fetched: 10, RunAs: "smollm2:135m"}
	out := ansi.ReplaceAllString(renderSummary(o, lmget.Target{Kind: "ollama"}, "/s", "", 100), "")
	if !strings.Contains(out, "ollama:smollm2:135m") || !strings.Contains(out, "ollama run smollm2:135m") {
		t.Fatalf("summary:\n%s", out)
	}
	var b bytes.Buffer
	List(ModePlain, &b, []lmget.Installed{{Ref: p.Ref, Commit: p.Commit}}, lmget.StoreStats{})
	if !strings.HasPrefix(b.String(), "ollama:smollm2:135m\t") {
		t.Fatalf("ls: %q", b.String())
	}
}
