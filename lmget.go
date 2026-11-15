// Package lmget resolves open models at their sources, downloads them with
// resumable ranged requests, verifies every byte against the hash the source
// publishes, keeps them in a content-addressed store and exports them to the
// layouts runtimes read (Hugging Face cache, plain directory).
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
	"time"
)

// Version is set at build time (-ldflags "-X github.com/fabriziosalmi/lmget.Version=...").
var Version = "dev"

// Options configures a Client. Zero values pick documented defaults.
type Options struct {
	Home       string // store root; default $LMGET_HOME, else ~/.lmget
	HFEndpoint string // default $HF_ENDPOINT, else https://huggingface.co
	HFToken    string // default $HF_TOKEN, else the token file of huggingface_hub
	Conns      int    // parallel connections per file; default 8
	ChunkSize  int64  // ranged request size; default 16 MiB
	HTTP       *http.Client
	UserAgent  string
}

type Client struct {
	opt         Options
	store       *Store
	http        *http.Client
	tokenSource string // where the token came from; never the token itself
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
	if o.HFEndpoint == "" {
		o.HFEndpoint = "https://huggingface.co"
	}
	o.HFEndpoint = strings.TrimRight(o.HFEndpoint, "/")
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
	return &Client{opt: o, store: st, http: hc, tokenSource: tokenSource}, nil
}

func (c *Client) Store() *Store { return c.store }

// hfToken follows huggingface_hub: $HF_TOKEN, $HF_TOKEN_PATH, $HF_HOME/token,
// ~/.cache/huggingface/token. It returns the token and where it was found.
// The value is never logged or printed.
func hfToken() (string, string) {
	if t := strings.TrimSpace(os.Getenv("HF_TOKEN")); t != "" {
		return t, "HF_TOKEN"
	}
	var paths []string
	if p := os.Getenv("HF_TOKEN_PATH"); p != "" {
		paths = append(paths, p)
	}
	if p := os.Getenv("HF_HOME"); p != "" {
		paths = append(paths, filepath.Join(p, "token"))
	}
	if h, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(h, ".cache", "huggingface", "token"))
	}
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			if t := strings.TrimSpace(string(b)); t != "" {
				return t, p
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "unreadable:" + p // exists but cannot be read: say so, do not pretend there is none
		}
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
