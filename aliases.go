package lmget

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/BurntSushi/toml"
)

// Nicknames: "lmget qwen" instead of an org/name. The project publishes them
// as aliases.json next to the index, signed by the same keys, in a file of
// its own: index.json is decoded strictly, so a new field there would make
// every older client refuse the index. A user's own nicknames, in
// $LMGET_HOME/aliases.toml, win over the project's.

// AliasList is the signed list of the project's nicknames.
type AliasList struct {
	Version  int              `json:"version"` // 1
	Sequence int64            `json:"sequence"`
	Issued   time.Time        `json:"issued"`
	Expires  time.Time        `json:"expires"`
	Aliases  map[string]Alias `json:"aliases"`
	signedBy string
}

// Alias is one nickname: a family of models, in several sizes and formats.
type Alias struct {
	Kind     string         `json:"kind"`   // chat, transcribe, embed, rerank, image-embed
	Family   string         `json:"family"` // e.g. "Qwen3.5"
	Variants []AliasVariant `json:"variants"`
}

// AliasVariant is one concrete model of a nickname.
type AliasVariant struct {
	Ref     string   `json:"ref"`    // hf:Org/Name or ollama:name:tag
	Params  string   `json:"params"` // as the publisher writes it: "0.8B", "4B", "E4B"
	Format  string   `json:"format"` // safetensors, mlx, gguf, ollama
	Quant   string   `json:"quant,omitempty"`
	Include []string `json:"include,omitempty"`
	Bytes   int64    `json:"bytes"` // the selected files, as the builder measured them
	License string   `json:"license"`
}

// AliasFormats are the formats a variant can have.
var AliasFormats = map[string]bool{"safetensors": true, "mlx": true, "gguf": true, "ollama": true}

var aliasNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// IsAliasName reports whether s can only be a nickname: no "/" and no ":",
// which every other model reference has.
func IsAliasName(s string) bool { return aliasNameRe.MatchString(s) }

const aliasesMaxBytes = 4 << 20

func (l *AliasList) validate() error {
	switch {
	case l.Version != 1:
		return fmt.Errorf("version %d is not supported", l.Version)
	case l.Sequence < 1:
		return fmt.Errorf("sequence %d", l.Sequence)
	case !l.Expires.After(l.Issued):
		return fmt.Errorf("expires before it is issued")
	case l.Expires.Sub(l.Issued) > MaxIndexValidity:
		return fmt.Errorf("valid for more than %d days", int(MaxIndexValidity.Hours()/24))
	case len(l.Aliases) > 1000:
		return fmt.Errorf("%d nicknames", len(l.Aliases))
	}
	for name, a := range l.Aliases {
		if err := a.validate(name); err != nil {
			return err
		}
	}
	return nil
}

func (a *Alias) validate(name string) error {
	if !IsAliasName(name) || reservedAliases[name] {
		return fmt.Errorf("nickname %s: not a valid name", quote(name))
	}
	if len(a.Variants) == 0 || len(a.Variants) > 64 {
		return fmt.Errorf("nickname %s: %d variants", name, len(a.Variants))
	}
	if printable(a.Kind) != a.Kind || printable(a.Family) != a.Family || len(a.Kind) > 32 || len(a.Family) > 64 {
		return fmt.Errorf("nickname %s: bad kind or family", name)
	}
	for _, v := range a.Variants {
		ref, err := ParseRef(v.Ref)
		if err != nil || ref.Revision != "main" && ref.Source == "hf" || ref.File != "" || ref.SHA256 != "" {
			return fmt.Errorf("nickname %s: variant %s is not a plain model reference", name, quote(v.Ref))
		}
		if ref.Source != "hf" && ref.Source != "ollama" {
			return fmt.Errorf("nickname %s: variant %s: only Hugging Face and Ollama models", name, quote(v.Ref))
		}
		if !AliasFormats[v.Format] || (v.Format == "ollama") != (ref.Source == "ollama") {
			return fmt.Errorf("nickname %s: variant %s: format %s", name, quote(v.Ref), quote(v.Format))
		}
		if v.Bytes <= 0 || v.Bytes > MaxFileSize {
			return fmt.Errorf("nickname %s: variant %s: size %d", name, quote(v.Ref), v.Bytes)
		}
		for _, s := range append([]string{v.Params, v.Quant, v.License}, v.Include...) {
			if printable(s) != s || len(s) > 200 {
				return fmt.Errorf("nickname %s: variant %s: unprintable field", name, quote(v.Ref))
			}
		}
	}
	return nil
}

// openAliases verifies a signed nickname list, as openIndex does an index.
func (c *Client) openAliases(data []byte, sig string, now time.Time) (*AliasList, error) {
	keys, _, err := c.trustedIndexKeys(now)
	if err != nil {
		return nil, err
	}
	signedBy := ""
	for _, k := range keys {
		if _, err := verifyMinisign(k, data, sig); err == nil {
			signedBy = KeyNumber(k)
			break
		}
	}
	if signedBy == "" {
		return nil, newErr(ExitVerify, "verify", "the nickname list signature is not valid").why("%s", c.whyNotTrusted(sig, keys))
	}
	var l AliasList
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return nil, newErr(ExitVerify, "verify", "the signed nickname list is not valid JSON").wrap(err)
	}
	if err := l.validate(); err != nil {
		return nil, newErr(ExitVerify, "verify", "the signed nickname list is not valid").why("%s", err.Error())
	}
	if l.Issued.After(now.Add(indexClockSkew)) || l.Expires.Sub(now) > MaxIndexValidity+indexClockSkew {
		return nil, newErr(ExitVerify, "verify", "the nickname list is dated in the future").
			fix("", "check this machine's clock")
	}
	if !now.Before(l.Expires) {
		return nil, newErr(ExitVerify, "verify", "the project's nickname list expired on "+l.Expires.UTC().Format(time.DateOnly)).
			why("an expired list may name models the project no longer recommends").
			fix("lmget index", "fetch the current one").
			fix("lmget <org>/<name>", "or name the model itself")
	}
	l.signedBy = signedBy
	return &l, nil
}

func (c *Client) aliasPaths() (data, sig, highest string) {
	d := filepath.Join(c.store.Root(), "signed-index")
	return filepath.Join(d, "aliases.json"), filepath.Join(d, "aliases.json.minisig"), filepath.Join(d, "aliases-highest")
}

// AliasesURL is aliases.json next to the index.
func (c *Client) AliasesURL() string {
	u, err := url.Parse(c.opt.IndexURL)
	if err != nil {
		return ""
	}
	u.Path = path.Join(path.Dir(u.Path), "aliases.json")
	return u.String()
}

// CachedAliases returns the cached nickname list, verified again; nil and no
// error when none was ever fetched.
func (c *Client) CachedAliases() (*AliasList, time.Time, error) {
	p, ps, _ := c.aliasPaths()
	fi, err := os.Stat(p)
	if err != nil {
		return nil, time.Time{}, nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, time.Time{}, diskErr(err)
	}
	sig, err := os.ReadFile(ps)
	if err != nil {
		return nil, time.Time{}, newErr(ExitUsage, "usage", "the cached nickname list has no signature").fix("lmget index", "fetch it again")
	}
	l, err := c.openAliases(data, string(sig), time.Now())
	return l, fi.ModTime(), err
}

// UpdateAliases fetches the project's nickname list. A project that publishes
// none answers 404: that is (nil, nil). An older sequence than one already
// accepted is refused, so a replayed list cannot point a nickname back.
func (c *Client) UpdateAliases(ctx context.Context) (*AliasList, error) {
	u := c.AliasesURL()
	if pu, err := url.Parse(u); u == "" || err != nil || pu.Scheme != "https" {
		return nil, newErr(ExitUsage, "usage", "no index URL is configured").why("LMGET_INDEX_URL must be an https URL")
	}
	if err := c.UpdateKeys(ctx); err != nil {
		return nil, err
	}
	data, err := c.fetchSmall(ctx, u, aliasesMaxBytes)
	if err != nil && AsError(err).Code == ExitNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sig, err := c.fetchSmall(ctx, u+".minisig", sigMaxBytes)
	if err != nil {
		return nil, err
	}
	l, err := c.openAliases(data, string(sig), time.Now())
	if err != nil {
		return nil, err
	}
	p, ps, hp := c.aliasPaths()
	sum := sha256.Sum256(data)
	var seq int64
	var had string
	if b, err := os.ReadFile(hp); err == nil {
		if n, _ := fmt.Sscanf(string(b), "%d %64s", &seq, &had); n == 2 {
			switch {
			case l.Sequence < seq:
				return nil, newErr(ExitVerify, "verify", fmt.Sprintf("the nickname list served is older than one already accepted (%d < %d)", l.Sequence, seq))
			case l.Sequence == seq && had != hex.EncodeToString(sum[:]):
				return nil, newErr(ExitVerify, "verify", "two different nickname lists carry the same sequence number")
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, diskErr(err)
	}
	// as for the index: the accepted sequence first, then signature, then data
	if err := writeFileAtomic(hp, []byte(fmt.Sprintf("%d %x\n", l.Sequence, sum))); err != nil {
		return nil, diskErr(err)
	}
	if err := writeFileAtomic(ps, sig); err != nil {
		return nil, diskErr(err)
	}
	if err := writeFileAtomic(p, data); err != nil {
		return nil, diskErr(err)
	}
	return l, nil
}

// LocalAliasesPath is the user's own nicknames file.
func (c *Client) LocalAliasesPath() string { return filepath.Join(c.store.Root(), "aliases.toml") }

// LocalAliases reads the user's nicknames:
//
//	[aliases]
//	mine = "hf:mlx-community/Qwen3.5-4B-4bit"
//	asr  = "Systran/faster-whisper-small"
//
// A missing file is no nicknames.
func (c *Client) LocalAliases() (map[string]Ref, error) {
	p := c.LocalAliasesPath()
	body, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, diskErr(err)
	}
	var f struct {
		Aliases map[string]string `toml:"aliases"`
	}
	md, err := toml.Decode(string(body), &f)
	bad := func(why string) error {
		return newErr(ExitUsage, "usage", p+" is not valid").why("%s", why).
			fix("", `write it as [aliases] then lines like mine = "hf:org/name"`)
	}
	if err != nil {
		return nil, bad(err.Error())
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, bad("unknown key " + und[0].String())
	}
	out := map[string]Ref{}
	for name, s := range f.Aliases {
		if !IsAliasName(name) || reservedAliases[name] {
			return nil, bad("nickname " + quote(name) + ": lowercase letters, digits, '.', '_' and '-' only, and not a command of lmget")
		}
		ref, err := ParseRef(s)
		if err != nil {
			return nil, bad("nickname " + name + ": " + AsError(err).Msg)
		}
		out[name] = ref
	}
	return out, nil
}

// AliasNames lists the nicknames known here, the user's first.
func AliasNames(local map[string]Ref, public *AliasList) []string {
	seen := map[string]bool{}
	var names []string
	for n := range local {
		seen[n] = true
		names = append(names, n)
	}
	if public != nil {
		for n := range public.Aliases {
			if !seen[n] {
				names = append(names, n)
			}
		}
	}
	sort.Strings(names)
	return names
}
