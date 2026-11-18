package lmget

// LAN sharing. "lmget serve" shares the public models of this store with
// the local network over plain HTTP and announces itself by mDNS as an
// _lmget._tcp service. An lmget with LAN discovery on (LMGET_LAN=on) looks
// for such servers once per run and asks them first, as origins: what they
// serve is verified against the sha256 the plan knows, like any origin's
// bytes, and the source is asked when they do not have a file.
//
// Only public models are shared: a plan records Access "public" when the
// source says anyone may download it without a token or an accepted
// license. A gated or private model, or a URL, an OCI image or a model from
// a registry that may need a token, is never served: sharing it would hand
// colleagues what their own access does not allow.
//
// Discovery is off unless asked for: an mDNS query tells the network that
// this machine is about to download models, and the requests that follow
// tell an lmget serve which ones.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/netutil"
)

const (
	lanService     = "_lmget._tcp.local."
	mdnsPort       = 5353
	lanDiscoverFor = 600 * time.Millisecond
	// maxLANOrigins: what anyone on the network can add to a pull's origins
	// is a few servers, not hundreds (one answer can name 25,000)
	maxLANOrigins = 4
	// LANPort is the port lmget serve listens on by default.
	LANPort = 7878
)

var mdnsGroup = net.IPv4(224, 0, 0, 251)

// --- serving -------------------------------------------------------------------

// LANServer serves the public blobs of the store at /blobs/sha256/<sha256>,
// the paths every lmget asks an origin for.
type LANServer struct {
	// Open serves clients off this machine's networks too: by default only
	// the LAN (and this machine) are answered, whatever address it listens on
	Open    bool
	c       *Client
	mu      sync.Mutex
	blobs   map[string]int64 // sha256 -> size, public models only
	models  int
	scanned time.Time
}

// LANServer returns the handler lmget serve runs.
func (c *Client) LANServer() *LANServer { return &LANServer{c: c} }

// Shared returns how many models and blobs the server shares now.
func (s *LANServer) Shared() (models, blobs int, err error) {
	if err := s.refresh(true); err != nil {
		return 0, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.models, len(s.blobs), nil
}

// refresh reads the store's manifests again when the last read is older
// than a few seconds, so models pulled meanwhile are shared too.
func (s *LANServer) refresh(force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !force && time.Since(s.scanned) < 10*time.Second {
		return nil
	}
	ms, _, err := s.c.store.Manifests()
	if err != nil {
		return err
	}
	// what the signed index revoked is never served, to clients whose index
	// is older; when the record cannot be read, nothing is
	revoked, err := s.c.revokedBlobs()
	if err != nil {
		s.blobs, s.models, s.scanned = nil, 0, time.Now()
		return err
	}
	blobs, models := map[string]int64{}, 0
	for _, m := range ms {
		if m.Plan == nil || m.Plan.Access != "public" {
			continue
		}
		if src := m.Plan.Ref.Source; src != "hf" && src != "ollama" {
			continue
		}
		models++
		for _, f := range m.Plan.Files {
			if _, bad := revoked[f.SHA256]; hex64.MatchString(f.SHA256) && !bad {
				blobs[f.SHA256] = f.Size
			}
		}
	}
	s.blobs, s.models, s.scanned = blobs, models, time.Now()
	return nil
}

// lanMaxConns and lanWriteIdle bound what clients can hold: connections at
// once, and how long a response may wait for a client that stopped reading.
var (
	lanMaxConns  = 64
	lanWriteIdle = 30 * time.Second
)

// LANListener caps a server's connections at lanMaxConns.
func LANListener(ln net.Listener) net.Listener { return netutil.LimitListener(ln, lanMaxConns) }

// lanClient reports whether a client may be served: on this machine's
// networks, or this machine itself.
func lanClient(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || onLAN(ip, lanNets()))
}

// idleWriter pushes the write deadline forward before every write: a slow
// client keeps its download, one that stops reading loses it.
type idleWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func (w idleWriter) Write(b []byte) (int, error) {
	_ = w.rc.SetWriteDeadline(time.Now().Add(lanWriteIdle))
	return w.ResponseWriter.Write(b)
}

func (s *LANServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !s.Open && !lanClient(r.RemoteAddr) {
		http.Error(w, "this server answers its local network only", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "read only", http.StatusMethodNotAllowed)
		return
	}
	sum, ok := strings.CutPrefix(r.URL.Path, "/blobs/sha256/")
	if !ok || !hex64.MatchString(sum) {
		http.NotFound(w, r)
		return
	}
	_ = s.refresh(false)
	s.mu.Lock()
	size, shared := s.blobs[sum]
	s.mu.Unlock()
	if !shared || !s.c.store.Has(sum, size) { // only verified blobs of public models
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(s.c.store.BlobPath(sum))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(idleWriter{w, http.NewResponseController(w)}, r, sum, time.Time{}, f)
}

// --- announcing ------------------------------------------------------------------

var labelRe = regexp.MustCompile(`[^a-z0-9-]+`)

// lanInstance is this machine's service instance name: lmget-<host>.
func lanInstance() string {
	h, _ := os.Hostname()
	h = strings.Trim(labelRe.ReplaceAllString(strings.ToLower(strings.SplitN(h, ".", 2)[0]), "-"), "-")
	if h == "" {
		h = "host"
	}
	if len(h) > 50 {
		h = h[:50]
	}
	return "lmget-" + h
}

// lanInterfaces are the interfaces mDNS goes out on: up, multicast, not
// loopback, with an IPv4 address. All of them, not the default route's
// only: a VPN often holds the default route and carries no multicast.
func lanInterfaces() []net.Interface {
	ifs, _ := net.Interfaces()
	var out []net.Interface
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if len(ipv4Of(&ifi)) > 0 {
			out = append(out, ifi)
		}
	}
	return out
}

func ipv4Of(ifi *net.Interface) []net.IP {
	addrs, _ := ifi.Addrs()
	var out []net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
			out = append(out, n.IP.To4())
		}
	}
	return out
}

// lanNets are the IPv4 networks of the interfaces mDNS goes out on: a LAN
// server's address, and a querier's, is inside one of them.
func lanNets() []*net.IPNet {
	var out []*net.IPNet
	for _, ifi := range lanInterfaces() {
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				out = append(out, n)
			}
		}
	}
	return out
}

// onLAN reports whether ip is a unicast address inside one of nets, other
// than the network's own and broadcast addresses: not loopback, link-local
// (the cloud metadata address), or anything off this machine's networks.
func onLAN(ip net.IP, nets []*net.IPNet) bool {
	v4 := ip.To4()
	if v4 == nil || v4.IsLoopback() || v4.IsLinkLocalUnicast() || v4.IsUnspecified() || v4.IsMulticast() {
		return false
	}
	for _, n := range nets {
		base, mask := n.IP.To4(), n.Mask
		if base == nil || !n.Contains(v4) {
			continue
		}
		if len(mask) == 16 {
			mask = mask[12:]
		}
		netw, bcast := make(net.IP, 4), make(net.IP, 4)
		for i := range 4 {
			netw[i], bcast[i] = base[i]&mask[i], base[i]|^mask[i]
		}
		if ones, _ := mask.Size(); ones >= 31 || (!v4.Equal(netw) && !v4.Equal(bcast)) {
			return true
		}
	}
	return false
}

// Announce makes this machine's server at port known as an _lmget._tcp
// service until ctx ends. Where the system runs an mDNS responder, it goes
// through it: on macOS with dns-sd, on Linux with avahi-publish when Avahi is
// there. Two responders cannot share port 5353 reliably (macOS hands a query
// from the network to one socket only), and the system's answers the queries
// of every client. Elsewhere lmget answers the queries itself.
func Announce(ctx context.Context, port int) error {
	if argv := announceCommand(runtime.GOOS, lanInstance(), port, exec.LookPath); argv != nil {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr = nil, nil
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("mDNS: %s: %w", argv[0], err)
		}
		err := cmd.Wait()
		if ctx.Err() != nil {
			return nil // stopped with the server
		}
		return fmt.Errorf("mDNS: %s ended: %v", argv[0], err)
	}
	return respond(ctx, port)
}

// announceCommand is the command that registers the service with the
// system's responder, or nil when lmget answers itself. The instance name
// holds only [a-z0-9-] (lanInstance), and nothing goes through a shell.
func announceCommand(goos, instance string, port int, lookPath func(string) (string, error)) []string {
	p := strconv.Itoa(port)
	switch goos {
	case "darwin":
		if bin, err := lookPath("dns-sd"); err == nil {
			return []string{bin, "-R", instance, "_lmget._tcp", "local", p, "v=1"}
		}
	case "linux":
		if bin, err := lookPath("avahi-publish"); err == nil {
			return []string{bin, "-s", instance, "_lmget._tcp", p, "v=1"}
		}
	}
	return nil
}

// respond answers mDNS queries for the _lmget._tcp service itself.
func respond(ctx context.Context, port int) error {
	lc := net.ListenConfig{Control: reusePort}
	pc, err := lc.ListenPacket(ctx, "udp4", fmt.Sprintf("0.0.0.0:%d", mdnsPort))
	if err != nil {
		return fmt.Errorf("mDNS: %w", err)
	}
	defer pc.Close()
	p := ipv4.NewPacketConn(pc)
	joined := 0
	for _, ifi := range lanInterfaces() {
		if p.JoinGroup(&ifi, &net.UDPAddr{IP: mdnsGroup}) == nil {
			joined++
		}
	}
	if joined == 0 {
		return errors.New("mDNS: no network interface could join the multicast group")
	}
	_ = p.SetControlMessage(ipv4.FlagInterface, true)
	go func() { <-ctx.Done(); pc.Close() }()
	instance := lanInstance()
	buf := make([]byte, 9000)
	lim := newAnswerLimit()
	for {
		n, cm, src, err := p.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("mDNS: %w", err)
		}
		// a querier on this machine's networks only (RFC 6762 section 11),
		// at most once a second each, and the group at most once a second:
		// no answers for anyone off the LAN, no flood on request
		u, isUDP := src.(*net.UDPAddr)
		if !isUDP || !lim.answer(u, lanNets(), time.Now()) {
			continue
		}
		var ifi *net.Interface
		if cm != nil {
			ifi, _ = net.InterfaceByIndex(cm.IfIndex)
		}
		var ips []net.IP
		if ifi != nil {
			ips = ipv4Of(ifi)
		}
		if len(ips) == 0 {
			for _, i := range lanInterfaces() {
				ips = append(ips, ipv4Of(&i)...)
			}
		}
		reply, ok := mdnsAnswer(buf[:n], instance, port, ips)
		if !ok {
			continue
		}
		to := src
		if u.Port == mdnsPort {
			to = &net.UDPAddr{IP: mdnsGroup, Port: mdnsPort} // a full mDNS querier: answer to the group
			if ifi != nil {
				_ = p.SetMulticastInterface(ifi)
			}
		}
		_, _ = p.WriteTo(reply, nil, to)
	}
}

// answerLimit allows one answer a second to each source, and one a second
// to the multicast group.
type answerLimit struct {
	last  map[string]time.Time
	group time.Time
}

func newAnswerLimit() *answerLimit { return &answerLimit{last: map[string]time.Time{}} }

// answer decides whether the responder answers a query from src: a querier
// on this machine's networks (RFC 6762 section 11), within the rate limits.
// A query from port 5353 is answered to the group.
func (l *answerLimit) answer(src *net.UDPAddr, nets []*net.IPNet, now time.Time) bool {
	return onLAN(src.IP, nets) && l.allow(src.IP.String(), src.Port == mdnsPort, now)
}

func (l *answerLimit) allow(src string, toGroup bool, now time.Time) bool {
	if now.Sub(l.last[src]) < time.Second || (toGroup && now.Sub(l.group) < time.Second) {
		return false
	}
	if len(l.last) > 4096 { // forget, rather than grow with every spoofed source
		clear(l.last)
	}
	l.last[src] = now
	if toGroup {
		l.group = now
	}
	return true
}

// mdnsAnswer builds the answer to a query for the lmget service: PTR to
// the instance, then SRV, TXT and an A record for each of ips (those of the
// interface the query came in on). ok is false when the packet asks for
// something else, or there is no address to give.
func mdnsAnswer(msg []byte, instance string, port int, ips []net.IP) ([]byte, bool) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil || h.Response {
		return nil, false
	}
	qs, err := p.AllQuestions()
	if err != nil {
		return nil, false
	}
	asked := false
	var qcopy []dnsmessage.Question
	for _, q := range qs {
		if strings.EqualFold(q.Name.String(), lanService) && (q.Type == dnsmessage.TypePTR || q.Type == dnsmessage.TypeALL) {
			asked = true
			qcopy = append(qcopy, dnsmessage.Question{Name: q.Name, Type: q.Type, Class: dnsmessage.ClassINET})
		}
	}
	if !asked {
		return nil, false
	}
	svc := dnsmessage.MustNewName(lanService)
	inst := dnsmessage.MustNewName(instance + "." + lanService)
	host := dnsmessage.MustNewName(instance + ".local.")
	if len(ips) == 0 {
		return nil, false
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true})
	b.EnableCompression()
	ttl := uint32(120)
	in := dnsmessage.ClassINET
	if b.StartQuestions() != nil {
		return nil, false
	}
	if h.ID != 0 { // a one-shot (legacy) query: its question goes back with the answer
		for _, q := range qcopy {
			if b.Question(q) != nil {
				return nil, false
			}
		}
	}
	if b.StartAnswers() != nil ||
		b.PTRResource(dnsmessage.ResourceHeader{Name: svc, Class: in, TTL: ttl}, dnsmessage.PTRResource{PTR: inst}) != nil ||
		b.StartAdditionals() != nil ||
		b.SRVResource(dnsmessage.ResourceHeader{Name: inst, Class: in, TTL: ttl}, dnsmessage.SRVResource{Port: uint16(port), Target: host}) != nil ||
		b.TXTResource(dnsmessage.ResourceHeader{Name: inst, Class: in, TTL: ttl}, dnsmessage.TXTResource{TXT: []string{"v=1"}}) != nil {
		return nil, false
	}
	for _, ip := range ips {
		var a [4]byte
		copy(a[:], ip.To4())
		if b.AResource(dnsmessage.ResourceHeader{Name: host, Class: in, TTL: ttl}, dnsmessage.AResource{A: a}) != nil {
			return nil, false
		}
	}
	out, err := b.Finish()
	return out, err == nil
}

// --- discovering -----------------------------------------------------------------

// DiscoverLAN asks the local network for lmget servers, on every interface,
// and returns their base URLs (http://<ipv4>:<port>), sorted. It waits wait.
func DiscoverLAN(ctx context.Context, wait time.Duration) ([]string, error) {
	pc, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}
	defer pc.Close()
	p := ipv4.NewPacketConn(pc)
	var idb [2]byte
	_, _ = rand.Read(idb[:])
	id := binary.BigEndian.Uint16(idb[:]) | 1 // never 0: the answer must echo it
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(lanService), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET})
	q, err := b.Finish()
	if err != nil {
		return nil, err
	}
	group := &net.UDPAddr{IP: mdnsGroup, Port: mdnsPort}
	sent := 0
	for _, ifi := range lanInterfaces() {
		if p.SetMulticastInterface(&ifi) == nil {
			if _, err := p.WriteTo(q, nil, group); err == nil {
				sent++
			}
		}
	}
	if sent == 0 {
		if _, err := pc.WriteTo(q, group); err != nil {
			return nil, err
		}
	}
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = pc.SetReadDeadline(deadline)
	found := map[string]bool{}
	nets := lanNets()
	buf := make([]byte, 9000)
	for {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			break // the deadline: every answer that came is in
		}
		for _, o := range mdnsServers(buf[:n], id, nets) {
			found[o] = true
		}
	}
	return lanServerList(found), nil
}

// lanServerList is what discovery found, sorted, at most maxLANOrigins.
func lanServerList(found map[string]bool) []string {
	out := make([]string, 0, len(found))
	for o := range found {
		out = append(out, o)
	}
	sort.Strings(out)
	if len(out) > maxLANOrigins {
		out = out[:maxLANOrigins]
	}
	return out
}

// mdnsServers reads an answer to query id: the lmget instances it names,
// their port and their IPv4 addresses, as origin URLs. Only addresses on
// this machine's networks (nets): an answer comes from anyone on the LAN,
// and must not send lmget to this machine's own services, the cloud
// metadata address or the internet.
func mdnsServers(msg []byte, id uint16, nets []*net.IPNet) []string {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil || !h.Response || h.ID != id {
		return nil
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil
	}
	instances := map[string]bool{}
	ports := map[string]uint16{}   // instance -> port
	targets := map[string]string{} // instance -> host
	addrs := map[string][]net.IP{} // host -> addresses
	read := func(next func() (dnsmessage.ResourceHeader, error), skip func() error) {
		for {
			rh, err := next()
			if err != nil {
				return
			}
			name := strings.ToLower(rh.Name.String())
			switch rh.Type {
			case dnsmessage.TypePTR:
				r, err := p.PTRResource()
				if err == nil && name == lanService {
					instances[strings.ToLower(r.PTR.String())] = true
				}
			case dnsmessage.TypeSRV:
				if r, err := p.SRVResource(); err == nil {
					ports[name], targets[name] = r.Port, strings.ToLower(r.Target.String())
				}
			case dnsmessage.TypeA:
				if r, err := p.AResource(); err == nil {
					addrs[name] = append(addrs[name], net.IP(r.A[:]))
				}
			default:
				if skip() != nil {
					return
				}
			}
		}
	}
	read(p.AnswerHeader, p.SkipAnswer)
	_ = p.SkipAllAuthorities()
	read(p.AdditionalHeader, p.SkipAdditional)
	var out []string
	for inst := range instances {
		port, host := ports[inst], targets[inst]
		if port == 0 || host == "" {
			continue
		}
		for _, ip := range addrs[host] {
			if v4 := ip.To4(); v4 != nil && onLAN(v4, nets) {
				out = append(out, "http://"+net.JoinHostPort(v4.String(), strconv.Itoa(int(port))))
			}
		}
	}
	return out
}

// --- using what was found ----------------------------------------------------------

// lanOrigins returns the lmget servers on the local network, asked once per
// client when LAN discovery is on; nil otherwise.
func (c *Client) lanOrigins(ctx context.Context) []string {
	if !c.opt.LAN {
		return nil
	}
	c.lanOnce.Do(func() {
		found, err := DiscoverLAN(ctx, lanDiscoverFor)
		if err == nil {
			c.lanFound = found
		}
	})
	return c.lanFound
}
