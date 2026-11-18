package lmget

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
)

// MemoryEstimate says whether a model fits this machine: the weights it
// loads plus its KV cache (keys and values, 16-bit) for a context, against
// the memory lmget lets a model use here (the nicknames' budget). It counts
// no activations nor runtime overhead, and no speed: a floor, labelled so.
type MemoryEstimate struct {
	Weights    int64  `json:"weights"`      // the selected weight files
	KVPerToken int64  `json:"kv_per_token"` // bytes per token of context
	Context    int    `json:"context"`      // tokens the KV cache is counted for
	MaxContext int    `json:"max_context"`  // the model's own maximum
	Total      int64  `json:"total"`        // Weights + KVPerToken*Context
	Budget     int64  `json:"budget"`       // what lmget lets a model use here
	BudgetFrom string `json:"budget_from"`  // how that budget was measured
	Fits       bool   `json:"fits"`
	// UpperBound: sliding-window layers are counted for the whole context,
	// so the KV cache is at most this
	UpperBound bool `json:"upper_bound,omitempty"`
}

// estimateContext is the context the estimate is for, unless the model's
// maximum is lower: a common working length, not the longest possible.
const estimateContext = 8192

// llmConfig is the part of a transformers config.json the KV cache depends
// on; multimodal models keep it in text_config.
type llmConfig struct {
	Layers     int             `json:"num_hidden_layers"`
	Heads      int             `json:"num_attention_heads"`
	KVHeads    int             `json:"num_key_value_heads"`
	Hidden     int             `json:"hidden_size"`
	HeadDim    int             `json:"head_dim"`
	MaxPos     int             `json:"max_position_embeddings"`
	SlidingWin int             `json:"sliding_window"`
	UseSliding *bool           `json:"use_sliding_window"`      // Qwen2: a window set but not used
	FullEvery  int             `json:"full_attention_interval"` // hybrid (Qwen3-Next): one full-attention layer in so many
	LayerTypes []string        `json:"layer_types"`
	KVLoraRank int             `json:"kv_lora_rank"` // MLA (DeepSeek): another cache, not estimated
	RawTextCfg json.RawMessage `json:"text_config"`
}

// kvPerToken returns the KV cache bytes per token, and whether that is an
// upper bound; ok is false when the config does not say enough.
func (c *llmConfig) kvPerToken() (int64, bool, bool) {
	if c.KVLoraRank > 0 || c.Layers <= 0 || c.Heads <= 0 {
		return 0, false, false
	}
	kv := c.KVHeads
	if kv <= 0 {
		kv = c.Heads
	}
	hd := c.HeadDim
	if hd <= 0 {
		if c.Hidden <= 0 || c.Hidden%c.Heads != 0 {
			return 0, false, false
		}
		hd = c.Hidden / c.Heads
	}
	layers := c.Layers
	upper := c.SlidingWin > 0 && (c.UseSliding == nil || *c.UseSliding)
	if len(c.LayerTypes) != c.Layers && c.FullEvery > 1 { // linear-attention layers keep no cache per token
		layers = c.Layers / c.FullEvery
	}
	if len(c.LayerTypes) == c.Layers { // hybrid models: only full attention keeps a cache per token
		full, sliding := 0, 0
		for _, t := range c.LayerTypes {
			switch t {
			case "full_attention":
				full++
			case "sliding_attention":
				sliding++
			}
		}
		layers, upper = full+sliding, sliding > 0
	}
	// keys and values, 2 bytes each; in floating point, since a published
	// config can carry dimensions whose product wraps an int64 (to 0, which
	// would say any model fits). Past 4 GiB a token is no real model.
	per := 2 * float64(layers) * float64(kv) * float64(hd) * 2
	if per <= 0 || per > maxKVPerToken {
		return 0, false, false
	}
	return int64(per), upper, true
}

const maxKVPerToken = 4 << 30

func parseLLMConfig(b []byte) (*llmConfig, bool) {
	var c llmConfig
	if json.Unmarshal(b, &c) != nil {
		return nil, false
	}
	if c.Layers == 0 && len(c.RawTextCfg) > 0 {
		var t llmConfig
		if json.Unmarshal(c.RawTextCfg, &t) != nil {
			return nil, false
		}
		return &t, true
	}
	return &c, true
}

// weightBytes sums the selected files that hold weights.
func weightBytes(p *Plan) (int64, string) {
	var n int64
	format := "safetensors"
	for _, f := range p.Selected() {
		name := strings.ToLower(path.Base(f.Path))
		switch {
		case strings.HasSuffix(name, ".safetensors"):
			n += f.Size
		case strings.HasSuffix(name, ".gguf") && !strings.Contains(name, "mmproj"):
			n += f.Size
			format = "gguf"
		case pickleWeights(name):
			n += f.Size
		}
	}
	return n, format
}

func pickleWeights(name string) bool {
	for _, ext := range []string{".bin", ".pt", ".pth"} {
		if strings.HasSuffix(name, ext) && !strings.HasPrefix(name, "training_args") {
			return true
		}
	}
	return false
}

// estimateMemory computes the estimate from a config and the plan's weights
// for machine m; nil when the config does not say enough.
func estimateMemory(cfg []byte, p *Plan, m Machine) *MemoryEstimate {
	c, ok := parseLLMConfig(cfg)
	if !ok {
		return nil
	}
	perTok, upper, ok := c.kvPerToken()
	if !ok {
		return nil
	}
	weights, format := weightBytes(p)
	if weights == 0 {
		return nil
	}
	ctx := estimateContext
	if c.MaxPos > 0 && c.MaxPos < ctx {
		ctx = c.MaxPos
	}
	e := &MemoryEstimate{Weights: weights, KVPerToken: perTok, Context: ctx, MaxContext: c.MaxPos, UpperBound: upper}
	e.Total = weights + perTok*int64(ctx)
	e.Budget, e.BudgetFrom = budgetFor(m, format)
	e.Fits = e.Budget > 0 && e.Total <= e.Budget
	return e
}

// EstimateMemory reads the plan's config.json (from the store, else from
// the source: a few kB) and says whether the model fits this machine. It
// returns nil for a model without a transformers config (most GGUF and
// Ollama models), or one whose cache lmget cannot count.
func (c *Client) EstimateMemory(ctx context.Context, p *Plan) *MemoryEstimate {
	revoked, err := c.revokedBlobs()
	if err != nil {
		return nil
	}
	return c.estimateMemory(ctx, p, revoked)
}

// plannedFilesAllowed reports whether the checks a pull makes before it
// downloads (revoked blobs, the license policy) let this plan through.
func (c *Client) plannedFilesAllowed(p *Plan, revoked map[string]string, licenses []string) bool {
	for _, f := range p.Selected() {
		if revokedErr(revoked, f.Path, f.SHA256) != nil {
			return false
		}
	}
	return c.licensePolicy(p, licenses) == nil
}

func (c *Client) estimateMemory(ctx context.Context, p *Plan, revoked map[string]string) *MemoryEstimate {
	if p == nil || p.Ref.Source != "hf" {
		return nil
	}
	var cfg *File
	for i := range p.Files {
		if p.Files[i].Path == "config.json" {
			cfg = &p.Files[i]
		}
	}
	if cfg == nil || cfg.Size <= 0 || cfg.Size > 1<<20 || revokedErr(revoked, cfg.Path, cfg.SHA256) != nil {
		return nil
	}
	var b []byte
	if cfg.SHA256 != "" && c.store.Has(cfg.SHA256, cfg.Size) {
		b, _ = os.ReadFile(c.store.BlobPath(cfg.SHA256))
	} else if cfg.url != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.url, nil)
		if err != nil {
			return nil
		}
		c.authorize(req)
		resp, err := c.http.Do(req)
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil
		}
		b, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	}
	// the bytes the plan names, or no estimate (both ids cover the length)
	switch {
	case cfg.SHA256 != "":
		if s := sha256.Sum256(b); hex.EncodeToString(s[:]) != cfg.SHA256 {
			return nil
		}
	case cfg.GitSHA1 != "":
		if gitBlobID(b) != cfg.GitSHA1 {
			return nil
		}
	default:
		return nil
	}
	return estimateMemory(b, p, DetectMachine())
}

// gitBlobID is the id git gives a file's bytes (sha1 of "blob <size>\0" and
// the bytes): what Hugging Face lists for a file kept outside LFS.
func gitBlobID(b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
