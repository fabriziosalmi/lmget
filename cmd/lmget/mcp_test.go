package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fabriziosalmi/lmget"
)

const mcpCommit = "0123456789abcdef0123456789abcdef01234567"

// mcpHF is the smallest Hugging Face lmget needs: one repository, two
// files, served at one commit. hold, when set, keeps every file request
// waiting until the request is cancelled. modelBytes counts what was served
// of every file but config.json, which info reads to estimate memory.
type mcpHF struct {
	files      map[string][]byte
	served     atomic.Int64
	modelBytes atomic.Int64
	hold       bool
}

func (f *mcpHF) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/models/{org}/{name}/revision/{rev...}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("org") + "/" + r.PathValue("name"), "sha": mcpCommit,
			"gated": false, "cardData": map[string]any{"license": "apache-2.0"}})
	})
	mux.HandleFunc("GET /api/models/{org}/{name}/tree/"+mcpCommit, func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]any
		for p, b := range f.files {
			s := sha256.Sum256(b)
			out = append(out, map[string]any{"type": "file", "path": p, "size": len(b), "oid": "x",
				"lfs": map[string]any{"oid": hex.EncodeToString(s[:]), "size": len(b)}})
		}
		json.NewEncoder(w).Encode(out)
	})
	files := http.NewServeMux()
	files.HandleFunc("GET /{org}/{name}/resolve/"+mcpCommit+"/{path...}", func(w http.ResponseWriter, r *http.Request) {
		if f.hold {
			<-r.Context().Done()
			return
		}
		b, ok := f.files[r.PathValue("path")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.served.Add(int64(len(b)))
		if r.PathValue("path") != "config.json" {
			f.modelBytes.Add(int64(len(b)))
		}
		http.ServeContent(w, r, r.PathValue("path"), time.Time{}, strings.NewReader(string(b)))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			mux.ServeHTTP(w, r)
		} else {
			files.ServeHTTP(w, r)
		}
	})
}

// tinySafetensors is a valid safetensors file: one F32 tensor of one value.
func tinySafetensors() []byte {
	header := []byte(`{"w":{"dtype":"F32","shape":[1],"data_offsets":[0,4]}}`)
	for len(header)%8 != 0 {
		header = append(header, ' ')
	}
	out := make([]byte, 8, 8+len(header)+4)
	n := uint64(len(header))
	for i := 0; i < 8; i++ {
		out[i] = byte(n >> (8 * i))
	}
	return append(append(out, header...), 0, 0, 0x80, 0x3f) // 1.0
}

// mcpSession runs the server on pipes; send writes a message, wait returns
// the reply to an id.
type mcpSession struct {
	t      *testing.T
	in     *io.PipeWriter
	mu     sync.Mutex
	lines  []string
	byID   map[string]map[string]any
	done   chan struct{}
	notify chan struct{}
}

func startMCP(t *testing.T, hf *mcpHF) (*mcpSession, string) {
	t.Helper()
	tmp := t.TempDir()
	// nothing of this machine: store, Hugging Face cache, home and token all in tmp
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("LMGET_HOME", filepath.Join(tmp, "store"))
	t.Setenv("HF_HUB_CACHE", filepath.Join(tmp, "hfcache"))
	t.Setenv("HF_HOME", filepath.Join(tmp, "hf"))
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HF_TOKEN_PATH", filepath.Join(tmp, "no-token"))
	t.Setenv("LMGET_HISTORY", "off")
	t.Setenv("LMGET_LICENSES", "")
	srv := httptest.NewServer(hf.handler())
	t.Cleanup(srv.Close)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &mcpServer{out: outW, newClient: func() (*lmget.Client, error) {
		return lmget.New(lmget.Options{HFEndpoint: srv.URL, NoMirror: true, IndexURL: "https://127.0.0.1:9/index.json"})
	}}
	sess := &mcpSession{t: t, in: inW, byID: map[string]map[string]any{}, done: make(chan struct{}), notify: make(chan struct{}, 64)}
	go func() {
		defer close(sess.done)
		s.serve(context.Background(), inR)
		outW.Close()
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		for sc.Scan() {
			line := sc.Text()
			var m map[string]any
			sess.mu.Lock()
			sess.lines = append(sess.lines, line)
			if json.Unmarshal([]byte(line), &m) == nil {
				if id, ok := m["id"]; ok && id != nil {
					sess.byID[fmt.Sprint(id)] = m
				}
			}
			sess.mu.Unlock()
			sess.notify <- struct{}{}
		}
	}()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-sess.done:
		case <-time.After(10 * time.Second):
			t.Error("the server did not stop when stdin closed")
		}
	})
	return sess, tmp
}

func (s *mcpSession) send(raw string) {
	s.t.Helper()
	if _, err := io.WriteString(s.in, raw+"\n"); err != nil {
		s.t.Fatal(err)
	}
}

func (s *mcpSession) call(id int, method string, params any) map[string]any {
	s.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	s.send(string(b))
	return s.wait(id)
}

func (s *mcpSession) wait(id int) map[string]any {
	s.t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		s.mu.Lock()
		m, ok := s.byID[fmt.Sprint(id)]
		s.mu.Unlock()
		if ok {
			return m
		}
		select {
		case <-s.notify:
		case <-deadline:
			s.t.Fatalf("no reply to id %d", id)
		}
	}
}

func toolText(t *testing.T, reply map[string]any) (string, bool) {
	t.Helper()
	res, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("not a tool result: %v", reply)
	}
	content := res["content"].([]any)
	return content[0].(map[string]any)["text"].(string), res["isError"].(bool)
}

func TestMCPHandshake(t *testing.T) {
	s, _ := startMCP(t, &mcpHF{})
	r := s.call(1, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t"}})
	if v := r["result"].(map[string]any)["protocolVersion"]; v != "2025-03-26" {
		t.Fatalf("a version lmget speaks is kept: %v", v)
	}
	r = s.call(2, "initialize", map[string]any{"protocolVersion": "1999-01-01"})
	if v := r["result"].(map[string]any)["protocolVersion"]; v != mcpVersions[0] {
		t.Fatalf("an unknown version gets the newest: %v", v)
	}
	s.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if r := s.call(3, "ping", nil); r["result"] == nil {
		t.Fatalf("ping: %v", r)
	}
	tools := s.call(4, "tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	names := map[string]map[string]any{}
	for _, x := range tools {
		m := x.(map[string]any)
		names[m["name"].(string)] = m
	}
	if len(names) != 3 || names["info"] == nil || names["list"] == nil || names["get"] == nil {
		t.Fatalf("tools: %v", names)
	}
	if ann := names["info"]["annotations"].(map[string]any); ann["readOnlyHint"] != true {
		t.Fatal("info must say it only reads")
	}
	enum := names["get"]["inputSchema"].(map[string]any)["properties"].(map[string]any)["to"].(map[string]any)["enum"].([]any)
	for _, e := range enum {
		if strings.HasPrefix(e.(string), "dir") {
			t.Fatal("get must not offer a directory")
		}
	}
	if r := s.call(5, "resources/list", nil); r["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("an unknown method is an error: %v", r)
	}
	s.send(`this is not json`)
	s.send(`{"jsonrpc":"2.0","method":"notifications/whatever"}`)
	s.call(6, "ping", nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	var parseErrors, noID int
	for _, l := range s.lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil || m["jsonrpc"] != "2.0" {
			t.Fatalf("stdout carries only the protocol, got %q", l)
		}
		if e, ok := m["error"].(map[string]any); ok && e["code"].(float64) == -32700 {
			parseErrors++
		}
		if id, ok := m["id"]; ok && id == nil {
			noID++
		}
	}
	// garbage gets one parse error; a notification, known or not, no answer
	if parseErrors != 1 || noID != 1 {
		t.Fatalf("parse errors %d, replies without an id %d: want 1 and 1", parseErrors, noID)
	}
}

func TestMCPGetNeedsConfirm(t *testing.T) {
	hf := &mcpHF{files: map[string][]byte{"config.json": []byte(`{"a":1}`), "model.safetensors": tinySafetensors()}}
	s, tmp := startMCP(t, hf)
	s.call(1, "initialize", map[string]any{"protocolVersion": mcpVersions[0]})

	text, isErr := toolText(t, s.call(2, "tools/call", map[string]any{"name": "info", "arguments": map[string]any{"model": "org/m"}}))
	if isErr || !strings.Contains(text, "apache-2.0") || !strings.Contains(text, "No weights downloaded") {
		t.Fatalf("info: %v %s", isErr, text)
	}
	text, isErr = toolText(t, s.call(3, "tools/call", map[string]any{"name": "get", "arguments": map[string]any{"model": "org/m"}}))
	if isErr || !strings.Contains(text, "confirm: true") || hf.modelBytes.Load() != 0 {
		t.Fatalf("get without confirm downloads no model file: %v served=%d %s", isErr, hf.modelBytes.Load(), text)
	}
	text, isErr = toolText(t, s.call(4, "tools/call", map[string]any{"name": "get", "arguments": map[string]any{"model": "org/m", "confirm": true}}))
	if isErr || !strings.Contains(text, "every file verified") {
		t.Fatalf("get with confirm: %v %s", isErr, text)
	}
	if hf.served.Load() == 0 {
		t.Fatal("nothing was downloaded")
	}
	snap := filepath.Join(tmp, "hfcache", "models--org--m", "snapshots", mcpCommit, "model.safetensors")
	if b, err := os.ReadFile(snap); err != nil || string(b) != string(tinySafetensors()) {
		t.Fatalf("not linked into the Hugging Face cache: %v", err)
	}
	text, _ = toolText(t, s.call(5, "tools/call", map[string]any{"name": "list"}))
	if !strings.Contains(text, "org/m") || !strings.Contains(text, "Hugging Face cache") {
		t.Fatalf("list: %s", text)
	}
}

func TestMCPRefusesWhatAnAgentShouldNotChoose(t *testing.T) {
	hf := &mcpHF{files: map[string][]byte{"authorized_keys": []byte("ssh-ed25519 AAAA attacker")}}
	s, tmp := startMCP(t, hf)
	dir := filepath.Join(tmp, "ssh")
	for i, args := range []map[string]any{
		{"model": "org/m", "to": "dir:" + dir, "confirm": true},
		{"model": "org/m", "to": dir, "confirm": true},
		{"model": "org/m", "path": dir, "confirm": true}, // an argument lmget does not know
		{"model": "org/m", "format": "gguf"},             // a nickname's argument for a repository
		{"model": ""},
	} {
		text, isErr := toolText(t, s.call(10+i, "tools/call", map[string]any{"name": "get", "arguments": args}))
		if !isErr {
			t.Errorf("%v was accepted: %s", args, text)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) || hf.served.Load() != 0 {
		t.Fatalf("something was written or downloaded: %v served=%d", err, hf.served.Load())
	}
	if _, isErr := toolText(t, s.call(20, "tools/call", map[string]any{"name": "rm", "arguments": map[string]any{"model": "org/m"}})); !isErr {
		t.Fatal("an unknown tool is an error")
	}
}

func TestMCPCancel(t *testing.T) {
	hf := &mcpHF{files: map[string][]byte{"model.safetensors": []byte("x")}, hold: true}
	s, _ := startMCP(t, hf)
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": "get", "arguments": map[string]any{"model": "org/m", "confirm": true}}})
	s.send(string(b))
	time.Sleep(300 * time.Millisecond) // the download is waiting on the server
	if r := s.call(8, "ping", nil); r["result"] == nil {
		t.Fatal("a ping is answered while a download runs")
	}
	s.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`)
	if _, isErr := toolText(t, s.wait(7)); !isErr {
		t.Fatal("a cancelled download ends as an error")
	}
}

// An agent's URL cannot make lmget mcp reach this machine or its networks:
// the connection is refused before any request.
func TestMCPURLsReachPublicHostsOnly(t *testing.T) {
	var hits atomic.Int64
	local := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer local.Close()
	s, _ := startMCP(t, &mcpHF{files: map[string][]byte{}})
	s.call(1, "initialize", map[string]any{"protocolVersion": mcpVersions[0]})
	for i, u := range []string{local.URL + "/model.gguf", "https://169.254.169.254/latest/meta-data/x.gguf"} {
		for _, tool := range []string{"info", "get"} {
			text, isErr := toolText(t, s.call(10+i, "tools/call", map[string]any{"name": tool, "arguments": map[string]any{"model": u, "confirm": true}}))
			if !isErr || !strings.Contains(text, "not a public address") {
				t.Errorf("%s %s: %v %s", tool, u, isErr, text)
			}
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("a private host got %d requests", hits.Load())
	}
}

// One message too long for lmget mcp gets an error; the session goes on.
func TestMCPSurvivesAnOverlongLine(t *testing.T) {
	s, _ := startMCP(t, &mcpHF{files: map[string][]byte{}})
	s.send(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("x", 5<<20) + `"}}`)
	if r := s.call(2, "ping", nil); r["error"] != nil {
		t.Fatalf("after an overlong line: %v", r)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tooLong := 0
	for _, l := range s.lines {
		if strings.Contains(l, "longer than") {
			tooLong++
		}
	}
	if tooLong != 1 {
		t.Fatalf("%d errors for the overlong line, want 1", tooLong)
	}
}
