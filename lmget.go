// Package lmget resolves open models at their sources, downloads them with
// resumable ranged requests, verifies every byte against the hash the source
// publishes, keeps them in a content-addressed store and exports them to the
// layouts runtimes read (Hugging Face cache, Ollama, plain directory).
//
// The CLI is a thin renderer over Pull: everything it shows, --json included,
// is the same Event stream a library user receives.
package lmget

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultIndexURL is where the project publishes its signed index (the
// signature is at the same URL plus ".minisig"). LMGET_INDEX_URL overrides it.
const DefaultIndexURL = "https://lmget.org/index.json"

// Version is set at build time (-ldflags "-X github.com/fabriziosalmi/lmget.Version=...").
var Version = "dev"

// Options configures a Client. Zero values pick documented defaults.
type Options struct {
	Home       string // store root; default $LMGET_HOME, else ~/.lmget
	HFEndpoint string // default $HF_ENDPOINT, else https://huggingface.co
	HFToken    string // default $HF_TOKEN, else the token file of huggingface_hub
	// OllamaRegistry is the Ollama registry; default https://registry.ollama.ai
	OllamaRegistry string
	// ModelScopeEndpoint is the second origin for Hugging Face files; default
	// https://www.modelscope.ai. NoMirror never asks it.
	ModelScopeEndpoint string
	NoMirror           bool
	// Licenses a model may come under, as the source names them; "none"
	// allows a model that declares none. Default $LMGET_LICENSES (names
	// separated by commas or spaces); empty allows any. See policy.go.
	Licenses []string
	// IndexURL is the signed index (https); default $LMGET_INDEX_URL. Its
	// signature is IndexURL+".minisig". IndexKeys add minisign public keys
	// to the built-in ones; default $LMGET_INDEX_KEY.
	IndexURL string
	// NodePreference lists index node hosts, preferred first (the CLI orders
	// them by the speed its history measured); nodes not listed keep the
	// index's order, after the listed ones.
	NodePreference []string
	// IndexMirrors serve the same signed index when IndexURL cannot; default
	// DefaultIndexMirrors with the default IndexURL, none with another one.
	IndexMirrors []string
	IndexKeys    []string
	// RootKeys add minisign public keys to the built-in root keys, which
	// sign keys.json (keys.go); default $LMGET_ROOT_KEY. KeysURL is the key
	// list; default keys.json next to IndexURL.
	RootKeys []string
	KeysURL  string
	// Origins are blob servers asked first for files with a known sha256,
	// at <origin>/blobs/sha256/<sha256>; default $LMGET_ORIGINS (URLs
	// separated by commas or spaces). See origins.go.
	Origins   []string
	Conns     int   // parallel connections per file; default 8
	ChunkSize int64 // ranged request size; default 16 MiB
	HTTP      *http.Client
	UserAgent string
}

type Client struct {
	opt         Options
	store       *Store
	http        *http.Client
	tokenSource string // where the token came from; never the token itself
	msMu        sync.Mutex
	msFiles     map[string]map[string]msFile // ModelScope listings, per repository
	ociMu       sync.Mutex
	ociTokens   map[string]ociToken // pull tokens, per OCI repository
	nodeMu      sync.Mutex
	nodeDown    map[string]time.Time // index nodes that failed, until when they are skipped
	indexMu     sync.Mutex
	indexTried  bool          // ensureIndex fetched (or tried to fetch) the index in this run
	retryBase   time.Duration // first retry wait of a chunk, doubled each time
	// streamedFiles counts files verified from the hash computed while they
	// downloaded, instead of reading them again (issue #1); tests read it
	streamedFiles atomic.Int64
}

func New(o Options) (*Client, error) {
	if o.Home == "" {
		o.Home = os.Getenv("LMGET_HOME")
	}
	if o.Home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		o.Home = filepath.Join(h, ".lmget")
	}
	if o.HFEndpoint == "" {
		o.HFEndpoint = os.Getenv("HF_ENDPOINT")
	}
	if o.Licenses == nil {
		o.Licenses = ParseLicenses(os.Getenv("LMGET_LICENSES"))
	}
	if o.HFEndpoint == "" {
		o.HFEndpoint = "https://huggingface.co"
	}
	o.HFEndpoint = strings.TrimRight(o.HFEndpoint, "/")
	if o.OllamaRegistry == "" {
		o.OllamaRegistry = "https://registry.ollama.ai"
	}
	o.OllamaRegistry = strings.TrimRight(o.OllamaRegistry, "/")
	if o.IndexURL == "" {
		o.IndexURL = os.Getenv("LMGET_INDEX_URL")
	}
	if o.IndexURL == "" {
		o.IndexURL = DefaultIndexURL
	}
	if o.IndexMirrors == nil && o.IndexURL == DefaultIndexURL {
		o.IndexMirrors = DefaultIndexMirrors
	}
	if len(o.IndexKeys) == 0 && os.Getenv("LMGET_INDEX_KEY") != "" {
		o.IndexKeys = []string{os.Getenv("LMGET_INDEX_KEY")}
	}
	if len(o.RootKeys) == 0 && os.Getenv("LMGET_ROOT_KEY") != "" {
		o.RootKeys = []string{os.Getenv("LMGET_ROOT_KEY")}
	}
	if o.ModelScopeEndpoint == "" {
		o.ModelScopeEndpoint = "https://www.modelscope.ai"
	}
	o.ModelScopeEndpoint = strings.TrimRight(o.ModelScopeEndpoint, "/")
	if len(o.Origins) == 0 && os.Getenv("LMGET_ORIGINS") != "" {
		o.Origins = []string{os.Getenv("LMGET_ORIGINS")}
	}
	origins, err := parseOrigins(o.Origins)
	if err != nil {
		return nil, err
	}
	o.Origins = origins
	tokenSource := "option"
	if o.HFToken == "" {
		o.HFToken, tokenSource = hfToken()
	}
	if o.Conns <= 0 {
		o.Conns = 8
	}
	if o.ChunkSize <= 0 {
		o.ChunkSize = 16 << 20
	}
	if o.UserAgent == "" {
		o.UserAgent = "lmget/" + Version
	}
	hc := o.HTTP
	if hc != nil && hc.CheckRedirect == nil { // a copy: never change the caller's client
		cp := *hc
		cp.CheckRedirect = noDowngrade
		hc = &cp
	}
	if hc == nil {
		hc = &http.Client{CheckRedirect: noDowngrade, Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		}}
	}
	st, err := OpenStore(o.Home)
	if err != nil {
		return nil, newErr(ExitDisk, "disk", "cannot open the store at "+o.Home).wrap(err)
	}
	return &Client{opt: o, store: st, http: hc, tokenSource: tokenSource, retryBase: 500 * time.Millisecond}, nil
}

func (c *Client) Store() *Store { return c.store }

// hfToken follows huggingface_hub: $HF_TOKEN, else the one token file
// huggingface_hub reads: $HF_TOKEN_PATH, else $HF_HOME/token, where HF_HOME
// defaults to $XDG_CACHE_HOME/huggingface, then ~/.cache/huggingface. One
// file, not a chain: a project or a container that points HF_HOME elsewhere
// must not fall back to the user's personal token. It returns the token and
// where it was found; the value is never logged or printed.
func hfToken() (string, string) {
	if t := strings.TrimSpace(os.Getenv("HF_TOKEN")); t != "" {
		return t, "HF_TOKEN"
	}
	p := os.Getenv("HF_TOKEN_PATH")
	if p == "" {
		home := os.Getenv("HF_HOME")
		if home == "" {
			cache := os.Getenv("XDG_CACHE_HOME")
			if cache == "" {
				h, err := os.UserHomeDir()
				if err != nil {
					return "", ""
				}
				cache = filepath.Join(h, ".cache")
			}
			home = filepath.Join(cache, "huggingface")
		}
		p = filepath.Join(home, "token")
	}
	b, err := os.ReadFile(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", ""
	case err != nil:
		return "", "unreadable:" + p // exists but cannot be read: say so, do not pretend there is none
	}
	if t := strings.TrimSpace(string(b)); t != "" {
		return t, p
	}
	return "", ""
}

// noDowngrade refuses a redirect from https to plain http, and bounds the
// chain like net/http does.
// MaxFileSize bounds a size reported by a source (16 TiB), so arithmetic on
// sizes cannot overflow.
const MaxFileSize = int64(1) << 44

func noDowngrade(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	// net/http keeps Authorization on a redirect to the same host name on
	// another port, and to subdomains: the token stays with the exact host.
	if len(via) > 0 && req.URL.Host != via[0].URL.Host {
		req.Header.Del("Authorization")
	}
	if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return newErr(ExitNetwork, "network", "refused a redirect from https to "+req.URL.Scheme+"://"+req.URL.Host).
			why("a downgrade would let anyone on the path change the bytes")
	}
	return nil
}
