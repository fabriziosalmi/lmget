package lmget

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DetectApps lists the apps installed here that lmget can link models into,
// in the order a nickname prefers them. It looks at files only: whether an
// app is running is not needed to put a model where it looks.
func DetectApps(m Machine) []string {
	home, _ := os.UserHomeDir()
	exists := func(p string) bool { _, err := os.Stat(p); return p != "" && err == nil }
	has := map[string]bool{
		"omlx": os.Getenv("LMGET_OMLX_DIR") != "" || exists(filepath.Join(omlxBase(), "settings.json")) ||
			exists("/Applications/oMLX.app"),
		"lmstudio": os.Getenv("LMGET_LMSTUDIO_DIR") != "" || exists(filepath.Join(home, ".lmstudio")) ||
			exists("/Applications/LM Studio.app"),
		"ollama": os.Getenv("OLLAMA_MODELS") != "" || exists(filepath.Join(home, ".ollama", "models")),
	}
	if _, err := exec.LookPath("ollama"); err == nil {
		has["ollama"] = true
	}
	order := []string{"lmstudio", "ollama"}
	if m.AppleSilicon {
		order = []string{"omlx", "lmstudio", "ollama"} // MLX first where it runs
	}
	var out []string
	for _, a := range order {
		if has[a] {
			out = append(out, a)
		}
	}
	return out
}

// formatsFor lists the formats a target loads, best first.
func formatsFor(target string, m Machine) []string {
	switch target {
	case "omlx":
		return []string{"mlx", "safetensors"}
	case "lmstudio":
		if m.AppleSilicon {
			return []string{"mlx", "gguf"}
		}
		return []string{"gguf"}
	case "ollama":
		return []string{"ollama"}
	}
	// the Hugging Face cache and plain directories hold any of them;
	// transformers and vLLM read safetensors, mlx-lm reads MLX
	if m.AppleSilicon {
		return []string{"safetensors", "mlx", "gguf"}
	}
	return []string{"safetensors", "gguf"}
}

// StoreDiskFree is the free space on the store's disk, 0 when unknown.
func (c *Client) StoreDiskFree() int64 { return diskFree(c.store.Root()) }

// ChoiceOptions are what the user said; empty fields are decided here.
type ChoiceOptions struct {
	Target   string // --to, "" when not given
	Format   string // --format
	Budget   int64  // --for: memory for the model's weights, bytes
	DiskFree int64  // on the store's disk, 0 when unknown
}

// Choice is the variant lmget proposes, with why.
type Choice struct {
	Variant AliasVariant  `json:"variant"`
	Target  string        `json:"target"`
	Budget  int64         `json:"budget"`
	Why     []string      `json:"why"`
	Bigger  *AliasVariant `json:"bigger,omitempty"` // the next size up, that did not fit
	Tight   bool          `json:"tight,omitempty"`  // the memory free now is below the weights
}

// budgetFor is the memory the weights may take: a share of the machine's,
// leaving room for the runtime, the context cache and the system.
func budgetFor(m Machine, format string) (int64, string) {
	if m.VRAMTotal > 0 && (format == "safetensors" || format == "gguf") {
		return m.VRAMTotal * 85 / 100, fmt.Sprintf("85%% of the %s of the %s", HumanBytes(m.VRAMTotal), m.GPU)
	}
	if m.AppleSilicon {
		// unified memory, shared with macOS, the apps and the model's
		// context cache. On a 16 GB Mac, an 8B model at 4 bits (4.35 GB)
		// runs and a 9B one (5.98 GB) does not, in Fab's experience: 30%
		// (5.15 GB there) keeps the first and leaves out the second
		return m.TotalMem * 30 / 100, fmt.Sprintf("30%% of %s unified memory", HumanBytes(m.TotalMem))
	}
	what := "RAM"
	if m.ContainerCap {
		what = "the container's memory limit"
	}
	return m.TotalMem * 50 / 100, fmt.Sprintf("50%% of %s %s", HumanBytes(m.TotalMem), what)
}

// ChooseVariant picks the target and the largest variant whose weights fit
// the memory budget and the disk. The pick depends on the machine, not on
// what happens to be free at this moment, so it is the same every time;
// memory that is short right now is reported instead (Tight).
func ChooseVariant(a Alias, m Machine, apps []string, o ChoiceOptions) (Choice, error) {
	if o.Format != "" && !AliasFormats[o.Format] {
		return Choice{}, newErr(ExitUsage, "usage", "unknown format "+quote(o.Format)).
			fix("--format mlx", "or safetensors, gguf, ollama")
	}
	targets := []string{o.Target}
	switch {
	case o.Target != "":
	case a.Kind != "chat":
		// oMLX, LM Studio and Ollama run chat models; a transcriber, an
		// embedder or a reranker is loaded by transformers or
		// sentence-transformers, from the Hugging Face cache
		targets = []string{"hf"}
	default:
		targets = append(append([]string{}, apps...), "hf")
	}
	var why []string
	var smallest int64 // the smallest variant that did not fit, for the --for suggestion
	for _, t := range targets {
		formats := formatsFor(t, m)
		if o.Format != "" {
			if !contains(formats, o.Format) {
				continue
			}
			formats = []string{o.Format}
		}
		for _, f := range formats {
			var vs []AliasVariant
			for _, v := range a.Variants {
				if v.Format == f {
					vs = append(vs, v)
				}
			}
			if len(vs) == 0 {
				continue
			}
			sort.Slice(vs, func(i, j int) bool { return vs[i].Bytes < vs[j].Bytes })
			budget, from := o.Budget, "--for"
			if budget == 0 {
				if m.TotalMem == 0 {
					return Choice{}, newErr(ExitUsage, "usage", "cannot read this machine's memory").
						fix("--for 8GB", "say how much memory the model may use")
				}
				budget, from = budgetFor(m, f)
			}
			pick := -1
			for i, v := range vs {
				if v.Bytes <= budget && (o.DiskFree == 0 || v.Bytes < o.DiskFree) {
					pick = i
				}
			}
			if pick < 0 {
				a, b := apart(vs[0].Bytes, budget)
				why = append(why, fmt.Sprintf("%s for %s: the smallest is %s, over %s (%s)", f, TargetName(t), a, b, from))
				if smallest == 0 || vs[0].Bytes < smallest {
					smallest = vs[0].Bytes
				}
				continue
			}
			c := Choice{Variant: vs[pick], Target: t, Budget: budget}
			if o.Target == "" {
				if t == "hf" && a.Kind != "chat" {
					c.Why = append(c.Why, "a "+a.Kind+" model, for transformers and sentence-transformers: the Hugging Face cache")
				} else if t == "hf" {
					c.Why = append(c.Why, "no app found that loads this family's formats: the Hugging Face cache")
				} else {
					c.Why = append(c.Why, TargetName(t)+" is installed here")
				}
			}
			c.Why = append(c.Why, fmt.Sprintf("the largest %s that fits %s (%s)", f, HumanBytes(budget), from))
			if pick+1 < len(vs) {
				b := vs[pick+1]
				c.Bigger = &b
			}
			if m.AvailMem > 0 && c.Variant.Bytes > m.AvailMem {
				c.Tight = true
			}
			return c, nil
		}
	}
	e := newErr(ExitUsage, "usage", "no variant of this nickname fits here")
	if len(why) > 0 {
		e = e.why("%s", strings.Join(why, "; "))
	}
	if smallest > 0 {
		need := fmt.Sprintf("--for %dGB", (smallest+999_999_999)/1_000_000_000)
		e = e.fix(need, "allow the smallest variant ("+HumanBytes(smallest)+"), if this machine can take it")
	}
	return Choice{}, e.fix("--format gguf", "or another format")
}

// apart prints two sizes so that they read different when they are: one
// decimal more when the usual one makes them look equal.
func apart(a, b int64) (string, string) {
	sa, sb := HumanBytes(a), HumanBytes(b)
	if sa == sb && a != b {
		return fmt.Sprintf("%.3f GB", float64(a)/1e9), fmt.Sprintf("%.3f GB", float64(b)/1e9)
	}
	return sa, sb
}

// TargetName is how lmget names a target to people.
func TargetName(t string) string {
	switch t {
	case "omlx", "lmstudio":
		return appNames[t]
	case "ollama":
		return "Ollama"
	}
	return "the Hugging Face cache"
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

// ParseSize reads "8GB", "16G", "512MB", "1.5GB" (decimal units, as the
// sizes lmget prints).
func ParseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"T", 1e12}, {"G", 1e9}, {"M", 1e6}} {
		if strings.HasSuffix(t, u.suf) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suf)), u.m
			break
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || f <= 0 || f > 1e6 && mult >= 1e9 {
		return 0, newErr(ExitUsage, "usage", "not a size: "+quote(s)).fix("--for 8GB", "")
	}
	return int64(f * float64(mult)), nil
}
