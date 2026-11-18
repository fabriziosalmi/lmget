package lmget

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// An mDNS answer is anyone's: only addresses on this machine's networks
// become origins, never loopback, link-local (the cloud metadata address),
// a broadcast address or the internet.
func TestLANAnswersNameOnlyServersOnTheLAN(t *testing.T) {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 77})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(lanService), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET})
	q, _ := b.Finish()
	ips := []net.IP{
		net.IPv4(192, 168, 1, 20), net.IPv4(127, 0, 0, 1), net.IPv4(169, 254, 169, 254), net.IPv4(8, 8, 8, 8),
		net.IPv4(192, 168, 1, 255), net.IPv4(192, 168, 1, 0), net.IPv4(255, 255, 255, 255), net.IPv4(10, 1, 2, 3),
	}
	reply, ok := mdnsAnswer(q, "lmget-evil", 22, ips)
	if !ok {
		t.Fatal("no answer built")
	}
	nets := []*net.IPNet{{IP: net.IPv4(192, 168, 1, 0), Mask: net.CIDRMask(24, 32)}}
	if got := strings.Join(mdnsServers(reply, 77, nets), " "); got != "http://192.168.1.20:22" {
		t.Fatalf("origins taken from the answer: %q", got)
	}
}

func TestOnLAN(t *testing.T) {
	nets := []*net.IPNet{
		{IP: net.IPv4(192, 168, 1, 7), Mask: net.CIDRMask(24, 32)},
		{IP: net.IPv4(10, 9, 0, 1), Mask: net.CIDRMask(31, 32)}, // point-to-point: both are hosts
	}
	for ip, want := range map[string]bool{
		"192.168.1.20": true, "192.168.1.0": false, "192.168.1.255": false, "192.168.2.1": false,
		"10.9.0.0": true, "10.9.0.1": true, "127.0.0.1": false, "169.254.169.254": false, "8.8.8.8": false,
	} {
		if got := onLAN(net.ParseIP(ip), nets); got != want {
			t.Errorf("%s: %v", ip, got)
		}
	}
}

// One answer naming hundreds of servers adds a few.
func TestDiscoveryKeepsAFewServers(t *testing.T) {
	found := map[string]bool{}
	for i := range 550 {
		found[fmt.Sprintf("http://192.168.%d.%d:7878", i/250, i%250+1)] = true
	}
	if got := lanServerList(found); len(got) != maxLANOrigins {
		t.Fatalf("%d servers kept", len(got))
	}
}

// lmget's own responder answers a source once a second, and the group
// once a second.
func TestTheResponderIsRateLimited(t *testing.T) {
	l := newAnswerLimit()
	now := time.Now()
	if !l.allow("192.168.1.5", false, now) || l.allow("192.168.1.5", false, now.Add(500*time.Millisecond)) {
		t.Fatal("a source was answered twice in a second")
	}
	if !l.allow("192.168.1.6", true, now) || l.allow("192.168.1.7", true, now.Add(100*time.Millisecond)) {
		t.Fatal("the group was answered twice in a second")
	}
	if !l.allow("192.168.1.5", false, now.Add(1100*time.Millisecond)) {
		t.Fatal("a source was not answered after a second")
	}
}

// A LAN server that answers the probe and then trickles, or never answers,
// cannot hold a pull: the source takes over.
func TestASlowLANServerCannotHoldAPull(t *testing.T) {
	oldProbe, oldFile := lanProbeTimeout, lanFileTime
	lanProbeTimeout, lanFileTime = 300*time.Millisecond, time.Second
	defer func() { lanProbeTimeout, lanFileTime = oldProbe, oldFile }()
	stop := make(chan struct{})
	defer close(stop)
	trickle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" {
			w.Header().Set("Content-Range", "bytes 0-0/99999999")
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte("x"))
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		for {
			select {
			case <-stop:
				return
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
				w.Write([]byte("x"))
				w.(http.Flusher).Flush()
			}
		}
	}))
	defer trickle.Close()
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	defer silent.Close()

	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	c.opt.LAN = true
	c.lanOnce.Do(func() { c.lanFound = []string{silent.URL, trickle.URL} })
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := c.Pull(ctx, Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil); err != nil {
		t.Fatalf("after %v: %v", time.Since(t0), err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("the pull took %v", d)
	}
}

// An origin's redirect to another host is not followed; one within its
// own host is.
func TestAnOriginsRedirectStaysOnItsHost(t *testing.T) {
	f := &fakeHF{files: modelFiles()}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	var elsewhere atomic.Int64
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere.Add(1) }))
	defer local.Close()
	weights := f.files[0]
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/away/"):
			http.Redirect(w, r, local.URL+"/admin/delete?all=1", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/here/blobs/"):
			http.Redirect(w, r, "/real/"+strings.TrimPrefix(r.URL.Path, "/here/"), http.StatusFound)
		case r.URL.Path == "/real/blobs/sha256/"+sha(weights.data):
			http.ServeContent(w, r, "", time.Time{}, strings.NewReader(string(weights.data)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	for _, tc := range []struct {
		base     string
		fromThem bool
	}{{origin.URL + "/away", false}, {origin.URL + "/here", true}} {
		c, err := New(Options{Home: t.TempDir(), HFEndpoint: srv.URL, Conns: 4, ChunkSize: 256 << 10, Origins: []string{tc.base}})
		if err != nil {
			t.Fatal(err)
		}
		out, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		from := ""
		for _, x := range out.Plan.Files {
			if x.Path == weights.path {
				from = x.From
			}
		}
		if (from != "") != tc.fromThem {
			t.Errorf("%s: the weights came from %q", tc.base, from)
		}
	}
	if elsewhere.Load() != 0 {
		t.Fatalf("a redirect reached another host %d times", elsewhere.Load())
	}
}

// lmget serve never hands out what the signed index revoked, and serves
// nothing when it cannot read the revocations.
func TestServeSkipsRevokedBlobs(t *testing.T) {
	home, public, _, lan := lanPair(t)
	c, err := New(Options{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	var lfs fakeFile
	for _, f := range public {
		if f.lfs {
			lfs = f
		}
	}
	get := func() int {
		resp, err := http.Get(lan.URL + "/blobs/sha256/" + sha(lfs.data))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(); code != 200 {
		t.Fatalf("before the revocation: %d", code)
	}
	if err := c.writeRevokedBlobs(&Index{Sequence: 9, Revoked: []IndexRevoked{{SHA256: sha(lfs.data), Reason: "test"}}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(c.LANServer()) // a fresh server: no cached listing
	defer srv.Close()
	lan.URL = srv.URL
	if code := get(); code != 404 {
		t.Fatalf("a revoked blob was served: %d", code)
	}
	if err := writeFileAtomic(c.revokedBlobsPath(), []byte("garbage")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.LANServer().Shared(); err == nil {
		t.Fatal("an unreadable revocation record was taken as none")
	}
}

// lmget serve answers this machine and its networks, not a VPN peer or the
// internet, unless it is told to.
func TestServeAnswersTheLANOnly(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:5000": true, "[::1]:5000": true, "8.8.8.8:5000": false,
		"[2001:db8::1]:5000": false, "100.101.102.103:5000": false, "garbage": false,
	} {
		if got := lanClient(addr); got != want {
			t.Errorf("%s: %v", addr, got)
		}
	}
	c, err := New(Options{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ls := c.LANServer()
	for open, want := range map[bool]int{false: http.StatusForbidden, true: http.StatusNotFound} {
		ls.Open = open
		r := httptest.NewRequest("GET", "/blobs/sha256/"+strings.Repeat("a", 64), nil)
		r.RemoteAddr = "8.8.8.8:4000"
		w := httptest.NewRecorder()
		ls.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("open=%v: %d, want %d", open, w.Code, want)
		}
	}
}

// A client that stops reading loses its download; it does not hold the
// server's connection.
func TestServeCutsAClientThatStopsReading(t *testing.T) {
	old := lanWriteIdle
	lanWriteIdle = 300 * time.Millisecond
	defer func() { lanWriteIdle = old }()
	big := fakeSafetensors(32 << 20)
	f := &fakeHF{files: []fakeFile{{path: "model.safetensors", data: big, lfs: true}}}
	src := httptest.NewServer(f.handler())
	home := t.TempDir()
	if _, err := newTestClient(t, src, home).Pull(context.Background(), Request{Ref: hfRef("org/big"), Target: Target{Kind: "none"}}, nil); err != nil {
		t.Fatal(err)
	}
	src.Close()
	c, err := New(Options{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	lan := httptest.NewServer(c.LANServer())
	defer lan.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(lan.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /blobs/sha256/%s HTTP/1.1\r\nHost: x\r\n\r\n", sha(big))
	time.Sleep(1500 * time.Millisecond) // reads nothing
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var got int
	buf := make([]byte, 1<<20)
	for {
		n, err := conn.Read(buf)
		got += n
		if err != nil {
			break
		}
	}
	if got >= len(big) {
		t.Fatalf("a client that stopped reading got all %d bytes", got)
	}
}

func TestServeCapsItsConnections(t *testing.T) {
	old := lanMaxConns
	lanMaxConns = 2
	defer func() { lanMaxConns = old }()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := LANListener(raw)
	defer ln.Close()
	var accepted atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			defer c.Close()
		}
	}()
	var conns []net.Conn
	for range 3 {
		c, err := net.Dial("tcp", raw.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	time.Sleep(300 * time.Millisecond)
	if n := accepted.Load(); n != 2 {
		t.Fatalf("%d connections accepted at once, cap 2", n)
	}
	for _, c := range conns {
		c.Close()
	}
}

func TestPublicIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false,
		"169.254.169.254": false, "100.64.1.1": false, "0.0.0.0": false, "255.255.255.255": false,
		"::1": false, "fc00::1": false, "fe80::1": false, "::ffff:127.0.0.1": false, "::ffff:10.0.0.1": false,
	} {
		if got := publicIP(net.ParseIP(ip)); got != want {
			t.Errorf("%s: %v", ip, got)
		}
	}
}

// A public-only request (lmget mcp's URL and OCI refs) never connects to
// this machine or its networks, by address or by name, nor through a proxy.
func TestPublicOnlyRequestsReachPublicHostsOnly(t *testing.T) {
	var hits atomic.Int64
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer local.Close()
	c, err := New(Options{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(local.URL, "http://"))
	for _, u := range []string{local.URL, "http://localhost:" + port} {
		req, _ := http.NewRequestWithContext(PublicOnly(context.Background()), "GET", u+"/x", nil)
		if resp, err := c.http.Do(req); err == nil {
			resp.Body.Close()
			t.Errorf("%s: reached", u)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("a private host got %d requests", hits.Load())
	}
	req, _ := http.NewRequest("GET", local.URL+"/x", nil) // an ordinary request still goes
	if resp, err := c.http.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("an ordinary request: %v", err)
	}
	old := envProxy
	envProxy = func(*http.Request) (*url.URL, error) { return url.Parse("http://proxy.example:3128") }
	defer func() { envProxy = old }()
	pr, _ := http.NewRequestWithContext(PublicOnly(context.Background()), "GET", "https://example.com/x", nil)
	if _, err := publicOnlyProxy(pr); err == nil {
		t.Fatal("a public-only request went through a proxy")
	}
	if u, err := publicOnlyProxy(req); err != nil || u == nil {
		t.Fatalf("an ordinary request lost its proxy: %v %v", u, err)
	}
}

// What the responder answers: a querier on the LAN, once a second; nobody
// off it, whatever the port.
func TestTheResponderAnswersTheLANOnly(t *testing.T) {
	nets := []*net.IPNet{{IP: net.IPv4(192, 168, 1, 7), Mask: net.CIDRMask(24, 32)}}
	l, now := newAnswerLimit(), time.Now()
	for _, tc := range []struct {
		ip   string
		port int
		want bool
	}{
		{"203.0.113.9", 5353, false}, {"203.0.113.9", 40000, false}, {"127.0.0.1", 40000, false},
		{"192.168.1.30", 40000, true}, {"192.168.1.30", 40000, false}, // again within the second
		{"192.168.1.31", 5353, true}, {"192.168.1.32", 5353, false}, // the group, again within the second
	} {
		if got := l.answer(&net.UDPAddr{IP: net.ParseIP(tc.ip), Port: tc.port}, nets, now); got != tc.want {
			t.Errorf("%s:%d: %v", tc.ip, tc.port, got)
		}
	}
}
