package lmget

import (
	"context"
	"net/http/httptest"
	"testing"
)

// the fields of real config.json files (2026-11), read from Hugging Face
const (
	cfgQwen3_06B   = `{"num_hidden_layers":28,"num_attention_heads":16,"num_key_value_heads":8,"hidden_size":1024,"head_dim":128,"max_position_embeddings":40960,"sliding_window":null,"use_sliding_window":false}`
	cfgQwen35_4B   = `{"model_type":"qwen3_5","text_config":{"num_hidden_layers":32,"num_attention_heads":16,"num_key_value_heads":4,"hidden_size":2560,"head_dim":256,"max_position_embeddings":262144,"full_attention_interval":4,"layer_types":["linear_attention","linear_attention","linear_attention","full_attention","linear_attention","linear_attention","linear_attention","full_attention","linear_attention","linear_attention","linear_attention","full_attention","linear_attention","linear_attention","linear_attention","full_attention","linear_attention","linear_attention","linear_attention","full_attention","linear_attention","linear_attention","linear_attention","full_attention","linear_attention","linear_attention","linear_attention","full_attention","linear_attention","linear_attention","linear_attention","full_attention"]}}`
	cfgQwen3Next   = `{"num_hidden_layers":48,"num_attention_heads":16,"num_key_value_heads":2,"hidden_size":2048,"head_dim":256,"max_position_embeddings":262144,"full_attention_interval":4,"use_sliding_window":false}`
	cfgGemma3_4B   = `{"text_config":{"num_hidden_layers":34,"num_attention_heads":8,"num_key_value_heads":4,"hidden_size":2560,"head_dim":256,"max_position_embeddings":131072,"sliding_window":1024}}`
	cfgQwen25      = `{"num_hidden_layers":24,"num_attention_heads":14,"num_key_value_heads":2,"hidden_size":896,"max_position_embeddings":32768,"sliding_window":32768,"use_sliding_window":false}`
	cfgDeepSeekV3  = `{"num_hidden_layers":61,"num_attention_heads":128,"num_key_value_heads":128,"hidden_size":7168,"kv_lora_rank":512,"max_position_embeddings":163840}`
	cfgNoHeadDim   = `{"num_hidden_layers":16,"num_attention_heads":32,"num_key_value_heads":8,"hidden_size":2048,"max_position_embeddings":131072}`
	cfgShortWindow = `{"num_hidden_layers":2,"num_attention_heads":2,"hidden_size":128,"max_position_embeddings":512}`
)

func TestKVPerToken(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg   string
		want  int64
		upper bool
		ok    bool
	}{
		"Qwen3-0.6B":              {cfgQwen3_06B, 2 * 28 * 8 * 128 * 2, false, true},
		"Qwen3.5-4B, layer_types": {cfgQwen35_4B, 2 * 8 * 4 * 256 * 2, false, true},
		"Qwen3-Next, interval":    {cfgQwen3Next, 2 * 12 * 2 * 256 * 2, false, true},
		"Gemma 3 4B, window":      {cfgGemma3_4B, 2 * 34 * 4 * 256 * 2, true, true},
		"Qwen2.5, window unused":  {cfgQwen25, 2 * 24 * 2 * 64 * 2, false, true},
		"no head_dim":             {cfgNoHeadDim, 2 * 16 * 8 * 64 * 2, false, true},
		"DeepSeek-V3, MLA":        {cfgDeepSeekV3, 0, false, false},
		"not a config":            {`{"a":1}`, 0, false, false},
	} {
		c, ok := parseLLMConfig([]byte(tc.cfg))
		if !ok {
			t.Fatalf("%s: not parsed", name)
		}
		got, upper, ok := c.kvPerToken()
		if ok != tc.ok || got != tc.want || upper != tc.upper {
			t.Errorf("%s: %d upper=%v ok=%v, want %d upper=%v ok=%v", name, got, upper, ok, tc.want, tc.upper, tc.ok)
		}
	}
}

func TestEstimateMemory(t *testing.T) {
	gib := int64(1) << 30
	mac16 := Machine{AppleSilicon: true, TotalMem: 16 * gib}
	plan := func(weights int64) *Plan {
		return &Plan{Ref: hfRef("org/m"), Files: []File{{Path: "model.safetensors", Size: weights}, {Path: "config.json", Size: 700}, {Path: "README.md", Size: 3000}}}
	}
	e := estimateMemory([]byte(cfgQwen3_06B), plan(1_500_000_000), mac16)
	if e == nil || e.Weights != 1_500_000_000 || e.Context != estimateContext || e.MaxContext != 40960 {
		t.Fatalf("%+v", e)
	}
	if e.Total != 1_500_000_000+2*28*8*128*2*8192 || e.Budget != 16*gib*30/100 || !e.Fits {
		t.Fatalf("a 0.6B model on a 16 GB Mac: %+v", e)
	}
	if e := estimateMemory([]byte(cfgQwen3_06B), plan(16_000_000_000), mac16); e == nil || e.Fits {
		t.Fatalf("16 GB of weights on a 16 GB Mac fit: %+v", e)
	}
	if e := estimateMemory([]byte(cfgShortWindow), plan(1000), mac16); e == nil || e.Context != 512 {
		t.Fatalf("the model's own maximum is shorter: %+v", e)
	}
	if e := estimateMemory([]byte(cfgDeepSeekV3), plan(1000), mac16); e != nil {
		t.Fatal("an estimate for a cache lmget cannot count")
	}
	gguf := &Plan{Ref: hfRef("org/m"), Files: []File{{Path: "m-Q4_K_M.gguf", Size: 4000}, {Path: "mmproj-F16.gguf", Size: 900}}}
	if w, format := weightBytes(gguf); w != 4000 || format != "gguf" {
		t.Fatalf("GGUF weights %d %s: the projector is not the model", w, format)
	}
	nvidia := Machine{TotalMem: 64 * gib, GPU: "RTX 4080", VRAMTotal: 16 * gib}
	if e := estimateMemory([]byte(cfgQwen3_06B), plan(1000), nvidia); e == nil || e.Budget != 16*gib*85/100 {
		t.Fatalf("an NVIDIA budget is its VRAM: %+v", e)
	}
}

func TestInfoEstimatesMemory(t *testing.T) {
	f := &fakeHF{files: []fakeFile{
		{path: "config.json", data: []byte(cfgQwen3_06B)},
		{path: "model.safetensors", data: fakeSafetensors(4000), lfs: true},
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	out, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}, DryRun: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := out.Plan.Memory
	if m == nil || m.Weights != int64(len(f.files[1].data)) || m.KVPerToken != 2*28*8*128*2 {
		t.Fatalf("info: %+v", m)
	}
	out, err = c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil)
	if err != nil || out.Plan.Memory != nil {
		t.Fatalf("a download estimated memory: %+v %v", out.Plan.Memory, err)
	}
}

// info reads config.json only when its bytes are the ones the plan names:
// by git id when Hugging Face keeps it in git, by sha256 when in LFS.
func TestInfoRefusesAConfigOtherThanThePlanned(t *testing.T) {
	for _, lfs := range []bool{false, true} {
		f := &fakeHF{files: []fakeFile{
			{path: "config.json", data: []byte(cfgQwen3_06B), lfs: lfs, corrupt: true},
			{path: "model.safetensors", data: fakeSafetensors(4000), lfs: true},
		}}
		srv := httptest.NewServer(f.handler())
		c := newTestClient(t, srv, t.TempDir())
		out, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}, DryRun: true}, nil)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if f.served.Load() == 0 || out.Plan.Memory != nil {
			t.Fatalf("lfs=%t: a config.json other than the planned one was used: served=%d %+v", lfs, f.served.Load(), out.Plan.Memory)
		}
	}
}

// Dimensions whose product overflows (a published config is anyone's) give
// no estimate: wrapped to 0 or negative, the KV cache would say it fits.
func TestEstimateMemoryRefusesOverflow(t *testing.T) {
	p := &Plan{Files: []File{{Path: "model.safetensors", Size: 1000}}}
	m := Machine{OS: "darwin", AppleSilicon: true, TotalMem: 16 << 30}
	for _, cfg := range []string{
		`{"num_hidden_layers":2147483648,"num_attention_heads":1,"num_key_value_heads":2147483648,"head_dim":4}`, // 2^66: wraps to 0
		`{"num_hidden_layers":3037000500,"num_attention_heads":1,"num_key_value_heads":3037000500,"head_dim":3}`, // wraps negative
		`{"num_hidden_layers":64,"num_attention_heads":64,"num_key_value_heads":64,"head_dim":4294967296}`,
	} {
		if e := estimateMemory([]byte(cfg), p, m); e != nil {
			t.Errorf("%s: %+v", cfg, e)
		}
	}
}
