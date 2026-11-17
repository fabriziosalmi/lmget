package lmget

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Key rotation. Two kinds of minisign keys:
//
//   - the root key (RootKeys) stays offline and signs one thing: keys.json,
//     the list of the keys that may sign the index, and of revoked keys;
//   - index keys sign the index. IndexKeys, built in, are the ones trusted
//     until a key list says otherwise.
//
// keys.json sits next to the index (https://lmget.org/keys.json). While a
// valid one is known, the index keys are exactly the ones it lists: a stolen
// index key is replaced by publishing a new list, without a new release.
// Without one (none published, or the cached one expired) lmget falls back to
// the built-in index keys. Revocations and the highest accepted list are kept
// in records that never expire, so a revoked key is never trusted again by a
// store that saw it revoked, and an older list cannot come back.

// RootKeys are the built-in root keys: the backup key, key number
// 736BC89C91E98921 (lmget-index-backup.pub), kept offline apart from the
// project key. They sign keys.json only; an index signed by a root key is
// refused, so the root key is never needed online.
var RootKeys = []string{
	"RWQhiemRnMhrcwx8Kp7d8KmyCw5WZzE0PGWIMgXsWWInXEiTuwx5BBBr",
}

// RevokedKeys are key numbers this release refuses whatever a key list says.
var RevokedKeys = []string{}

// MaxKeyListValidity bounds how long one signed key list stays valid. The
// root key signs a new one at least this often.
const MaxKeyListValidity = 400 * 24 * time.Hour

const (
	keyListVersion  = 1
	keyListMaxBytes = 64 << 10
	maxListedKeys   = 8
	maxRevokedKeys  = 64
)

var keyNumberRe = regexp.MustCompile(`^[0-9A-F]{16}$`)

// KeyList is keys.json: which keys sign the index, and which are revoked.
type KeyList struct {
	Version   int       `json:"version"`
	Sequence  int64     `json:"sequence"`
	Issued    time.Time `json:"issued"`
	Expires   time.Time `json:"expires"`
	IndexKeys []string  `json:"index_keys"` // minisign public keys (base64 lines)
	Revoked   []string  `json:"revoked"`    // key numbers, as minisign prints them
}

// KeyNumber is the key number minisign prints (the key id as a little-endian
// uint64, in hex).
func KeyNumber(k MinisignKey) string {
	var b strings.Builder
	for i := 7; i >= 0; i-- {
		fmt.Fprintf(&b, "%02X", k.ID[i])
	}
	return b.String()
}

// CheckKeyList validates a key list against the root keys it will be signed
// with: what lmget-index writes, lmget accepts.
func CheckKeyList(kl *KeyList, roots []string) error {
	switch {
	case kl.Version != keyListVersion:
		return fmt.Errorf("version %d is not supported", kl.Version)
	case kl.Sequence <= 0:
		return errors.New("no sequence number")
	case !kl.Expires.After(kl.Issued):
		return errors.New("expires before it was issued")
	case kl.Expires.Sub(kl.Issued) > MaxKeyListValidity:
		return fmt.Errorf("valid for %s, more than the %d days allowed", kl.Expires.Sub(kl.Issued).Round(time.Hour), int(MaxKeyListValidity.Hours()/24))
	case len(kl.IndexKeys) == 0 || len(kl.IndexKeys) > maxListedKeys:
		return fmt.Errorf("lists %d index keys, from 1 to %d are allowed", len(kl.IndexKeys), maxListedKeys)
	case len(kl.Revoked) > maxRevokedKeys:
		return fmt.Errorf("revokes %d keys, at most %d", len(kl.Revoked), maxRevokedKeys)
	}
	root := map[string]bool{}
	var rootPKs []MinisignKey
	for _, s := range roots {
		if k, err := ParseMinisignKey(s); err == nil {
			root[KeyNumber(k)] = true
			rootPKs = append(rootPKs, k)
		}
	}
	revoked := map[string]bool{}
	for _, n := range kl.Revoked {
		switch {
		case !keyNumberRe.MatchString(n):
			return fmt.Errorf("revoked key %s is not a key number (16 hex digits, upper case)", quote(n))
		case root[n]:
			return fmt.Errorf("revoked key %s is a root key: a root key is replaced by a release", n)
		}
		revoked[n] = true
	}
	seen := map[string]bool{}
	for _, s := range kl.IndexKeys {
		k, err := ParseMinisignKey(s)
		if err != nil {
			return fmt.Errorf("index key %s is not a minisign public key", quote(s))
		}
		n := KeyNumber(k)
		switch {
		case seen[n]:
			return fmt.Errorf("index key %s is listed twice", n)
		case revoked[n]:
			return fmt.Errorf("index key %s is listed and revoked", n)
		case root[n] || isRootPK(k, rootPKs):
			return fmt.Errorf("index key %s is a root key: the root key must stay offline", n)
		}
		seen[n] = true
	}
	return nil
}

func (c *Client) rootKeys() []MinisignKey {
	var out []MinisignKey
	for _, s := range append(append([]string(nil), RootKeys...), c.opt.RootKeys...) {
		if k, err := ParseMinisignKey(s); err == nil {
			out = append(out, k)
		}
	}
	return out
}

func (c *Client) rootKeyStrings() []string {
	return append(append([]string(nil), RootKeys...), c.opt.RootKeys...)
}

// openKeyList verifies keys.json with a root key and validates it. An
// expired list is returned with expired set: it no longer names the index
// keys, but its revocations still count.
func (c *Client) openKeyList(data []byte, sig string, now time.Time) (kl *KeyList, expired bool, err error) {
	ok := false
	for _, k := range c.rootKeys() {
		if _, err := verifyMinisign(k, data, sig); err == nil {
			ok = true
			break
		}
	}
	if !ok {
		return nil, false, newErr(ExitVerify, "verify", "the key list signature is not valid").
			why("no root key signed these bytes")
	}
	kl = &KeyList{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(kl); err != nil {
		return nil, false, newErr(ExitVerify, "verify", "the signed key list is not valid JSON").wrap(err)
	}
	if err := CheckKeyList(kl, c.rootKeyStrings()); err != nil {
		return nil, false, newErr(ExitVerify, "verify", "the signed key list is not valid").why("%s", err.Error())
	}
	if kl.Issued.After(now.Add(indexClockSkew)) || kl.Expires.Sub(now) > MaxKeyListValidity+indexClockSkew {
		return nil, false, newErr(ExitVerify, "verify", "the key list is dated in the future").
			why("issued %s, expires %s", kl.Issued.UTC().Format(time.RFC3339), kl.Expires.UTC().Format(time.RFC3339)).
			fix("", "check this machine's clock")
	}
	return kl, !now.Before(kl.Expires), nil
}

func (c *Client) keyPaths() (data, sig, highest, revoked string) {
	d := filepath.Join(c.store.Root(), "signed-index")
	return filepath.Join(d, "keys.json"), filepath.Join(d, "keys.json.minisig"),
		filepath.Join(d, "keys-highest"), filepath.Join(d, "revoked-keys")
}

// cachedKeyList returns the cached key list when it still verifies and has
// not expired.
func (c *Client) cachedKeyList(now time.Time) *KeyList {
	p, ps, _, _ := c.keyPaths()
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	sig, err := os.ReadFile(ps)
	if err != nil {
		return nil
	}
	kl, expired, err := c.openKeyList(data, string(sig), now)
	if err != nil || expired {
		return nil
	}
	if seq, ok := c.keysHighest(); ok && kl.Sequence < seq {
		return nil // a newer list was accepted since: this one is not in force
	}
	return kl
}

// keysHighest is the sequence of the highest key list this store accepted.
func (c *Client) keysHighest() (int64, bool) {
	_, _, ph, _ := c.keyPaths()
	b, err := os.ReadFile(ph)
	if err != nil {
		return 0, false
	}
	var seq int64
	var sum string
	if n, _ := fmt.Sscanf(string(b), "%d %64s", &seq, &sum); n != 2 {
		return 0, true // a record that cannot be read still says a list was accepted
	}
	return seq, true
}

// isRootPK reports a key whose public key is a root key's, whatever key
// number it carries.
func isRootPK(k MinisignKey, roots []MinisignKey) bool {
	for _, r := range roots {
		if bytes.Equal(k.PK, r.PK) {
			return true
		}
	}
	return false
}

// revokedKeys is every key number this store has seen revoked, plus the
// ones built into this release.
func (c *Client) revokedKeys() map[string]bool {
	out := map[string]bool{}
	for _, n := range RevokedKeys {
		out[n] = true
	}
	_, _, _, pr := c.keyPaths()
	if b, err := os.ReadFile(pr); err == nil {
		for _, n := range strings.Fields(string(b)) {
			if keyNumberRe.MatchString(n) {
				out[n] = true
			}
		}
	}
	return out
}

// KeyState says which keys verify the index, for lmget index and doctor.
type KeyState struct {
	List    *KeyList `json:"list,omitempty"` // the valid key list in force, or nil: the built-in keys
	Keys    []string `json:"keys"`           // key numbers trusted for the index
	Revoked int      `json:"revoked"`        // key numbers this store refuses
}

// IndexKeyState reports the keys that verify the index now.
func (c *Client) IndexKeyState() (KeyState, error) {
	keys, kl, err := c.trustedIndexKeys(time.Now())
	st := KeyState{List: kl, Revoked: len(c.revokedKeys())}
	for _, k := range keys {
		st.Keys = append(st.Keys, KeyNumber(k))
	}
	return st, err
}

// trustedIndexKeys returns the keys that may sign the index: the valid key
// list's, else the built-in ones; plus the keys the user configured; minus
// every revoked key and every root key.
func (c *Client) trustedIndexKeys(now time.Time) ([]MinisignKey, *KeyList, error) {
	kl := c.cachedKeyList(now)
	_, listAccepted := c.keysHighest()
	base := IndexKeys
	switch {
	case kl != nil:
		base = kl.IndexKeys
	case listAccepted:
		// once a key list was accepted, the built-in keys are not trusted
		// again: a key retired by a list stays retired when the list expires
		base = nil
	}
	revoked := c.revokedKeys()
	roots := c.rootKeys()
	root := map[string]bool{}
	for _, k := range roots {
		root[KeyNumber(k)] = true
	}
	var keys []MinisignKey
	dropped := 0
	for _, s := range append(append([]string(nil), base...), c.opt.IndexKeys...) {
		k, err := ParseMinisignKey(s)
		if err != nil {
			return nil, kl, newErr(ExitUsage, "usage", "an index key is not a minisign public key")
		}
		n := KeyNumber(k)
		if revoked[n] || root[n] || isRootPK(k, roots) {
			dropped++
			continue
		}
		if !slices.ContainsFunc(keys, func(o MinisignKey) bool { return o.ID == k.ID }) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		e := newErr(ExitUsage, "usage", "no index key is configured").why("the project index is not published yet")
		if listAccepted && kl == nil {
			e = newErr(ExitVerify, "verify", "no key list is in force").
				why("this store accepted a key list, and the one it holds has expired or was replaced; the built-in keys are not trusted again after a key list").
				fix("lmget index", "fetch the current key list and index")
		} else if dropped > 0 {
			e = newErr(ExitVerify, "verify", "every index key is revoked").
				why("a newer key list names the keys in force").
				fix("lmget index", "fetch the current key list and index")
		}
		return nil, kl, e
	}
	return keys, kl, nil
}

// sigKeyNumber is the key number a minisign signature names, or "".
func sigKeyNumber(sigText string) string {
	lines := strings.Split(strings.TrimSpace(sigText), "\n")
	if len(lines) < 2 {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil || len(b) != 74 {
		return ""
	}
	var k MinisignKey
	copy(k.ID[:], b[2:10])
	return KeyNumber(k)
}

// whyNotTrusted says why no trusted key verified sig: the key it names is
// revoked, is a root key, or is not among the keys in force.
func (c *Client) whyNotTrusted(sig string, keys []MinisignKey) string {
	n := sigKeyNumber(sig)
	var inForce []string
	for _, k := range keys {
		inForce = append(inForce, KeyNumber(k))
		if KeyNumber(k) == n {
			return "key " + n + " is trusted, but its signature does not match these bytes"
		}
	}
	root := false
	for _, k := range c.rootKeys() {
		root = root || KeyNumber(k) == n
	}
	switch {
	case n == "":
		return "the signature is malformed"
	case c.revokedKeys()[n]:
		return "it is signed by key " + n + ", which is revoked"
	case root:
		return "it is signed by the root key " + n + ", which signs key lists only"
	}
	return "it is signed by key " + n + ", not by a key in force (" + strings.Join(inForce, ", ") + ")"
}

// keysURL is keys.json next to the index.
func (c *Client) keysURL() string {
	if c.opt.KeysURL != "" {
		return c.opt.KeysURL
	}
	u, err := url.Parse(c.opt.IndexURL)
	if err != nil {
		return ""
	}
	u.Path = path.Join(path.Dir(u.Path), "keys.json")
	return u.String()
}

// UpdateKeys fetches keys.json and caches it when it is valid and not older
// than the one already accepted. None published is not an error: the
// built-in keys stay in force. A list whose signature or content is wrong is.
func (c *Client) UpdateKeys(ctx context.Context) error {
	if len(c.rootKeys()) == 0 {
		return nil
	}
	u := c.keysURL()
	if pu, err := url.Parse(u); u == "" || err != nil || pu.Scheme != "https" {
		return nil
	}
	data, err := c.fetchSmall(ctx, u, keyListMaxBytes)
	if err != nil {
		if AsError(err).Code == ExitNotFound || AsError(err).Code == ExitNetwork {
			return nil // none published, or unreachable: the cached list or the built-in keys
		}
		return err
	}
	sig, err := c.fetchSmall(ctx, u+".minisig", sigMaxBytes)
	if err != nil {
		return err
	}
	kl, expired, err := c.openKeyList(data, string(sig), time.Now())
	if err != nil {
		return err
	}
	p, ps, ph, pr := c.keyPaths()
	sum := sha256.Sum256(data)
	if b, err := os.ReadFile(ph); err == nil {
		var seq int64
		var had string
		if n, _ := fmt.Sscanf(string(b), "%d %64s", &seq, &had); n == 2 {
			switch {
			case kl.Sequence < seq:
				return newErr(ExitVerify, "verify", fmt.Sprintf("the key list served is older than one already accepted (%d < %d)", kl.Sequence, seq)).
					why("an old key list could bring back a revoked key")
			case kl.Sequence == seq && had != hex.EncodeToString(sum[:]):
				return newErr(ExitVerify, "verify", "two different key lists carry the same sequence number")
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return diskErr(err)
	}
	// revocations first: they hold even if a later write fails
	revoked := c.revokedKeys()
	for _, n := range kl.Revoked {
		revoked[n] = true
	}
	var lines []string
	for n := range revoked {
		lines = append(lines, n)
	}
	slices.Sort(lines)
	if err := writeFileAtomic(pr, []byte(strings.Join(lines, "\n")+"\n")); err != nil {
		return diskErr(err)
	}
	if err := writeFileAtomic(ph, []byte(fmt.Sprintf("%d %x\n", kl.Sequence, sum))); err != nil {
		return diskErr(err)
	}
	if expired { // its revocations count; it names no keys any more
		return nil
	}
	// signature first, then the data: a reader never pairs new data with an old signature
	if err := writeFileAtomic(ps, sig); err != nil {
		return diskErr(err)
	}
	if err := writeFileAtomic(p, data); err != nil {
		return diskErr(err)
	}
	return nil
}
