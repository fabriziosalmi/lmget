package lmget

import (
	"fmt"
	"strings"
	"testing"
)

func qwenAlias() Alias {
	v := func(ref, params, format string, gb float64) AliasVariant {
		return AliasVariant{Ref: ref, Params: params, Format: format, Bytes: int64(gb * 1e9), License: "apache-2.0"}
	}
	return Alias{Kind: "chat", Family: "Qwen3.5", Variants: []AliasVariant{
		v("hf:Qwen/Qwen3.5-0.8B", "0.8B", "safetensors", 1.77),
		v("hf:Qwen/Qwen3.5-4B", "4B", "safetensors", 9.34),
		v("hf:Qwen/Qwen3.5-9B", "9B", "safetensors", 19.33),
		v("hf:mlx-community/Qwen3.5-0.8B-4bit", "0.8B", "mlx", 0.65),
		v("hf:lmstudio-community/Qwen3.5-4B-MLX-4bit", "4B", "mlx", 3.03),
		v("hf:mlx-community/Qwen3.5-9B-4bit", "9B", "mlx", 5.98),
		v("hf:unsloth/Qwen3.5-4B-GGUF", "4B", "gguf", 2.58),
		v("hf:unsloth/Qwen3.5-9B-GGUF", "9B", "gguf", 5.38),
		v("ollama:qwen3.5:4b", "4B", "ollama", 3.32),
	}}
}

var (
	mac16  = Machine{OS: "darwin", Arch: "arm64", AppleSilicon: true, TotalMem: 17.18e9, AvailMem: 8e9} // 16 GiB, as macOS reports it
	mac32  = Machine{OS: "darwin", Arch: "arm64", AppleSilicon: true, TotalMem: 34.36e9, AvailMem: 20e9}
	pc8    = Machine{OS: "linux", Arch: "amd64", TotalMem: 8e9, AvailMem: 6e9}
	gpu24  = Machine{OS: "linux", Arch: "amd64", TotalMem: 64e9, AvailMem: 40e9, GPU: "RTX", VRAMTotal: 24e9}
	box512 = Machine{OS: "linux", Arch: "amd64", TotalMem: 512e6, ContainerCap: true}
)

func TestChooseVariant(t *testing.T) {
	cases := []struct {
		name   string
		m      Machine
		apps   []string
		o      ChoiceOptions
		ref    string
		target string
	}{
		{"Mac with oMLX: the largest MLX in 30% of 16 GB, not the 9B", mac16, []string{"omlx", "ollama"}, ChoiceOptions{}, "hf:lmstudio-community/Qwen3.5-4B-MLX-4bit", "omlx"},
		{"--for picks smaller", mac16, []string{"omlx"}, ChoiceOptions{Budget: 1e9}, "hf:mlx-community/Qwen3.5-0.8B-4bit", "omlx"},
		{"--to ollama", mac16, []string{"omlx"}, ChoiceOptions{Target: "ollama"}, "ollama:qwen3.5:4b", "ollama"},
		{"PC with LM Studio: GGUF in half of 8 GB", pc8, []string{"lmstudio"}, ChoiceOptions{}, "hf:unsloth/Qwen3.5-4B-GGUF", "lmstudio"},
		{"PC without apps: the HF cache, safetensors", pc8, nil, ChoiceOptions{}, "hf:Qwen/Qwen3.5-0.8B", "hf"},
		{"NVIDIA 24 GB: safetensors in 85% of the VRAM", gpu24, nil, ChoiceOptions{}, "hf:Qwen/Qwen3.5-9B", "hf"},
		{"--format gguf skips an app that cannot load it", mac16, []string{"omlx", "lmstudio"}, ChoiceOptions{Format: "gguf"}, "hf:unsloth/Qwen3.5-4B-GGUF", "lmstudio"},
		{"a 32 GB Mac takes the 9B", mac32, []string{"omlx"}, ChoiceOptions{}, "hf:mlx-community/Qwen3.5-9B-4bit", "omlx"},
		{"disk too small for the largest", mac16, []string{"omlx"}, ChoiceOptions{DiskFree: 2e9}, "hf:mlx-community/Qwen3.5-0.8B-4bit", "omlx"},
	}
	for _, tc := range cases {
		c, err := ChooseVariant(qwenAlias(), tc.m, tc.apps, tc.o)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if c.Variant.Ref != tc.ref || c.Target != tc.target {
			t.Errorf("%s: got %s into %s, want %s into %s", tc.name, c.Variant.Ref, c.Target, tc.ref, tc.target)
		}
		if len(c.Why) == 0 {
			t.Errorf("%s: no reason given", tc.name)
		}
	}
}

func TestChooseVariantSaysWhatIsShortOrBigger(t *testing.T) {
	m := mac16
	m.AvailMem = 1.5e9
	c, err := ChooseVariant(qwenAlias(), m, []string{"omlx"}, ChoiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Tight {
		t.Error("1.5 GB free under a 6 GB model is not reported")
	}
	c, err = ChooseVariant(qwenAlias(), mac16, []string{"omlx"}, ChoiceOptions{Budget: 1e9})
	if err != nil || c.Bigger == nil || c.Bigger.Ref != "hf:lmstudio-community/Qwen3.5-4B-MLX-4bit" {
		t.Fatalf("the next size up is not named: %+v %v", c.Bigger, err)
	}
	if c.Tight {
		t.Error("0.65 GB with 8 GB free reported as tight")
	}
}

func TestChooseVariantRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		m    Machine
		apps []string
		o    ChoiceOptions
		want string
	}{
		"nothing fits a 512 MB container": {box512, nil, ChoiceOptions{}, "fits"},
		"unknown memory":                  {Machine{OS: "linux"}, nil, ChoiceOptions{}, "memory"},
		"unknown format":                  {mac16, nil, ChoiceOptions{Format: "onnx"}, "format"},
		"a format the target cannot load": {mac16, nil, ChoiceOptions{Target: "omlx", Format: "gguf"}, "fits"},
	} {
		_, err := ChooseVariant(qwenAlias(), tc.m, tc.apps, tc.o)
		if err == nil || !strings.Contains(AsError(err).Msg+AsError(err).Why, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"8GB": 8e9, "8 gb": 8e9, "16G": 16e9, "512MB": 512e6, "1.5GB": 1.5e9, "2TB": 2e12} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "GB", "-1GB", "big", "0"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestMachineParsers(t *testing.T) {
	vm := `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                3662.
Pages active:                            153823.
Pages inactive:                          141601.
Pages speculative:                         1000.
Pages throttled:                              0.
Pages wired down:                        224609.
Pages purgeable:                            500.
`
	if got, want := parseVMStat([]byte(vm)), int64((3662+1000+500+141601/2)*16384); got != want {
		t.Errorf("vm_stat: %d, want %d", got, want)
	}
	if parseVMStat([]byte("garbage")) != 0 {
		t.Error("garbage read as memory")
	}
	total, avail := parseMeminfo([]byte("MemTotal:       16303428 kB\nMemFree:  100 kB\nMemAvailable:    8012345 kB\n"))
	if total != 16303428<<10 || avail != 8012345<<10 {
		t.Errorf("meminfo: %d %d", total, avail)
	}
	if parseCgroupMax([]byte("max\n")) != 0 || parseCgroupMax([]byte("4294967296\n")) != 4294967296 {
		t.Error("cgroup memory.max")
	}
}

func TestFormatsForAppleSiliconLMStudio(t *testing.T) {
	if f := formatsFor("lmstudio", mac16); f[0] != "mlx" {
		t.Errorf("LM Studio on Apple Silicon: %v", f)
	}
	if f := formatsFor("lmstudio", pc8); len(f) != 1 || f[0] != "gguf" {
		t.Errorf("LM Studio elsewhere: %v", f)
	}
}

// Found live on 2026-11-17: whisper went into oMLX's directory. The apps run
// chat models; other kinds go to the Hugging Face cache unless --to says so.
func TestNonChatKindsGoToTheHFCache(t *testing.T) {
	a := Alias{Kind: "transcribe", Family: "Whisper", Variants: []AliasVariant{
		{Ref: "hf:openai/whisper-tiny", Params: "39M", Format: "safetensors", Bytes: 155e6, License: "mit"},
	}}
	c, err := ChooseVariant(a, mac16, []string{"omlx", "lmstudio"}, ChoiceOptions{})
	if err != nil || c.Target != "hf" {
		t.Fatalf("a transcriber went to %q (%v)", c.Target, err)
	}
	if c, err := ChooseVariant(a, mac16, []string{"omlx"}, ChoiceOptions{Target: "omlx"}); err != nil || c.Target != "omlx" {
		t.Fatalf("--to omlx was not honoured: %q %v", c.Target, err)
	}
}

// Found live: "the smallest is 5.15 GB, over 5.15 GB" and a --for 16GB out
// of nowhere. Sizes that round alike get one more decimal, and --for names
// the smallest variant.
func TestNoFitSaysByHowMuch(t *testing.T) {
	a := Alias{Kind: "chat", Family: "Gemma 4", Variants: []AliasVariant{
		{Ref: "hf:google/gemma-4-E4B-it-qat-q4_0-gguf", Params: "E4B", Format: "gguf", Bytes: 5_154_000_000, License: "apache-2.0"},
	}}
	_, err := ChooseVariant(a, mac16, nil, ChoiceOptions{Budget: 5_153_960_755})
	e := AsError(err)
	if e == nil || !strings.Contains(e.Why, "5.154 GB, over 5.154 GB") && !strings.Contains(e.Why, "5.154 GB, over 5.153 GB") {
		t.Fatalf("why: %q", e.Why)
	}
	if fmt.Sprint(e.Fixes) == "" || !strings.Contains(fmt.Sprint(e.Fixes), "--for 6GB") {
		t.Fatalf("fixes: %v", e.Fixes)
	}
}
