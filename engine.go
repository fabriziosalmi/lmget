package lmget

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// lmget run asks the app that serves a model; lmget itself runs no model.
// An app is found at its configured address and recognised by how it
// answers, never by its port: two apps can share one (oMLX and LM Studio
// both default to 1234 on some machines).

// Engine is a running app that answers prompts.
type Engine struct {
	App string `json:"app"` // omlx, lmstudio, ollama, openai (an endpoint you gave)
	URL string `json:"url"` // base URL, without /v1
	key string // the API key the user gave lmget for it; never read from the app
}

// Remote reports whether the engine is on another machine: the prompt
// leaves this one.
func (e *Engine) Remote() bool {
	u, err := url.Parse(e.URL)
	if err != nil {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return !(u.Hostname() == "localhost" || ip != nil && ip.IsLoopback())
}

// omlxBase is oMLX's data directory: the one its launcher reads from
// "~/Library/Application Support/oMLX/base-path", else ~/.omlx.
func omlxBase() string {
	home, _ := os.UserHomeDir()
	if b, err := os.ReadFile(filepath.Join(home, "Library", "Application Support", "oMLX", "base-path")); err == nil {
		if p := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0]); filepath.IsAbs(expandHome(p)) {
			return expandHome(p)
		}
	}
	return filepath.Join(home, ".omlx")
}

// omlxAddress reads server.host and server.port from oMLX's settings (those
// keys only). oMLX serve's own default is 127.0.0.1:8000.
func omlxAddress() string {
	host, port := "127.0.0.1", 8000
	body, err := os.ReadFile(filepath.Join(omlxBase(), "settings.json"))
	if err == nil {
		var s struct {
			Server struct {
				Host string `json:"host"`
				Port int    `json:"port"`
			} `json:"server"`
		}
		if json.Unmarshal(body, &s) == nil {
			if s.Server.Port > 0 && s.Server.Port < 65536 {
				port = s.Server.Port
			}
			if h := s.Server.Host; h != "" && h != "0.0.0.0" && h != "::" {
				host = h
			}
		}
	}
	return fmt.Sprintf("http://%s", net.JoinHostPort(host, fmt.Sprint(port)))
}

// ollamaAddress follows OLLAMA_HOST as Ollama does: host, host:port or a URL.
func ollamaAddress() string {
	h := strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	if h == "" {
		return "http://127.0.0.1:11434"
	}
	if !strings.Contains(h, "://") {
		h = "http://" + h
	}
	u, err := url.Parse(h)
	if err != nil {
		return "http://127.0.0.1:11434"
	}
	host, port := u.Hostname(), u.Port()
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "11434"
	}
	return u.Scheme + "://" + net.JoinHostPort(host, port)
}

// EngineFor returns where the app of target should be listening. endpoint,
// when given, is an OpenAI-compatible server (vLLM, llama-server, ...).
func EngineFor(target, endpoint string) (*Engine, error) {
	if endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, newErr(ExitUsage, "usage", "not an http(s) URL: "+quote(endpoint))
		}
		return &Engine{App: "openai", URL: strings.TrimSuffix(strings.TrimRight(endpoint, "/"), "/v1"), key: os.Getenv("LMGET_RUN_API_KEY")}, nil
	}
	switch target {
	case "omlx":
		u := os.Getenv("LMGET_OMLX_URL")
		if u == "" {
			u = omlxAddress()
		}
		return &Engine{App: "omlx", URL: strings.TrimRight(u, "/"), key: os.Getenv("LMGET_OMLX_API_KEY")}, nil
	case "lmstudio":
		u := os.Getenv("LMGET_LMSTUDIO_URL")
		if u == "" {
			u = "http://127.0.0.1:1234" // the address of every example in LM Studio's documentation
		}
		return &Engine{App: "lmstudio", URL: strings.TrimRight(u, "/"), key: os.Getenv("LMGET_LMSTUDIO_API_KEY")}, nil
	case "ollama":
		return &Engine{App: "ollama", URL: ollamaAddress()}, nil
	}
	return nil, newErr(ExitUsage, "usage", "no app serves "+TargetName(target)).
		why("lmget run asks an app that is running: oMLX, LM Studio, Ollama, or an OpenAI-compatible server").
		fix("--to omlx", "or --to lmstudio, --to ollama").
		fix("--endpoint http://127.0.0.1:8000", "an OpenAI-compatible server (vLLM, llama-server)")
}

func (e *Engine) startHint() string {
	switch e.App {
	case "omlx":
		return "open oMLX, or run omlx serve"
	case "lmstudio":
		return "open LM Studio and start its server, or run lms server start"
	case "ollama":
		return "run ollama serve"
	}
	return "start the server"
}

func (c *Client) engineDo(ctx context.Context, e *Engine, method, p string, body any) (*http.Response, error) {
	return c.engineDoKey(ctx, e, method, p, body, true)
}

// engineDoKey sends the user's key for the app only when withKey: before an
// app is recognised, whatever answers on its port gets no key.
func (c *Client) engineDoKey(ctx context.Context, e *Engine, method, p string, body any, withKey bool) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.URL+p, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if e.key != "" && withKey {
		req.Header.Set("Authorization", "Bearer "+e.key)
	}
	// a client of its own: no proxy (a local app) and no Hugging Face token
	hc := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, newErr(ExitNetwork, "engine", appNames2(e.App)+" is not answering at "+e.URL).
			fix("", e.startHint())
	}
	return resp, nil
}

func appNames2(app string) string {
	switch app {
	case "omlx", "lmstudio":
		return appNames[app]
	case "ollama":
		return "Ollama"
	}
	return "the server"
}

func (c *Client) authErr(e *Engine) error {
	env := map[string]string{"omlx": "LMGET_OMLX_API_KEY", "lmstudio": "LMGET_LMSTUDIO_API_KEY"}[e.App]
	if env == "" {
		env = "LMGET_RUN_API_KEY"
	}
	return newErr(ExitAuth, "engine_auth", appNames2(e.App)+" at "+e.URL+" asks for an API key").
		why("lmget never reads another app's keys: give it the key yourself").
		fix("export "+env+"=<the key>", "")
}

// Identify checks that the app answering at e.URL is the one expected.
func (c *Client) Identify(ctx context.Context, e *Engine) error {
	wrong := func(why string) error {
		return newErr(ExitUsage, "engine", "the server at "+e.URL+" is not "+appNames2(e.App)).why("%s", why).
			fix("", "set "+map[string]string{"omlx": "LMGET_OMLX_URL", "lmstudio": "LMGET_LMSTUDIO_URL", "ollama": "OLLAMA_HOST"}[e.App]+" to its address")
	}
	switch e.App {
	case "ollama":
		resp, err := c.engineDo(ctx, e, http.MethodGet, "/api/version", nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var v struct {
			Version string `json:"version"`
		}
		if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&v) != nil || v.Version == "" {
			return wrong("it does not answer /api/version as Ollama does")
		}
	case "lmstudio":
		// no key first; LM Studio has no answer that names it, so the key the
		// user gave for this address goes only when the server asks for one
		resp, err := c.engineDoKey(ctx, e, http.MethodGet, "/api/v0/models", nil, false)
		if err != nil {
			return err
		}
		if resp.StatusCode == 401 && e.key != "" {
			resp.Body.Close()
			if resp, err = c.engineDo(ctx, e, http.MethodGet, "/api/v0/models", nil); err != nil {
				return err
			}
		}
		defer resp.Body.Close()
		if resp.StatusCode == 401 {
			return c.authErr(e)
		}
		var v struct {
			Data []map[string]any `json:"data"`
		}
		if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&v) != nil {
			return wrong("it does not answer /api/v0/models as LM Studio does")
		}
	case "omlx":
		// recognised without the key: oMLX answers /health to anyone, with
		// fields of its own; any other server on the port gets nothing
		resp, err := c.engineDoKey(ctx, e, http.MethodGet, "/health", nil, false)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var h map[string]json.RawMessage
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&h) != nil || h["status"] == nil || h["engine_pool"] == nil {
			return wrong("it does not answer /health as oMLX does")
		}
		ids, owners, err := c.modelIDs(ctx, e)
		if err != nil {
			return err
		}
		if len(ids) > 0 && !contains(owners, "omlx") {
			return wrong("its models are not owned by omlx")
		}
	}
	return nil
}

func (c *Client) modelIDs(ctx context.Context, e *Engine) (ids, owners []string, err error) {
	resp, err := c.engineDo(ctx, e, http.MethodGet, "/v1/models", nil)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, nil, c.authErr(e)
	}
	var v struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&v) != nil {
		return nil, nil, newErr(ExitNetwork, "engine", fmt.Sprintf("%s answered %s to /v1/models", e.URL, printable(resp.Status)))
	}
	for _, m := range v.Data {
		ids, owners = append(ids, m.ID), append(owners, m.OwnedBy)
	}
	return ids, owners, nil
}

// Reload makes oMLX look at its model directory again: it discovers models
// when it starts, so one lmget just linked is unknown to it until then.
func (c *Client) Reload(ctx context.Context, e *Engine) error {
	if e.App != "omlx" {
		return nil
	}
	resp, err := c.engineDo(ctx, e, http.MethodPost, "/admin/api/reload", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
		return nil
	case 401, 403:
		return newErr(ExitAuth, "engine_auth", "oMLX discovers models when it starts, and refused to look again without its API key").
			why("lmget never reads another app's keys").
			fix("", "restart oMLX (quit and reopen it, or omlx restart), then run the same command").
			fix("export LMGET_OMLX_API_KEY=<the key>", "or let lmget ask oMLX to reload")
	}
	return newErr(ExitNetwork, "engine", "oMLX answered "+printable(resp.Status)+" to a reload")
}

// ModelID is the name the app knows the model by.
func (c *Client) ModelID(ctx context.Context, e *Engine, plan *Plan, runAs string) (string, error) {
	if e.App == "ollama" {
		return runAs, nil
	}
	ids, _, err := c.modelIDs(ctx, e)
	if err != nil {
		return "", err
	}
	repo := plan.Ref.Repo
	want := []string{repo, path.Base(repo), strings.ReplaceAll(repo, "/", "--")}
	for _, w := range want {
		for _, id := range ids {
			if strings.EqualFold(id, w) {
				return id, nil
			}
		}
	}
	return "", newErr(ExitNotFound, "engine", appNames2(e.App)+" does not list "+repo).
		why("it lists %d models; it may need a restart to see a model added while it runs", len(ids)).
		fix("", e.startHint())
}

// AskStats is what the answer cost.
type AskStats struct {
	FirstToken time.Duration `json:"first_token"`
	Total      time.Duration `json:"total"`
	Chars      int           `json:"chars"`
}

// Ask sends prompt to model and streams the answer to out, and the
// reasoning of a model that thinks aloud to thinking (may be nil).
func (c *Client) Ask(ctx context.Context, e *Engine, model, prompt string, maxTokens int, out, thinking io.Writer) (AskStats, error) {
	t0 := time.Now()
	var st AskStats
	emit := func(w io.Writer, s string) {
		if s == "" || w == nil {
			return
		}
		if st.FirstToken == 0 {
			st.FirstToken = time.Since(t0)
		}
		if w == out {
			st.Chars += len(s)
		}
		io.WriteString(w, s)
	}
	msgs := []map[string]string{{"role": "user", "content": prompt}}
	var resp *http.Response
	var err error
	if e.App == "ollama" {
		resp, err = c.engineDo(ctx, e, http.MethodPost, "/api/chat", map[string]any{
			"model": model, "messages": msgs, "stream": true, "options": map[string]any{"num_predict": maxTokens}})
	} else {
		resp, err = c.engineDo(ctx, e, http.MethodPost, "/v1/chat/completions", map[string]any{
			"model": model, "messages": msgs, "stream": true, "max_tokens": maxTokens})
	}
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		msg := printable(strings.TrimSpace(string(b)))
		var je struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(b, &je) == nil { // the app's own words, not its JSON
			if m := je.Error.Message + je.Detail; m != "" {
				msg = printable(m)
			}
		}
		switch {
		case resp.StatusCode == 401 || resp.StatusCode == 403:
			return st, c.authErr(e)
		case resp.StatusCode == 507 || strings.Contains(strings.ToLower(msg), "memory"):
			return st, newErr(ExitFailure, "memory", appNames2(e.App)+" has not enough memory for "+model).
				why("%s", msg).
				fix("", "close some apps, or take a smaller variant (with a nickname: --for 4GB)")
		}
		return st, newErr(ExitFailure, "engine", fmt.Sprintf("%s answered %s", appNames2(e.App), printable(resp.Status))).why("%s", msg)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if e.App == "ollama" {
			var ev struct {
				Message struct {
					Content  string `json:"content"`
					Thinking string `json:"thinking"`
				} `json:"message"`
				Error string `json:"error"`
			}
			if line == "" || json.Unmarshal([]byte(line), &ev) != nil {
				continue
			}
			if ev.Error != "" {
				return st, newErr(ExitFailure, "engine", "Ollama: "+printable(ev.Error))
			}
			emit(thinking, ev.Message.Thinking)
			emit(out, ev.Message.Content)
			continue
		}
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		if ev.Error != nil {
			return st, newErr(ExitFailure, "engine", appNames2(e.App)+": "+printable(ev.Error.Message))
		}
		for _, ch := range ev.Choices {
			emit(thinking, ch.Delta.ReasoningContent+ch.Delta.Reasoning)
			emit(out, ch.Delta.Content)
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return st, newErr(ExitNetwork, "engine", "the answer stopped: "+err.Error())
	}
	st.Total = time.Since(t0)
	return st, ctx.Err()
}
