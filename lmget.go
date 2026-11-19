// Package lmget resolves open models at their sources, downloads them with
// resumable ranged requests, verifies every byte against the hash the source
// publishes, keeps them in a content-addressed store and exports them to the
// layouts runtimes read (Hugging Face cache, Ollama, plain directory).
//
// The CLI is a thin renderer over Pull: everything it shows, --json included,
// is the same Event stream a library user receives.
package lmget

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
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
	// LAN looks for lmget servers on the local network by mDNS, once per
	// client, and asks them first, as origins (lan.go). Default: on when
	// $LMGET_LAN is on, 1 or true.
	LAN bool
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
	lanOnce     sync.Once
	lanFound    []string // lmget servers found on the local network (lan.go)
	opt         Options
	store       *Store
	http        *http.Client
	tokenSource string    // where the token came from; never the token itself
	s3          *s3Config // endpoint and keys for s3:// origins; nil without one
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
	switch strings.ToLower(os.Getenv("LMGET_LAN")) {
	case "on", "1", "true":
		o.LAN = true
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
			Proxy:                 publicOnlyProxy,
			DialContext:           publicOnlyDial(&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}),
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
	c := &Client{opt: o, store: st, http: hc, tokenSource: tokenSource, retryBase: 500 * time.Millisecond}
	for _, or := range o.Origins {
		if strings.HasPrefix(or, "s3://") {
			if c.s3, err = s3FromEnv(); err != nil {
				return nil, err
			}
			break
		}
	}
	return c, nil
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
	// an origin's redirects stay on its host and port: anyone's server must
	// not point lmget at this machine's own services
	if len(via) > 0 && isOriginRequest(req.Context()) && req.URL.Host != via[0].URL.Host {
		return newErr(ExitNetwork, "network", "refused an origin's redirect to "+req.URL.Host).
			why("an origin may redirect within its own host only")
	}
	// net/http keeps Authorization on a redirect to the same host name on
	// another port, and to subdomains: the token stays with the exact host.
	// a signed request (an s3:// origin) goes to its endpoint only: the
	// session token is a header too, and would follow
	if len(via) > 0 && strings.HasPrefix(via[0].Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		return newErr(ExitNetwork, "network", "refused a redirect of a signed request to "+req.URL.Host).
			why("the bucket's keys sign requests for its endpoint only")
	}
	if len(via) > 0 && req.URL.Host != via[0].URL.Host {
		req.Header.Del("Authorization")
	}
	if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return newErr(ExitNetwork, "network", "refused a redirect from https to "+req.URL.Scheme+"://"+req.URL.Host).
			why("a downgrade would let anyone on the path change the bytes")
	}
	return nil
}

// publicKey marks a context whose requests may reach public addresses only:
// lmget mcp's URL and OCI refs, whose host an agent chose.
type publicKey struct{}

// PublicOnly returns a context whose downloads connect to public internet
// addresses only: never this machine, its networks, link-local (the cloud
// metadata address) or carrier-grade NAT. The address is checked when the
// connection is made, so a name that resolves elsewhere later (DNS
// rebinding) cannot get around it. Clients built with their own
// Options.HTTP do not check it.
func PublicOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, publicKey{}, true)
}

func isPublicOnly(ctx context.Context) bool { v, _ := ctx.Value(publicKey{}).(bool); return v }

// envProxy is the environment's proxy (a variable for tests: net/http reads
// the environment once per process).
var envProxy = http.ProxyFromEnvironment

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// publicIP reports a global unicast address outside the private, shared
// and loopback ranges.
func publicIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !cgnat.Contains(ip)
}

func publicOnlyDial(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !isPublicOnly(ctx) {
			return d.DialContext(ctx, network, addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errors.New("no address for " + host)
		}
		for _, ip := range ips {
			if !publicIP(ip.IP) {
				return nil, newErr(ExitUsage, "usage", host+" is not a public address ("+ip.IP.String()+")").
					why("lmget mcp fetches URLs and OCI images from public hosts only: an agent must not reach this machine or its networks")
			}
		}
		return d.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port)) // the address checked
	}
}

// publicOnlyProxy is the environment's proxy, except for a public-only
// request: through a proxy the address it reaches cannot be checked.
func publicOnlyProxy(req *http.Request) (*url.URL, error) {
	u, err := envProxy(req)
	if err == nil && u != nil && isPublicOnly(req.Context()) {
		return nil, newErr(ExitUsage, "usage", "lmget mcp does not fetch a URL or an OCI image through a proxy").
			why("through a proxy, lmget cannot check that the host is public")
	}
	return u, err
}
