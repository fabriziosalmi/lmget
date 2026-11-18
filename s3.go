package lmget

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// A private bucket as an origin: s3://bucket/prefix in LMGET_ORIGINS or
// --origin, holding blobs at <prefix>/blobs/sha256/<sha256>, the layout of a
// store's blobs (aws s3 sync or rclone of <LMGET_HOME>/blobs). Amazon S3,
// Cloudflare R2 and other S3 servers.
//
// The endpoint and the keys come from lmget's own variables only, never from
// AWS_* nor ~/.aws: those belong to other tools.
//
//	LMGET_S3_ENDPOINT            https://<account>.r2.cloudflarestorage.com,
//	                             https://s3.<region>.amazonaws.com, ...
//	LMGET_S3_REGION              default auto (R2); the bucket's region on S3
//	LMGET_S3_ACCESS_KEY_ID       a read-only key is enough
//	LMGET_S3_SECRET_ACCESS_KEY
//	LMGET_S3_SESSION_TOKEN       for temporary keys
//
// Requests are signed with AWS Signature Version 4, path-style
// (<endpoint>/<bucket>/<key>), only to the endpoint's host: a signed request
// never follows a redirect. What the bucket serves is verified like any
// origin's bytes: the keys buy access, not trust.

type s3Config struct {
	endpoint string // scheme://host[:port], no trailing slash
	host     string
	region   string
	keyID    string
	secret   string
	token    string
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

var (
	s3BucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	s3SegRe    = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// parseS3Origin checks s3://bucket[/prefix] and returns it without a
// trailing slash. Bucket and prefix keep to characters that need no
// escaping, so the signed path is the path sent.
func parseS3Origin(o string) (string, bool) {
	rest, ok := strings.CutPrefix(o, "s3://")
	if !ok {
		return "", false
	}
	rest = strings.TrimRight(rest, "/")
	bucket, prefix, _ := strings.Cut(rest, "/")
	if !s3BucketRe.MatchString(bucket) || strings.Contains(bucket, "..") {
		return "", false
	}
	if prefix != "" {
		for _, s := range strings.Split(prefix, "/") {
			if !s3SegRe.MatchString(s) || s == "." || s == ".." {
				return "", false
			}
		}
	}
	return "s3://" + rest, true
}

// s3FromEnv reads the endpoint and keys for s3:// origins.
func s3FromEnv() (*s3Config, error) {
	missing := []string{}
	get := func(k string) string {
		v := strings.TrimSpace(os.Getenv(k))
		if v == "" {
			missing = append(missing, k)
		}
		return v
	}
	ep, id, secret := get("LMGET_S3_ENDPOINT"), get("LMGET_S3_ACCESS_KEY_ID"), get("LMGET_S3_SECRET_ACCESS_KEY")
	if len(missing) > 0 {
		return nil, newErr(ExitUsage, "usage", "an s3:// origin needs "+strings.Join(missing, ", ")).
			why("lmget reads the bucket's endpoint and keys from its own variables only, never from AWS_* or ~/.aws").
			fix("export LMGET_S3_ENDPOINT=https://<account>.r2.cloudflarestorage.com", "or https://s3.<region>.amazonaws.com with LMGET_S3_REGION").
			fix("", "and a read-only key in LMGET_S3_ACCESS_KEY_ID and LMGET_S3_SECRET_ACCESS_KEY")
	}
	u, err := url.Parse(ep)
	bad := err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != ""
	if !bad && u.Scheme != "https" && !(u.Scheme == "http" && loopbackHost(u.Hostname())) {
		bad = true
	}
	if bad {
		return nil, newErr(ExitUsage, "usage", "LMGET_S3_ENDPOINT is not an https URL of a host: "+quote(ep)).
			why("the keys sign every request: plain http only to this machine (a local test server)").
			fix("", "e.g. https://<account>.r2.cloudflarestorage.com")
	}
	region := strings.TrimSpace(os.Getenv("LMGET_S3_REGION"))
	if region == "" {
		region = "auto"
	}
	return &s3Config{endpoint: u.Scheme + "://" + u.Host, host: u.Host, region: region,
		keyID: id, secret: secret, token: strings.TrimSpace(os.Getenv("LMGET_S3_SESSION_TOKEN"))}, nil
}

func loopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// objectURL is the blob's URL on the endpoint, path-style.
func (s *s3Config) objectURL(origin, sha string) string {
	return s.endpoint + "/" + strings.TrimPrefix(origin, "s3://") + "/blobs/sha256/" + sha
}

// sign adds the SigV4 headers to a GET of an object on the endpoint.
func (s *s3Config) sign(req *http.Request, now time.Time) {
	t := now.UTC()
	req.Header.Set("X-Amz-Date", t.Format("20060102T150405Z"))
	req.Header.Set("X-Amz-Content-Sha256", emptySHA256)
	h := map[string]string{"host": req.URL.Host, "x-amz-content-sha256": emptySHA256, "x-amz-date": t.Format("20060102T150405Z")}
	if s.token != "" {
		req.Header.Set("X-Amz-Security-Token", s.token)
		h["x-amz-security-token"] = s.token
	}
	req.Header.Set("Authorization", sigv4(req.Method, req.URL.EscapedPath(), h, emptySHA256, s.keyID, s.secret, s.region, "s3", t))
}

// sigv4 is the Authorization value of AWS Signature Version 4 for a request
// with an empty query whose headers (lower-case names) are all signed.
func sigv4(method, uri string, headers map[string]string, payload, keyID, secret, region, service string, t time.Time) string {
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canon strings.Builder
	for _, k := range names {
		canon.WriteString(k + ":" + strings.Join(strings.Fields(headers[k]), " ") + "\n")
	}
	signed := strings.Join(names, ";")
	req := method + "\n" + uri + "\n\n" + canon.String() + "\n" + signed + "\n" + payload
	day := t.Format("20060102")
	scope := day + "/" + region + "/" + service + "/aws4_request"
	sum := sha256.Sum256([]byte(req))
	toSign := "AWS4-HMAC-SHA256\n" + t.Format("20060102T150405Z") + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := []byte("AWS4" + secret)
	for _, part := range []string{day, region, service, "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	return "AWS4-HMAC-SHA256 Credential=" + keyID + "/" + scope + ", SignedHeaders=" + signed +
		", Signature=" + hex.EncodeToString(hmacSHA256(key, toSign))
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

// s3KeyErrors are the S3 error codes that mean the endpoint, the keys or the
// clock are wrong, not that a blob is missing: AccessDenied is also what S3
// answers for a missing object to a key that may not list the bucket.
var s3KeyErrors = map[string]string{
	"SignatureDoesNotMatch":        "the secret key does not match the key id",
	"InvalidAccessKeyId":           "the endpoint does not know this key id",
	"AuthorizationHeaderMalformed": "the region is wrong for this bucket (LMGET_S3_REGION)",
	"RequestTimeTooSkewed":         "this machine's clock is too far from the server's",
	"ExpiredToken":                 "the temporary key has expired",
	"InvalidToken":                 "the session token is not valid",
	"TokenRefreshRequired":         "the temporary key has expired",
}

// s3Refusal reads a 400 or 403 answer to a signed request: an error when it
// says the configuration is wrong, nil when the blob is simply not there.
func s3Refusal(resp *http.Response, origin string) error {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusBadRequest {
		return nil
	}
	var body struct {
		Code string `xml:"Code"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if xml.Unmarshal(b, &body) != nil {
		return nil
	}
	why, ok := s3KeyErrors[body.Code]
	if !ok {
		return nil
	}
	e := newErr(ExitAuth, "auth", origin+" refused lmget's signature ("+printable(body.Code)+")").
		why("%s", why).
		fix("", "check LMGET_S3_ENDPOINT, LMGET_S3_REGION and the keys in LMGET_S3_ACCESS_KEY_ID and LMGET_S3_SECRET_ACCESS_KEY")
	e.Err = errS3Keys // not wrap: no cause line, the message says it
	return e
}

// errS3Keys marks a bucket that refused the keys themselves.
var errS3Keys = errors.New("the bucket refused the keys")
