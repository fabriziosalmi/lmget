package lmget

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/blake2b"
)

// The signed index lists models the project mirrors: for each, the source
// reference, the commit and every file with its size and sha256, plus the
// nodes that serve those files by sha256. It is signed with minisign (the
// official tool signs it; lmget only verifies), so the file list of an
// indexed model can be trusted even when its source is unreachable.
//
// A sequence number refuses replays of an older index, and an expiry date
// refuses an index frozen in time. The cached copy is verified again on
// every read.

// IndexKeys are the minisign public keys lmget trusts for the project index:
// the project key, key number 21C7BDA090E969F4 (lmget-index.pub in the
// repository). Its secret key is kept in Infisical and never on disk here.
var IndexKeys = []string{"RWT0aemQoL3HIZb5q4xhyeAUFBAtnyetjUH2KFXr0H235qQdXzZhtZrB"}

const (
	indexMaxBytes = 32 << 20
	sigMaxBytes   = 4 << 10
	indexVersion  = 1
)

// MinisignKey is a minisign Ed25519 public key.
type MinisignKey struct {
	ID [8]byte
	PK ed25519.PublicKey
}

// ParseMinisignKey reads a public key in minisign's format: the base64 line,
// optionally after an "untrusted comment:" line.
func ParseMinisignKey(s string) (MinisignKey, error) {
	var line string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "untrusted comment:") {
			line = l
		}
	}
	b, err := base64.StdEncoding.DecodeString(line)
	if err != nil || len(b) != 42 || string(b[:2]) != "Ed" {
		return MinisignKey{}, errors.New("not a minisign public key")
	}
	var k MinisignKey
	copy(k.ID[:], b[2:10])
	k.PK = ed25519.PublicKey(append([]byte(nil), b[10:]...))
	return k, nil
}

// verifyMinisign checks a minisign signature (legacy "Ed" or prehashed "ED")
// of data, including the signature over the trusted comment, and returns the
// trusted comment.
func verifyMinisign(k MinisignKey, data []byte, sigText string) (string, error) {
	lines := strings.Split(strings.ReplaceAll(strings.TrimRight(sigText, "\r\n"), "\r\n", "\n"), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "untrusted comment:") || !strings.HasPrefix(lines[2], "trusted comment: ") {
		return "", errors.New("not a minisign signature")
	}
	sig, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil || len(sig) != 74 {
		return "", errors.New("not a minisign signature")
	}
	global, err := base64.StdEncoding.DecodeString(lines[3])
	if err != nil || len(global) != ed25519.SignatureSize {
		return "", errors.New("not a minisign signature")
	}
	if !bytes.Equal(sig[2:10], k.ID[:]) {
		return "", errors.New("signed with another key")
	}
	msg := data
	switch string(sig[:2]) {
	case "Ed":
	case "ED":
		h := blake2b.Sum512(data)
		msg = h[:]
	default:
		return "", errors.New("unknown minisign algorithm")
	}
	if !ed25519.Verify(k.PK, msg, sig[10:]) {
		return "", errors.New("the signature does not match the data")
	}
	trusted := strings.TrimPrefix(lines[2], "trusted comment: ")
	if !ed25519.Verify(k.PK, append(append([]byte(nil), sig[10:]...), trusted...), global) {
		return "", errors.New("the trusted comment was changed")
	}
	return trusted, nil
}

// Index is the signed list of mirrored models.
type Index struct {
	Version  int          `json:"version"`
	Sequence int64        `json:"sequence"`
	Issued   time.Time    `json:"issued"`
	Expires  time.Time    `json:"expires"`
	Nodes    []string     `json:"nodes"`
	Models   []IndexModel `json:"models"`
}

// IndexModel pins one model, as a lock entry does.
type IndexModel struct {
	Ref     string      `json:"ref"`
	Commit  string      `json:"commit"`
	License string      `json:"license"`
	Files   []IndexFile `json:"files"`
}

// IndexFile is a locked file plus the BitTorrent v2 infohash of its blob
// (BlobTorrent), under which the nodes seed it.
type IndexFile struct {
	LockFile
	BTIH2 string `json:"btih2,omitempty"`
}

// LockFiles returns the files without their torrent identity.
func (m IndexModel) LockFiles() []LockFile {
	out := make([]LockFile, len(m.Files))
	for i, f := range m.Files {
		out[i] = f.LockFile
	}
	return out
}

// Find returns the entry of ref at commit, or nil.
func (ix *Index) Find(ref Ref, commit string) *IndexModel {
	for i := range ix.Models {
		if r, err := ParseRef(ix.Models[i].Ref); err == nil && r.String() == ref.String() && ix.Models[i].Commit == commit {
			return &ix.Models[i]
		}
	}
	return nil
}

func (ix *Index) validate() error {
	switch {
	case ix.Version != indexVersion:
		return fmt.Errorf("version %d is not supported", ix.Version)
	case ix.Sequence <= 0:
		return errors.New("no sequence number")
	case !ix.Expires.After(ix.Issued):
		return errors.New("expires before it was issued")
	case len(ix.Nodes) > 32 || len(ix.Models) > 10000:
		return errors.New("too many nodes or models")
	}
	for _, n := range ix.Nodes {
		u, err := url.Parse(n)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("node %s is not a plain https URL", quote(n))
		}
	}
	seen := map[string]bool{}
	for _, m := range ix.Models {
		ref, err := ParseRef(m.Ref)
		if err != nil || ref.SHA256 != "" || ref.File != "" || Lockable(ref) != nil {
			return fmt.Errorf("model %s: not a canonical reference", quote(m.Ref))
		}
		key := ref.String() + "@" + m.Commit
		if seen[key] || !validCommit(ref, m.Commit) || printable(m.License) != m.License || len(m.Files) == 0 {
			return fmt.Errorf("model %s: bad commit, license, files, or listed twice", quote(m.Ref))
		}
		seen[key] = true
		paths := map[string]bool{}
		for _, f := range m.Files {
			if !isLocalSlash(f.Path) || paths[f.Path] || !hex64.MatchString(f.SHA256) || f.Size < 0 || f.Size > MaxFileSize ||
				(f.GitSHA1 != "" && !hex40.MatchString(f.GitSHA1)) || (f.BTIH2 != "" && !hex64.MatchString(f.BTIH2)) {
				return fmt.Errorf("model %s: bad file %s", quote(m.Ref), quote(f.Path))
			}
			paths[f.Path] = true
		}
	}
	return nil
}

func (c *Client) indexKeys() ([]MinisignKey, error) {
	var keys []MinisignKey
	for _, s := range append(append([]string(nil), IndexKeys...), c.opt.IndexKeys...) {
		k, err := ParseMinisignKey(s)
		if err != nil {
			return nil, newErr(ExitUsage, "usage", "an index key is not a minisign public key")
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, newErr(ExitUsage, "usage", "no index key is configured").
			why("the project index is not published yet")
	}
	return keys, nil
}

// openIndex verifies a signed index with any trusted key and validates it.
func (c *Client) openIndex(data []byte, sig string, now time.Time) (*Index, error) {
	keys, err := c.indexKeys()
	if err != nil {
		return nil, err
	}
	ok := false
	for _, k := range keys {
		if _, err := verifyMinisign(k, data, sig); err == nil {
			ok = true
			break
		}
	}
	if !ok {
		return nil, newErr(ExitVerify, "verify", "the index signature is not valid").
			why("no trusted key signed these bytes")
	}
	var ix Index
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ix); err != nil {
		return nil, newErr(ExitVerify, "verify", "the signed index is not valid JSON").wrap(err)
	}
	if err := ix.validate(); err != nil {
		return nil, newErr(ExitVerify, "verify", "the signed index is not valid").why("%s", err.Error())
	}
	if !now.Before(ix.Expires) {
		return nil, newErr(ExitVerify, "verify", "the index expired on "+ix.Expires.UTC().Format(time.DateOnly)).
			why("an expired index may describe models that were withdrawn since").
			fix("lmget index", "fetch the current one")
	}
	return &ix, nil
}

func (c *Client) indexPaths() (string, string) {
	d := filepath.Join(c.store.Root(), "signed-index")
	return filepath.Join(d, "index.json"), filepath.Join(d, "index.json.minisig")
}

// Index returns the cached signed index, verified again.
func (c *Client) Index() (*Index, error) {
	p, ps := c.indexPaths()
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, newErr(ExitUsage, "usage", "no index has been fetched yet").fix("lmget index", "fetch it")
	}
	sig, err := os.ReadFile(ps)
	if err != nil {
		return nil, newErr(ExitUsage, "usage", "the cached index has no signature").fix("lmget index", "fetch it again")
	}
	return c.openIndex(data, string(sig), time.Now())
}

// UpdateIndex fetches the index and its signature, verifies them, refuses a
// sequence older than the cached one, and caches them.
func (c *Client) UpdateIndex(ctx context.Context) (*Index, error) {
	if _, err := c.indexKeys(); err != nil { // nothing could verify it: do not even fetch it
		return nil, err
	}
	u := c.opt.IndexURL
	if pu, err := url.Parse(u); u == "" || err != nil || pu.Scheme != "https" {
		return nil, newErr(ExitUsage, "usage", "no index URL is configured").
			why("LMGET_INDEX_URL must be an https URL")
	}
	data, err := c.fetchSmall(ctx, u, indexMaxBytes)
	if err != nil {
		return nil, err
	}
	sig, err := c.fetchSmall(ctx, u+".minisig", sigMaxBytes)
	if err != nil {
		return nil, err
	}
	ix, err := c.openIndex(data, string(sig), time.Now())
	if err != nil {
		return nil, err
	}
	if old, err := c.Index(); err == nil {
		p, _ := c.indexPaths()
		cur, _ := os.ReadFile(p)
		switch {
		case ix.Sequence < old.Sequence:
			return nil, newErr(ExitVerify, "verify", fmt.Sprintf("the index served is older than the cached one (%d < %d)", ix.Sequence, old.Sequence)).
				why("an old index could hide a withdrawn model or an updated commit")
		case ix.Sequence == old.Sequence && !bytes.Equal(cur, data):
			return nil, newErr(ExitVerify, "verify", "two different indexes carry the same sequence number")
		}
	}
	p, ps := c.indexPaths()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, diskErr(err)
	}
	// signature first, then the data: a reader never pairs new data with an old signature
	if err := writeFileAtomic(ps, sig); err != nil {
		return nil, diskErr(err)
	}
	if err := writeFileAtomic(p, data); err != nil {
		return nil, diskErr(err)
	}
	return ix, nil
}

func (c *Client) fetchSmall(ctx context.Context, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, newErr(ExitNetwork, "network", "cannot reach "+hostOf(u)).wrap(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, newErr(ExitNetwork, "network", fmt.Sprintf("%s answered %s", hostOf(u), printable(resp.Status)))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, newErr(ExitNetwork, "network", "the download from "+hostOf(u)+" did not complete").wrap(err)
	}
	if int64(len(b)) > limit {
		return nil, newErr(ExitFailure, "source", fmt.Sprintf("%s is larger than %d bytes", u, limit))
	}
	return b, nil
}
