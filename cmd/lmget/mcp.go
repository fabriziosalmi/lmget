package main

// lmget mcp: lmget as a Model Context Protocol server on stdin and stdout,
// for agents (Claude Code, Cursor, Zed, OpenCode). Three tools:
//
//	info  the plan of a model, or the variant a nickname picks here (reads only)
//	list  the models in the store and where they are linked (reads only)
//	get   download, verify and link a model, only when called with confirm: true
//
// The protocol is newline-delimited JSON-RPC 2.0, spoken here in full for
// what lmget needs, so lmget takes no MCP library. Stdout carries the
// protocol and nothing else; nothing in this file prints to it directly.
//
// get links only into the apps and the Hugging Face cache, never into a
// directory: the file names come from the repository, and an agent that
// picked the directory could have a repository write, say, authorized_keys
// into ~/.ssh. A directory is chosen on the command line, by a person.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fabriziosalmi/lmget"
)

// mcpVersions are the protocol versions lmget speaks, newest first.
var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// mcpTargets are the targets get accepts: the apps and the Hugging Face
// cache, or the store alone.
var mcpTargets = []string{"hf", "ollama", "omlx", "lmstudio", "jan", "localai", "exo", "none"}

type rpcIn struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpServer struct {
	mu       sync.Mutex // one message on the wire at a time
	out      io.Writer
	work     sync.Mutex // one tool call at a time: the store is one
	inflight sync.Map   // request id -> context.CancelFunc
	wg       sync.WaitGroup
	// newClient builds the lmget client for a call; tests point it at fakes
	newClient func() (*lmget.Client, error)
}

func doMCP() lmget.ExitCode {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s := &mcpServer{out: os.Stdout, newClient: func() (*lmget.Client, error) { return lmget.New(lmget.Options{}) }}
	if err := s.serve(ctx, os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "lmget mcp:", err)
		return lmget.ExitFailure
	}
	return lmget.ExitOK
}

// serve reads messages until stdin ends, then waits for the calls in flight.
func (s *mcpServer) serve(ctx context.Context, in io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		s.wg.Wait()
	}()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m rpcIn
		if line[0] != '{' || json.Unmarshal(line, &m) != nil || m.JSONRPC != "2.0" {
			s.reply(json.RawMessage("null"), nil, &rpcError{Code: -32700, Message: "not a JSON-RPC 2.0 message"})
			continue
		}
		s.handle(ctx, m)
	}
	return sc.Err()
}

func (s *mcpServer) handle(ctx context.Context, m rpcIn) {
	isRequest := len(m.ID) > 0 && string(m.ID) != "null"
	switch m.Method {
	case "":
		return // a response to a request lmget never sends
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		v := mcpVersions[0]
		for _, known := range mcpVersions {
			if p.ProtocolVersion == known {
				v = known
			}
		}
		s.reply(m.ID, map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "lmget", "title": "lmget", "version": lmget.Version},
			"instructions": "lmget downloads open model weights from their sources and checks every file against the hash the source publishes. " +
				"Call info first: it shows the files, sizes, license and checks without downloading. get downloads only with confirm: true; " +
				"say to the user what it will download, how big it is and where it goes before you confirm.",
		}, nil)
	case "ping":
		s.reply(m.ID, map[string]any{}, nil)
	case "tools/list":
		s.reply(m.ID, map[string]any{"tools": mcpTools()}, nil)
	case "tools/call":
		if !isRequest {
			return
		}
		cctx, cancel := context.WithCancel(ctx)
		s.inflight.Store(string(m.ID), cancel)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.inflight.Delete(string(m.ID))
			defer cancel()
			s.work.Lock()
			defer s.work.Unlock()
			s.reply(m.ID, s.call(cctx, m.Params), nil)
		}()
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(m.Params, &p) == nil {
			if c, ok := s.inflight.Load(string(p.RequestID)); ok {
				c.(context.CancelFunc)()
			}
		}
	default:
		if isRequest { // notifications lmget does not know need no answer
			s.reply(m.ID, nil, &rpcError{Code: -32601, Message: "unknown method " + m.Method})
		}
	}
}

func (s *mcpServer) reply(id json.RawMessage, result any, e *rpcError) {
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		msg["error"] = e
	} else {
		msg["result"] = result
	}
	s.send(msg)
}

func (s *mcpServer) send(msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out.Write(append(b, '\n'))
}

// --- tools -----------------------------------------------------------------

func mcpTools() []map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	globs := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}
	model := str("A model: org/name on Hugging Face (optionally @revision), ollama:name:tag, hf.co/org/name:QUANT (a GGUF repository for Ollama), oci://registry/image:tag, an https URL, " +
		"or a nickname such as qwen, whisper or embed, for which lmget picks the variant that fits this machine.")
	common := map[string]any{
		"model":        model,
		"include":      globs("Only files matching these globs (path or file name), e.g. *Q4_K_M*."),
		"exclude":      globs("Skip files matching these globs."),
		"allow_pickle": map[string]any{"type": "boolean", "description": "Keep pickle weights (.bin, .pt) even when safetensors exist. Leave false."},
		"to":           map[string]any{"type": "string", "enum": mcpTargets, "description": "Where to link the model: an app's models folder, the Hugging Face cache (hf), or the store only (none). Default: hf, or ollama for an Ollama model."},
		"format":       map[string]any{"type": "string", "enum": []string{"safetensors", "mlx", "gguf", "ollama"}, "description": "With a nickname: the format wanted."},
		"for":          str("With a nickname: the memory the model may use, e.g. 8GB. Default: measured on this machine."),
	}
	schema := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	getProps := map[string]any{"confirm": map[string]any{"type": "boolean",
		"description": "true to download. Without it, get returns the plan only. Confirm after telling the user what will be downloaded, its size and where it goes."}}
	for k, v := range common {
		getProps[k] = v
	}
	return []map[string]any{
		{
			"name":        "info",
			"title":       "Plan a model download",
			"description": "Show what lmget would download for a model, without downloading: commit, license, files with sizes and sha256, what is already stored, pickle and gated checks, free disk. For a nickname, the variant chosen for this machine and why.",
			"inputSchema": schema(common, "model"),
			"annotations": map[string]any{"title": "Plan a model download", "readOnlyHint": true, "openWorldHint": true},
		},
		{
			"name":        "list",
			"title":       "Models in the store",
			"description": "List the models in lmget's store: commit, size, files, and the apps or folders each one is linked into.",
			"inputSchema": schema(map[string]any{}),
			"annotations": map[string]any{"title": "Models in the store", "readOnlyHint": true, "openWorldHint": false},
		},
		{
			"name":  "get",
			"title": "Download and verify a model",
			"description": "Download a model, check every file against the hash its source publishes, store each file once and link the model into an app or the Hugging Face cache. " +
				"Without confirm: true it only returns the plan. It never writes into an arbitrary directory.",
			"inputSchema": schema(getProps, "model"),
			"annotations": map[string]any{"title": "Download and verify a model", "readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": true},
		},
	}
}

type mcpArgs struct {
	Model       string   `json:"model"`
	Include     []string `json:"include"`
	Exclude     []string `json:"exclude"`
	AllowPickle bool     `json:"allow_pickle"`
	To          string   `json:"to"`
	Format      string   `json:"format"`
	For         string   `json:"for"`
	Confirm     bool     `json:"confirm"`
}

// call runs one tools/call and returns its result; a tool's failure is a
// result with isError, as the protocol asks, not a JSON-RPC error.
func (s *mcpServer) call(ctx context.Context, params json.RawMessage) map[string]any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return toolError(&lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: "malformed tools/call parameters"})
	}
	var a mcpArgs
	if len(p.Arguments) > 0 && string(p.Arguments) != "null" {
		dec := json.NewDecoder(bytes.NewReader(p.Arguments))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			return toolError(&lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: "arguments of " + p.Name + ": " + err.Error()})
		}
	}
	c, err := s.newClient()
	if err != nil {
		return toolError(err)
	}
	switch p.Name {
	case "list":
		return s.list(c)
	case "info":
		a.Confirm = false
		return s.plan(ctx, c, a, "info", nil)
	case "get":
		return s.plan(ctx, c, a, "get", p.Meta.ProgressToken)
	}
	return toolError(&lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: "unknown tool " + p.Name,
		Why: "lmget's tools are info, list and get"})
}

func (s *mcpServer) list(c *lmget.Client) map[string]any {
	models, err := c.List()
	if err != nil {
		return toolError(err)
	}
	stats := c.Stats()
	var b strings.Builder
	if len(models) == 0 {
		b.WriteString("The store is empty.\n")
	}
	for _, m := range models {
		fmt.Fprintf(&b, "%s at %s: %s, %d files", m.Ref.Display(), short(m.Commit), lmget.HumanBytes(m.Bytes), m.Files)
		if m.Missing > 0 {
			fmt.Fprintf(&b, ", %d missing", m.Missing)
		}
		var to []string
		for _, e := range m.Exports {
			to = append(to, lmget.TargetName(e.Kind)+" ("+e.Path+")")
		}
		if len(to) > 0 {
			fmt.Fprintf(&b, ", linked into %s", strings.Join(to, ", "))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Store: %s.", stats.Root)
	return toolResult(b.String(), map[string]any{"models": models, "store": stats}, false)
}

// plan resolves the arguments to a request and runs it: a dry run for
// info and for get without confirm, a pull for get with it.
func (s *mcpServer) plan(ctx context.Context, c *lmget.Client, a mcpArgs, tool string, progress json.RawMessage) map[string]any {
	if a.To != "" && !contains(mcpTargets, a.To) {
		return toolError(&lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: "lmget mcp does not link into " + lmget.Printable(fmt.Sprintf("%q", a.To)),
			Why:   "the file names come from the repository: a folder is chosen on the command line, by a person, not by an agent",
			Fixes: []lmget.Fix{{Cmd: "to: " + strings.Join(mcpTargets, ", "), Note: "the apps, the Hugging Face cache, or the store only"}}})
	}
	arg := a.Model
	var nick *nickname
	var dec *nickDecision
	if lmget.IsAliasName(arg) {
		d, err := decideNickname(c, arg, a.To, a.Format, a.For)
		if err != nil {
			return toolError(err)
		}
		if nick, err = d.resolved(len(a.Include) > 0, a.To != ""); err != nil {
			return toolError(err)
		}
		dec = d
		if nick.include != nil {
			a.Include = nick.include
		}
		if nick.to != "" {
			a.To = nick.to
		}
		arg = nick.ref.String()
	} else if a.Format != "" || a.For != "" {
		return toolError(&lmget.Error{Code: lmget.ExitUsage, Kind: "usage", Msg: "format and for go with a nickname",
			Why: a.Model + " names the model itself: lmget downloads exactly that"})
	}
	ref, err := lmget.ParseRef(arg)
	if err != nil {
		return toolError(err)
	}
	to := a.To
	if to == "" {
		switch ref.Source {
		case "ollama":
			to = "ollama"
		case "url", "oci":
			to = "none" // the command line puts these in a folder; here they stay in the store
		default:
			to = "hf"
		}
	}
	target, err := lmget.ParseTarget(to)
	if err != nil {
		return toolError(err)
	}
	sel := lmget.Selection{Include: a.Include, Exclude: a.Exclude, AllowPickle: a.AllowPickle}
	if ref.File != "" {
		sel.Include = []string{lmget.GlobEscape(ref.File)}
	}
	dry := !(tool == "get" && a.Confirm)
	req := lmget.Request{Ref: ref, Selection: sel, Target: target, DryRun: dry}
	preferFasterNodes(c)
	out, err := c.Pull(ctx, req, s.progress(progress))
	code := lmget.ExitOK
	if err != nil {
		code = lmget.AsError(err).Code
	}
	recordPull(c, "mcp", req, out, code, nick)
	if err != nil {
		return toolError(err)
	}
	return planResult(out, target, dec, tool, dry)
}

// progress turns the pull's events into MCP progress notifications, when
// the client asked for them, at most about twice a second.
func (s *mcpServer) progress(token json.RawMessage) func(lmget.Event) {
	if len(token) == 0 || string(token) == "null" {
		return nil
	}
	var total int64
	size := map[string]int64{} // the files to fetch, by path
	done := map[string]int64{}
	var last time.Time
	return func(e lmget.Event) {
		switch e.Type {
		case lmget.EvPlan:
			if e.Plan != nil {
				total = e.Plan.ToFetch
				for _, f := range e.Plan.Selected() {
					if !f.Cached {
						size[f.Path] = f.Size
					}
				}
			}
			return
		case lmget.EvProgress:
			done[e.File] = e.Bytes
		case lmget.EvFileDone:
			if e.Cached {
				return // nothing fetched for it
			}
			done[e.File] = size[e.File] // small files end without a progress event
		default:
			return
		}
		if time.Since(last) < 500*time.Millisecond && e.Type == lmget.EvProgress {
			return
		}
		last = time.Now()
		var sum int64
		for _, n := range done {
			sum += n
		}
		params := map[string]any{"progressToken": token, "progress": sum}
		if total > 0 {
			params["total"] = total
			params["message"] = lmget.HumanBytes(sum) + " of " + lmget.HumanBytes(total)
		}
		s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": params})
	}
}

func planResult(out *lmget.Outcome, target lmget.Target, dec *nickDecision, tool string, dry bool) map[string]any {
	p := out.Plan
	var b strings.Builder
	if dec != nil && dec.local == nil {
		v := dec.ch.Variant
		fmt.Fprintf(&b, "Nickname %s: %s for %s", dec.name, strings.TrimPrefix(v.Ref, "hf:"), lmget.TargetName(dec.ch.Target))
		if len(dec.ch.Why) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(dec.ch.Why, "; "))
		}
		b.WriteString(".\n")
	}
	lic := p.License
	if lic == "" {
		lic = "no license declared"
	}
	sel := p.Selected()
	fmt.Fprintf(&b, "%s at %s, %s: %d files, %s", p.Ref.Display(), short(p.Commit), lic, len(sel), lmget.HumanBytes(p.Bytes))
	if dry {
		fmt.Fprintf(&b, ", %s to download, %s free on the store's disk", lmget.HumanBytes(p.ToFetch), lmget.HumanBytes(p.DiskFree))
	}
	b.WriteString(".\n")
	if p.Gated != "" {
		fmt.Fprintf(&b, "Gated (%s): the source asks for an accepted license and a token.\n", p.Gated)
	}
	if len(p.Pickle) > 0 {
		fmt.Fprintf(&b, "Pickle weights selected: %s. Loading them runs code from the file.\n", strings.Join(p.Pickle, ", "))
	}
	if p.FromIndex {
		b.WriteString("The source is unreachable: files and hashes come from the signed index, the bytes from its nodes.\n")
	}
	const most = 40
	for i, f := range sel {
		if i == most {
			fmt.Fprintf(&b, "  and %d more files\n", len(sel)-most)
			break
		}
		state := ""
		if f.Cached {
			state = ", stored"
		}
		hash := "git sha1 " + short(f.GitSHA1)
		if f.SHA256 != "" {
			hash = "sha256 " + short(f.SHA256)
		}
		fmt.Fprintf(&b, "  %s  %s  %s%s\n", f.Path, lmget.HumanBytes(f.Size), hash, state)
	}
	where := lmget.TargetName(target.Kind)
	switch {
	case dry && tool == "get":
		fmt.Fprintf(&b, "Nothing downloaded. Tell the user the size and where it goes (%s), then call get again with confirm: true.", where)
	case dry:
		fmt.Fprintf(&b, "Nothing downloaded. get with confirm: true downloads it into %s.", where)
	default:
		fmt.Fprintf(&b, "Downloaded %s in %.1f s, every file verified, linked into %s", lmget.HumanBytes(out.Fetched), out.Elapsed.Seconds(), where)
		if out.Path != "" {
			fmt.Fprintf(&b, " at %s", out.Path)
		}
		b.WriteString(".")
		if out.RunAs != "" {
			fmt.Fprintf(&b, " Run it with: ollama run %s", out.RunAs)
		}
	}
	data := map[string]any{"plan": p, "target": target.Kind, "downloaded": !dry, "path": out.Path, "fetched": out.Fetched}
	if out.RunAs != "" {
		data["run_as"] = out.RunAs
	}
	if dec != nil && dec.local == nil {
		data["nickname"] = map[string]any{"name": dec.name, "family": dec.alias.Family, "kind": dec.alias.Kind, "choice": dec.ch}
	}
	return toolResult(b.String(), data, false)
}

func toolResult(text string, data any, isError bool) map[string]any {
	r := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
	if data != nil {
		r["structuredContent"] = data
	}
	return r
}

func toolError(err error) map[string]any {
	e := lmget.AsError(err)
	var b strings.Builder
	b.WriteString(e.Msg)
	if e.Why != "" {
		b.WriteString("\nwhy: " + e.Why)
	}
	for _, f := range e.Fixes {
		b.WriteString("\nfix: " + f.Cmd)
		if f.Note != "" {
			b.WriteString(" (" + f.Note + ")")
		}
	}
	return toolResult(b.String(), map[string]any{"error": e}, true)
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
