package lmget

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// reservedAliases are lmget's commands: a nickname with such a name could
// never be reached.
var reservedAliases = map[string]bool{"get": true, "info": true, "sync": true, "ls": true, "rm": true,
	"doctor": true, "index": true, "help": true, "completion": true, "run": true, "history": true, "alias": true, "aliases": true}

// AliasSource is the maintainer's list, aliases.toml in the repository:
//
//	[qwen]
//	kind = "chat"
//	family = "Qwen3.5"
//	[[qwen.variant]]
//	ref = "hf:mlx-community/Qwen3.5-4B-4bit"
//	params = "4B"
//	format = "mlx"
//	quant = "4bit"
type AliasSource map[string]struct {
	Kind    string `toml:"kind"`
	Family  string `toml:"family"`
	Variant []struct {
		Ref     string   `toml:"ref"`
		Params  string   `toml:"params"`
		Format  string   `toml:"format"`
		Quant   string   `toml:"quant"`
		Include []string `toml:"include"`
		License string   `toml:"license"` // Ollama variants only: the registry declares none
	} `toml:"variant"`
}

// ReadAliasSource reads aliases.toml strictly: an unknown key is an error.
func ReadAliasSource(path string) (AliasSource, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var src AliasSource
	md, err := toml.Decode(string(body), &src)
	if err != nil {
		return nil, err
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("unknown key %s", und[0])
	}
	return src, nil
}

// BuildAliases resolves every variant at its source and keeps only what the
// project would mirror: not gated, a license on the allowlist, no pickle
// selected, no code from the repository needed to load it (no auto_map in
// config.json), and files that match the declared format. The sizes are
// measured here, so a client can choose without asking the source.
func (c *Client) BuildAliases(ctx context.Context, src AliasSource, seq int64, valid time.Duration, now time.Time) (*AliasList, error) {
	l := &AliasList{Version: 1, Sequence: seq, Issued: now.UTC().Truncate(time.Second),
		Expires: now.Add(valid).UTC().Truncate(time.Second), Aliases: map[string]Alias{}}
	names := make([]string, 0, len(src))
	for n := range src {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		s := src[name]
		if reservedAliases[name] {
			return nil, fmt.Errorf("%s: a command of lmget, not usable as a nickname", name)
		}
		a := Alias{Kind: s.Kind, Family: s.Family}
		for _, sv := range s.Variant {
			v := AliasVariant{Ref: sv.Ref, Params: sv.Params, Format: sv.Format, Quant: sv.Quant, Include: sv.Include}
			if err := c.checkVariant(ctx, &v, sv.License); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", name, sv.Ref, err)
			}
			a.Variants = append(a.Variants, v)
		}
		l.Aliases[name] = a
	}
	if err := l.validate(); err != nil {
		return nil, err
	}
	return l, nil
}

func (c *Client) checkVariant(ctx context.Context, v *AliasVariant, declared string) error {
	ref, err := ParseRef(v.Ref)
	if err != nil {
		return err
	}
	p, err := c.Resolve(ctx, ref, Selection{Include: v.Include})
	if err != nil {
		return err
	}
	sel := p.Selected()
	if len(sel) == 0 {
		return fmt.Errorf("the selection is empty")
	}
	v.Ref = ref.String()
	v.Bytes = p.Bytes
	switch {
	case p.Gated != "":
		return fmt.Errorf("gated")
	case len(p.Pickle) > 0:
		return fmt.Errorf("pickle files selected: %s", strings.Join(p.Pickle, ", "))
	}
	if ref.Source == "ollama" {
		if !LicenseAllowed(declared) {
			return fmt.Errorf("declare an allowlisted license for an Ollama variant (got %q)", declared)
		}
		v.License = declared
		return nil
	}
	if !LicenseAllowed(p.License) {
		return fmt.Errorf("license %q is not on the allowlist", p.License)
	}
	v.License = p.License
	var ext string
	switch v.Format {
	case "safetensors", "mlx":
		ext = ".safetensors"
	case "gguf":
		ext = ".gguf"
	default:
		return fmt.Errorf("format %q for a Hugging Face model", v.Format)
	}
	n, config := 0, false
	for _, f := range sel {
		if strings.HasSuffix(strings.ToLower(f.Path), ext) {
			n++
		}
		if f.Path == "config.json" {
			config = true
		}
	}
	if n == 0 {
		return fmt.Errorf("no %s file selected", ext)
	}
	if v.Format != "gguf" && !config {
		return fmt.Errorf("no config.json selected")
	}
	if config {
		u := strings.TrimRight(c.opt.HFEndpoint, "/") + "/" + ref.Repo + "/resolve/" + url.PathEscape(p.Commit) + "/config.json"
		body, err := c.fetchSmall(ctx, u, 4<<20)
		if err != nil {
			return fmt.Errorf("config.json: %s", AsError(err).Msg)
		}
		var cfg map[string]json.RawMessage
		if err := json.Unmarshal(body, &cfg); err != nil {
			return fmt.Errorf("config.json is not JSON")
		}
		if _, ok := cfg["auto_map"]; ok {
			return fmt.Errorf("config.json has auto_map: loading it runs code from the repository")
		}
	}
	return nil
}
