package lmget

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestPlanAccess(t *testing.T) {
	for name, tc := range map[string]struct {
		private bool
		gated   string
		want    string
	}{
		"public":       {false, "", "public"},
		"private":      {true, "", ""},
		"gated auto":   {false, "auto", ""},
		"gated manual": {false, "manual", ""},
	} {
		f := &fakeHF{files: modelFiles(), gatedAs: tc.gated}
		f.private.Store(tc.private)
		srv := httptest.NewServer(f.handler())
		c := newTestClient(t, srv, t.TempDir())
		out, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}, DryRun: true}, nil)
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out.Plan.Access != tc.want {
			t.Errorf("%s: access %q, want %q", name, out.Plan.Access, tc.want)
		}
	}
}

// lanPair is a store holding one public and one private model, served by
// lmget serve's handler.
func lanPair(t *testing.T) (home string, public, private []fakeFile, lan *httptest.Server) {
	t.Helper()
	pub := &fakeHF{files: modelFiles()}
	priv := &fakeHF{files: []fakeFile{{path: "secret.safetensors", data: fakeSafetensors(5000), lfs: true}}}
	priv.private.Store(true)
	home = t.TempDir()
	for repo, f := range map[string]*fakeHF{"org/pub": pub, "org/priv": priv} {
		srv := httptest.NewServer(f.handler())
		c := newTestClient(t, srv, home)
		if _, err := c.Pull(context.Background(), Request{Ref: hfRef(repo), Target: Target{Kind: "none"}}, nil); err != nil {
			t.Fatal(err)
		}
		srv.Close()
	}
	c, err := New(Options{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	lan = httptest.NewServer(c.LANServer())
	t.Cleanup(lan.Close)
	return home, pub.files, priv.files, lan
}

func TestLANServerSharesPublicModelsOnly(t *testing.T) {
	_, public, private, lan := lanPair(t)
	get := func(method, p string) (int, []byte) {
		req, _ := http.NewRequest(method, lan.URL+p, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	for _, f := range public {
		if !f.lfs {
			continue
		}
		code, b := get("GET", "/blobs/sha256/"+sha(f.data))
		if code != 200 || sha(b) != sha(f.data) {
			t.Fatalf("a public blob: %d", code)
		}
	}
	if code, _ := get("GET", "/blobs/sha256/"+sha(private[0].data)); code != 404 {
		t.Fatalf("a private model's blob was served: %d", code)
	}
	for _, p := range []string{"/", "/blobs/sha256/../../x", "/blobs/sha256/" + strings.Repeat("A", 64), "/index.json"} {
		if code, _ := get("GET", p); code != 404 {
			t.Errorf("%s: %d", p, code)
		}
	}
	if code, _ := get("PUT", "/blobs/sha256/"+sha(public[0].data)); code != 405 {
		t.Fatalf("a write was accepted: %d", code)
	}
}

func TestLANServersAreAskedFirst(t *testing.T) {
	_, public, _, lan := lanPair(t)
	src := &fakeHF{files: public}
	srv := httptest.NewServer(src.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	c.opt.LAN = true
	c.lanOnce.Do(func() { c.lanFound = []string{lan.URL} }) // as DiscoverLAN would find it
	out, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lfs int64
	for _, f := range public {
		if f.lfs {
			lfs += int64(len(f.data))
		}
	}
	if got := src.served.Load(); got > out.Plan.Bytes-lfs {
		t.Fatalf("the source served %d bytes: the LAN server holds every LFS file (%d bytes)", got, lfs)
	}
	// with discovery off, the same found server is not asked
	c2 := newTestClient(t, srv, t.TempDir())
	c2.lanOnce.Do(func() { c2.lanFound = []string{lan.URL} })
	before := src.served.Load()
	if _, err := c2.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil); err != nil {
		t.Fatal(err)
	}
	if src.served.Load()-before < lfs {
		t.Fatal("LAN servers were asked with discovery off")
	}
}

func TestMDNSAnswer(t *testing.T) {
	query := func(id uint16, name string, typ dnsmessage.Type) []byte {
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id})
		b.StartQuestions()
		b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET})
		out, _ := b.Finish()
		return out
	}
	ips := []net.IP{net.IPv4(192, 168, 1, 20), net.IPv4(10, 0, 0, 5)}
	reply, ok := mdnsAnswer(query(4242, lanService, dnsmessage.TypePTR), "lmget-box", 7878, ips)
	if !ok {
		t.Fatal("no answer to a query for the service")
	}
	got := strings.Join(mdnsServers(reply, 4242), " ")
	if got != "http://192.168.1.20:7878 http://10.0.0.5:7878" && got != "http://10.0.0.5:7878 http://192.168.1.20:7878" {
		t.Fatalf("servers %q", got)
	}
	if s := mdnsServers(reply, 4243); s != nil {
		t.Fatal("an answer to another query was taken")
	}
	if _, ok := mdnsAnswer(query(1, "_http._tcp.local.", dnsmessage.TypePTR), "lmget-box", 7878, ips); ok {
		t.Fatal("answered a query for another service")
	}
	if _, ok := mdnsAnswer(reply, "lmget-box", 7878, ips); ok {
		t.Fatal("answered an answer")
	}
	if _, ok := mdnsAnswer(query(1, lanService, dnsmessage.TypePTR), "lmget-box", 7878, nil); ok {
		t.Fatal("answered with no address to give")
	}
	if _, ok := mdnsAnswer([]byte{1, 2, 3}, "lmget-box", 7878, ips); ok {
		t.Fatal("answered garbage")
	}
}

func TestAnnounceCommand(t *testing.T) {
	has := func(names ...string) func(string) (string, error) {
		return func(n string) (string, error) {
			for _, x := range names {
				if x == n {
					return "/usr/bin/" + n, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	got := announceCommand("darwin", "lmget-box", 7878, has("dns-sd"))
	if strings.Join(got, " ") != "/usr/bin/dns-sd -R lmget-box _lmget._tcp local 7878 v=1" {
		t.Fatalf("darwin: %q", got)
	}
	got = announceCommand("linux", "lmget-box", 7878, has("avahi-publish"))
	if strings.Join(got, " ") != "/usr/bin/avahi-publish -s lmget-box _lmget._tcp 7878 v=1" {
		t.Fatalf("linux with avahi: %q", got)
	}
	for _, tc := range []struct {
		goos string
		has  []string
	}{{"darwin", nil}, {"linux", nil}, {"linux", []string{"dns-sd"}}, {"windows", []string{"dns-sd", "avahi-publish"}}} {
		if got := announceCommand(tc.goos, "lmget-box", 7878, has(tc.has...)); got != nil {
			t.Errorf("%s with %v: %q, want lmget's own responder", tc.goos, tc.has, got)
		}
	}
	if inst := lanInstance(); !regexp.MustCompile(`^lmget-[a-z0-9-]{1,50}$`).MatchString(inst) {
		t.Fatalf("instance name %q", inst)
	}
}

// TestRespondAndDiscover runs lmget's own responder and finds it, where
// lmget uses it: Linux without Avahi.
func TestRespondAndDiscover(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("lmget answers itself on Linux without Avahi (and Windows); macOS goes through dns-sd")
	}
	if _, err := exec.LookPath("avahi-publish"); err == nil {
		t.Skip("Avahi is here: lmget goes through it")
	}
	if len(lanInterfaces()) == 0 {
		t.Skip("no multicast interface")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- respond(ctx, 17979) }()
	var found []string
	for i := 0; i < 5 && len(found) == 0; i++ {
		select {
		case err := <-errc:
			t.Fatalf("the responder stopped: %v", err)
		default:
		}
		found, _ = DiscoverLAN(ctx, time.Second)
	}
	if len(found) == 0 {
		t.Fatal("the responder was not found")
	}
	for _, o := range found {
		if !strings.HasPrefix(o, "http://") || !strings.HasSuffix(o, ":17979") {
			t.Fatalf("found %q", o)
		}
	}
}

func TestAModelFromAnOlderLmgetIsSharedOncePulledAgain(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	home := t.TempDir()
	c := newTestClient(t, srv, home)
	if _, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil); err != nil {
		t.Fatal(err)
	}
	// a manifest written before plans recorded their access
	ms, _, err := c.store.Manifests()
	if err != nil || len(ms) != 1 {
		t.Fatalf("%d manifests, %v", len(ms), err)
	}
	for p := range ms {
		b, _ := os.ReadFile(p)
		old := strings.Replace(string(b), `"access": "public",`, "", 1)
		if old == string(b) {
			t.Fatal("the manifest has no access field to remove")
		}
		os.Chmod(p, 0o644)
		if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	shared := func() int {
		n, _, err := c.LANServer().Shared()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := shared(); n != 0 {
		t.Fatalf("a model whose access is unknown was shared: %d", n)
	}
	served := f.served.Load()
	if _, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil); err != nil {
		t.Fatal(err)
	}
	if f.served.Load() != served {
		t.Fatal("pulling it again downloaded files")
	}
	if n := shared(); n != 1 {
		t.Fatalf("pulled again, the model is not shared: %d", n)
	}
}
