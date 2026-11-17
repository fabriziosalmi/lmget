package lmget

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
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

// IndexKeys are the built-in keys that sign the project index while no valid
// key list (keys.go) names others: the project key, key number
// 21C7BDA090E969F4 (lmget-index.pub). The root key that signs key lists is in
// RootKeys. No secret key is ever in the repository.
var IndexKeys = []string{
	"RWT0aemQoL3HIZb5q4xhyeAUFBAtnyetjUH2KFXr0H235qQdXzZhtZrB",
}

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
	// Revoked are blobs the project withdrew (malicious or mislabeled
	// content): lmget refuses to download or export them from any origin,
	// and doctor reports them in the store. No model may list one.
	Revoked  []IndexRevoked `json:"revoked,omitempty"`
	signedBy string         // key number of the key whose signature verified
}

// IndexRevoked is one withdrawn blob and why.
type IndexRevoked struct {
	SHA256 string `json:"sha256"`
	Reason string `json:"reason"`
}

// RevokedBlobs maps each revoked sha256 to its reason.
func (ix *Index) RevokedBlobs() map[string]string {
	out := make(map[string]string, len(ix.Revoked))
	for _, r := range ix.Revoked {
		out[r.SHA256] = r.Reason
	}
	return out
}

// SignedBy is the key number of the key that signed this index.
func (ix *Index) SignedBy() string { return ix.signedBy }

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

// MaxIndexValidity bounds how long one signed index stays valid: if the
// signing key were ever stolen, an index signed with it would stop being
// accepted within this time, whatever expiry it claims.
const MaxIndexValidity = 60 * 24 * time.Hour

// indexClockSkew is how far ahead of this machine's clock an index may be
// dated, for clocks that are slightly off.
const indexClockSkew = time.Hour

func (ix *Index) validate() error {
	switch {
	case ix.Version != indexVersion:
		return fmt.Errorf("version %d is not supported", ix.Version)
	case ix.Sequence <= 0:
		return errors.New("no sequence number")
	case !ix.Expires.After(ix.Issued):
		return errors.New("expires before it was issued")
	case ix.Expires.Sub(ix.Issued) > MaxIndexValidity:
		return fmt.Errorf("valid for %s, more than the %d days allowed", ix.Expires.Sub(ix.Issued).Round(time.Hour), int(MaxIndexValidity.Hours()/24))
	case len(ix.Nodes) > 32 || len(ix.Models) > 10000 || len(ix.Revoked) > 10000:
		return errors.New("too many nodes, models or revoked blobs")
	}
	revoked := map[string]bool{}
	for _, r := range ix.Revoked {
		if !hex64.MatchString(r.SHA256) || revoked[r.SHA256] || r.Reason == "" || len(r.Reason) > 300 || printable(r.Reason) != r.Reason {
			return fmt.Errorf("revoked blob %s: bad sha256 or reason, or listed twice", quote(r.SHA256))
		}
		revoked[r.SHA256] = true
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
		// the builder signs Hugging Face models only (url, oci and ollama
		// plans carry no license): an entry of another source would bypass
		// the checks their resolvers make (a pinned url#sha256, a manifest
		// whose digest is the commit), so it is refused
		if ref.Source != "hf" {
			return fmt.Errorf("model %s: the index lists Hugging Face models only", quote(m.Ref))
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
			if revoked[f.SHA256] {
				return fmt.Errorf("model %s: file %s is a revoked blob", quote(m.Ref), quote(f.Path))
			}
			paths[f.Path] = true
		}
	}
	return nil
}

// openIndex verifies a signed index with a key trusted now (keys.go) and
// validates it.
func (c *Client) openIndex(data []byte, sig string, now time.Time) (*Index, error) {
	return c.openIndexAt(data, sig, now, false)
}

// openIndexAt is openIndex; with expiredOK an index past its expiry date is
// returned too (its signature, keys and dates still checked), for what stays
// true after it expires: its revocations.
func (c *Client) openIndexAt(data []byte, sig string, now time.Time, expiredOK bool) (*Index, error) {
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
		return nil, newErr(ExitVerify, "verify", "the index signature is not valid").why("%s", c.whyNotTrusted(sig, keys))
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
	// the 60-day cap is measured against the clock, not only between the
	// two signed dates: an index "issued" in the future would otherwise stay
	// valid for as long as its signer chose
	if ix.Issued.After(now.Add(indexClockSkew)) || ix.Expires.Sub(now) > MaxIndexValidity+indexClockSkew {
		return nil, newErr(ExitVerify, "verify", "the index is dated in the future").
			why("issued %s, expires %s: an index is valid for at most %d days from now",
				ix.Issued.UTC().Format(time.RFC3339), ix.Expires.UTC().Format(time.RFC3339), int(MaxIndexValidity.Hours()/24)).
			fix("", "check this machine's clock")
	}
	if !now.Before(ix.Expires) && !expiredOK {
		return nil, newErr(ExitVerify, "verify", "the index expired on "+ix.Expires.UTC().Format(time.DateOnly)).
			why("an expired index may describe models that were withdrawn since").
			fix("lmget index", "fetch the current one")
	}
	ix.signedBy = signedBy
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
	if len(c.rootKeys()) == 0 && len(IndexKeys)+len(c.opt.IndexKeys) == 0 { // nothing could verify it: do not even fetch it
		return nil, newErr(ExitUsage, "usage", "no index key is configured").
			why("the project index is not published yet")
	}
	u := c.opt.IndexURL
	if pu, err := url.Parse(u); u == "" || err != nil || pu.Scheme != "https" {
		return nil, newErr(ExitUsage, "usage", "no index URL is configured").
			why("LMGET_INDEX_URL must be an https URL")
	}
	// the key list first: it says which keys sign the index
	if err := c.UpdateKeys(ctx); err != nil {
		return nil, err
	}
	if _, _, err := c.trustedIndexKeys(time.Now()); err != nil {
		return nil, err
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
	sum := sha256.Sum256(data)
	if seq, had, ok := c.highestIndex(); ok {
		switch {
		case ix.Sequence < seq:
			return nil, newErr(ExitVerify, "verify", fmt.Sprintf("the index served is older than one already accepted (%d < %d)", ix.Sequence, seq)).
				why("an old index could hide a withdrawn model or an updated commit")
		case ix.Sequence == seq && had != hex.EncodeToString(sum[:]):
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
	// last: the highest accepted index, kept apart from the copy that expires
	if err := writeFileAtomic(c.highestPath(), []byte(fmt.Sprintf("%d %x\n", ix.Sequence, sum))); err != nil {
		return nil, diskErr(err)
	}
	return ix, nil
}

func (c *Client) highestPath() string {
	return filepath.Join(c.store.Root(), "signed-index", "highest")
}

// highestIndex returns the sequence and sha256 of the highest index this
// store accepted. Unlike the cached index it does not expire and needs no
// key, so an older index cannot come back once the newer one has expired or
// its key was rotated. A store from before this record falls back to the
// cached index when that still verifies.
func (c *Client) highestIndex() (int64, string, bool) {
	var seq int64
	var sum string
	ok := false
	if b, err := os.ReadFile(c.highestPath()); err == nil {
		if n, _ := fmt.Sscanf(string(b), "%d %64s", &seq, &sum); n == 2 && hex64.MatchString(sum) {
			ok = true
		}
	}
	// the cached index counts too: the record is written last, so a crash
	// between the data and the record leaves it one index behind
	if old, err := c.Index(); err == nil && (!ok || old.Sequence > seq) {
		p, _ := c.indexPaths()
		if cur, err := os.ReadFile(p); err == nil {
			s := sha256.Sum256(cur)
			return old.Sequence, hex.EncodeToString(s[:]), true
		}
	}
	return seq, sum, ok
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
	if resp.StatusCode == http.StatusNotFound {
		return nil, newErr(ExitNotFound, "not_found", fmt.Sprintf("%s has no %s", hostOf(u), quote(path.Base(u))))
	}
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
