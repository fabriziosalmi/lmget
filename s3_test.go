package lmget

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The signatures AWS publishes: its SigV4 test suite (awslabs/aws-c-auth,
// tests/aws-signing-test-suite/v4) and the GET Object example of the S3
// documentation. The keys are AWS's documentation examples.
func TestSigV4MatchesAWSVectors(t *testing.T) {
	suite := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	for _, v := range []struct {
		name, uri       string
		headers         map[string]string
		id, secret      string
		region, service string
		at              time.Time
		want            string
	}{
		{"get-vanilla", "/", map[string]string{"host": "example.amazonaws.com", "x-amz-date": "20150830T123600Z"},
			"AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "us-east-1", "service", suite,
			"5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
		{"get-vanilla-with-session-token", "/", map[string]string{"host": "example.amazonaws.com", "x-amz-date": "20150830T123600Z",
			"x-amz-security-token": "6e86291e8372ff2a2260956d9b8aae1d763fbf315fa00fa31553b73ebf194267"}, // gitleaks:allow AWS's published test vector
			"AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "us-east-1", "service", suite,
			"07ec1639c89043aa0e3e2de82b96708f198cceab042d4a97044c66dd9f74e7f8"},
		{"s3 get object", "/test.txt", map[string]string{"host": "examplebucket.s3.amazonaws.com", "range": "bytes=0-9",
			"x-amz-content-sha256": emptySHA256, "x-amz-date": "20130524T000000Z"},
			"AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", "s3", time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC),
			"f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"},
	} {
		got := sigv4("GET", v.uri, v.headers, emptySHA256, v.id, v.secret, v.region, v.service, v.at)
		if !strings.HasSuffix(got, "Signature="+v.want) {
			t.Errorf("%s: %s", v.name, got)
		}
	}
}

// fakeS3 is a private bucket at /<bucket>/<prefix>/blobs/sha256/<sha>: it
// checks each request's signature with the keys it knows, and answers as S3
// does to a wrong key (SignatureDoesNotMatch) and to a missing object for a
// key that may not list the bucket (AccessDenied).
type fakeS3 struct {
	bucket, secret string
	blobs          map[string][]byte // path -> bytes
	asked          atomic.Int64
	redirect       string // when set, every request is sent there
}

func (s *fakeS3) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.asked.Add(1)
		if s.redirect != "" {
			http.Redirect(w, r, s.redirect+r.URL.Path, http.StatusFound)
			return
		}
		at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
		h := map[string]string{"host": r.Host, "x-amz-content-sha256": r.Header.Get("X-Amz-Content-Sha256"), "x-amz-date": r.Header.Get("X-Amz-Date")}
		if tok := r.Header.Get("X-Amz-Security-Token"); tok != "" {
			h["x-amz-security-token"] = tok
		}
		want := sigv4(r.Method, r.URL.EscapedPath(), h, emptySHA256, "lmgetkey", s.secret, "auto", "s3", at)
		if err != nil || r.Header.Get("Authorization") != want {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>SignatureDoesNotMatch</Code><Message>no</Message></Error>`))
			return
		}
		b, ok := s.blobs[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`))
			return
		}
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(string(b)))
	})
}

func s3Env(t *testing.T, endpoint, secret string) {
	t.Setenv("LMGET_S3_ENDPOINT", endpoint)
	t.Setenv("LMGET_S3_ACCESS_KEY_ID", "lmgetkey")
	t.Setenv("LMGET_S3_SECRET_ACCESS_KEY", secret)
	t.Setenv("LMGET_S3_REGION", "")
	t.Setenv("LMGET_S3_SESSION_TOKEN", "")
}

func s3Client(t *testing.T, srv *httptest.Server, origin string) *Client {
	t.Helper()
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HF_TOKEN_PATH", filepath.Join(t.TempDir(), "none"))
	t.Setenv("HF_HOME", t.TempDir())
	t.Setenv("LMGET_LICENSES", "")
	c, err := New(Options{Home: t.TempDir(), HFEndpoint: srv.URL, Conns: 4, ChunkSize: 256 << 10, Origins: []string{origin}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPrivateBucketOrigin(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	hf := httptest.NewServer(f.handler())
	defer hf.Close()
	weights := f.files[0] // the LFS file: the only one with a sha256 before download
	b := &fakeS3{bucket: "models", secret: "s3cret", blobs: map[string][]byte{
		"/models/team/lmget/blobs/sha256/" + sha(weights.data): weights.data}}
	s3 := httptest.NewServer(b.handler())
	defer s3.Close()

	s3Env(t, s3.URL, "s3cret")
	t.Setenv("LMGET_S3_SESSION_TOKEN", "session-token") // signed too, or the bucket refuses
	c := s3Client(t, hf, "s3://models/team/lmget/")
	out, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range out.Plan.Files {
		if x.Path == weights.path && x.From != UserOrigin+"s3://models/team/lmget" {
			t.Fatalf("the weights came from %q", x.From)
		}
	}
	if f.served.Load() >= int64(len(weights.data)) {
		t.Fatalf("the weights came from the source: %d bytes", f.served.Load())
	}

	// a wrong secret is the user's to fix: no silent download from the source
	f.served.Store(0)
	s3Env(t, s3.URL, "wrong")
	c = s3Client(t, hf, "s3://models/team/lmget")
	_, err = c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil)
	if e := AsError(err); e == nil || e.Code != ExitAuth || !strings.Contains(e.Msg, "SignatureDoesNotMatch") || f.served.Load() >= int64(len(weights.data)) {
		t.Fatalf("a refused signature: %v, %d bytes from the source", err, f.served.Load())
	}

	// a blob the bucket does not have (AccessDenied, as S3 answers a key
	// that may not list it): the source serves it
	s3Env(t, s3.URL, "s3cret")
	c = s3Client(t, hf, "s3://models/empty")
	out, err = c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil)
	if err != nil || f.served.Load() < int64(len(weights.data)) {
		t.Fatalf("a missing blob: %v, %d bytes from the source", err, f.served.Load())
	}
}

// A signed request goes to the endpoint only: a redirect is not followed,
// so the keys and the session token reach no other host.
func TestPrivateBucketNeverFollowsARedirect(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	hf := httptest.NewServer(f.handler())
	defer hf.Close()
	var elsewhere atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	b := &fakeS3{bucket: "models", secret: "s3cret", redirect: other.URL}
	s3 := httptest.NewServer(b.handler())
	defer s3.Close()
	s3Env(t, s3.URL, "s3cret")
	t.Setenv("LMGET_S3_SESSION_TOKEN", "session-token")
	c := s3Client(t, hf, "s3://models")
	if _, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil); err != nil {
		t.Fatal(err) // the source serves it
	}
	if b.asked.Load() == 0 || elsewhere.Load() != 0 {
		t.Fatalf("bucket asked %d times, the redirect target %d", b.asked.Load(), elsewhere.Load())
	}
}

func TestPrivateBucketConfig(t *testing.T) {
	for _, env := range []string{"LMGET_S3_ENDPOINT", "LMGET_S3_ACCESS_KEY_ID", "LMGET_S3_SECRET_ACCESS_KEY"} {
		t.Setenv(env, "")
	}
	// keys meant for other tools are never read
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAOTHERTOOL")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "other")
	t.Setenv("AWS_ENDPOINT_URL", "https://s3.example.com")
	if _, err := New(Options{Home: t.TempDir(), Origins: []string{"s3://models"}}); AsError(err) == nil || AsError(err).Code != ExitUsage {
		t.Fatalf("an s3 origin without lmget's own variables: %v", err)
	}
	for ep, ok := range map[string]bool{
		"https://acc.r2.cloudflarestorage.com": true, "http://127.0.0.1:9000": true, "http://localhost:9000": true,
		"http://minio.lan:9000": false, "https://acc.r2.cloudflarestorage.com/bucket": false, "ftp://x": false,
		"https://user@acc.r2.cloudflarestorage.com": false,
	} {
		s3Env(t, ep, "s")
		if _, err := s3FromEnv(); (err == nil) != ok {
			t.Errorf("endpoint %s: %v", ep, err)
		}
	}
	for o, want := range map[string]string{
		"s3://models": "s3://models", "s3://models/": "s3://models", "s3://my.models/a/b_c-1/": "s3://my.models/a/b_c-1",
		"s3://Models": "", "s3://m": "", "s3://models/../x": "", "s3://models/a//b": "", "s3://models/a b": "",
		"s3://models/a?x=1": "", "s3://models/%2e%2e": "",
	} {
		got, ok := parseS3Origin(o)
		if (want != "") != ok || got != want && ok {
			t.Errorf("%s: %q %v", o, got, ok)
		}
	}
}
