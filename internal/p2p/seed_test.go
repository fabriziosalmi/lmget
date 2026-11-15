package p2p

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/merkle"
	"github.com/anacrolix/torrent/metainfo"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"

	"github.com/fabriziosalmi/lmget"
)

func blob(t *testing.T, n int) (string, string) {
	t.Helper()
	b := make([]byte, n)
	rand.Read(b)
	s := sha256.Sum256(b)
	p := filepath.Join(t.TempDir(), hex.EncodeToString(s[:]))
	if err := os.WriteFile(p, b, 0o444); err != nil {
		t.Fatal(err)
	}
	return p, hex.EncodeToString(s[:])
}

// anacrolixTorrent builds the same one-blob v2 torrent with anacrolix's merkle
// and bencode code (the M0 spike showed anacrolix's piece roots equal
// libtorrent's).
func anacrolixTorrent(t *testing.T, path, sha string) (string, []byte, map[string]string) {
	t.Helper()
	f, _ := os.Open(path)
	defer f.Close()
	pl := int64(lmget.BTPieceLength)
	buf := make([]byte, pl)
	all := merkle.NewHash()
	var pieces [][32]byte
	var size int64
	for {
		n, err := io.ReadFull(f, buf)
		if n > 0 {
			size += int64(n)
			all.Write(buf[:n])
			ph := merkle.NewHash()
			ph.Write(buf[:n])
			var h [32]byte
			copy(h[:], ph.SumMinLength(nil, int(pl)))
			pieces = append(pieces, h)
		}
		if err != nil {
			break
		}
	}
	var root [32]byte
	copy(root[:], all.Sum(nil))
	leaf := metainfo.FileTree{File: metainfo.FileTreeFile{Length: size, PiecesRoot: string(root[:])}}
	info := metainfo.Info{Name: sha, PieceLength: pl, MetaVersion: 2,
		FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{sha: leaf}}}
	ib, err := bencode.Marshal(&info)
	if err != nil {
		t.Fatal(err)
	}
	layers := map[string]string{}
	if size > pl {
		var l []byte
		for _, p := range pieces {
			l = append(l, p[:]...)
		}
		layers[string(root[:])] = string(l)
	}
	ih := sha256.Sum256(ib)
	return hex.EncodeToString(ih[:]), ib, layers
}

func TestInfohashMatchesAnacrolix(t *testing.T) {
	for _, n := range []int{1, 16 << 10, 16<<10 + 1, 4 << 20, 4<<20 + 1, 9<<20 + 12345} {
		p, sha := blob(t, n)
		bt, err := lmget.NewBlobTorrent(p, sha)
		if err != nil {
			t.Fatal(err)
		}
		ih, ib, layers := anacrolixTorrent(t, p, sha)
		if bt.Infohash != ih || string(bt.InfoBytes) != string(ib) {
			t.Errorf("%d bytes: lmget %s, anacrolix %s", n, bt.Infohash[:16], ih[:16])
		}
		if len(bt.PieceLayers) != len(layers) {
			t.Errorf("%d bytes: %d piece layers, anacrolix %d", n, len(bt.PieceLayers), len(layers))
		}
		for k, v := range layers {
			if bt.PieceLayers[k] != v {
				t.Errorf("%d bytes: piece layer differs", n)
			}
		}
	}
	other, _ := blob(t, 10)
	_, otherSHA := blob(t, 11) // a well-formed sha256 of other bytes
	if _, err := lmget.NewBlobTorrent(other, otherSHA); err == nil {
		t.Error("a blob that does not match its sha256 was turned into a torrent")
	}
}

func TestSeederServesALeecher(t *testing.T) {
	p, sha := blob(t, 9<<20+777) // three pieces, the last one short
	bt, _ := lmget.NewBlobTorrent(p, sha)
	s, err := New(Config{NoDHT: true, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Add(p, sha, bt.Infohash[:63]+"0"); err == nil {
		t.Fatal("a blob was seeded under an infohash the index does not give")
	}
	if _, err := s.Add(p, sha, bt.Infohash); err != nil {
		t.Fatal(err)
	}

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.NoDHT, cfg.DisableTrackers, cfg.ListenPort, cfg.NoDefaultPortForwarding = true, true, 0, true
	leech, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer leech.Close()
	raw, _ := hex.DecodeString(bt.Infohash)
	var ih infohash_v2.T
	copy(ih[:], raw)
	tt, _, err := leech.AddTorrentSpec(&torrent.TorrentSpec{AddTorrentOpts: torrent.AddTorrentOpts{
		InfoHashV2: g.Some(ih), InfoBytes: bt.InfoBytes, PieceLayers: bt.PieceLayers}})
	if err != nil {
		t.Fatal(err)
	}
	<-tt.GotInfo()
	tt.AddPeers([]torrent.PeerInfo{{Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: s.Client().LocalPort()}, Trusted: true}})
	tt.DownloadAll()
	select {
	case <-tt.Complete().On():
	case <-time.After(60 * time.Second):
		t.Fatalf("download stuck at %d of %d bytes", tt.BytesCompleted(), tt.Length())
	}
	got, err := os.ReadFile(filepath.Join(cfg.DataDir, sha, sha))
	if err != nil {
		t.Fatal(err)
	}
	if s := sha256.Sum256(got); hex.EncodeToString(s[:]) != sha {
		t.Fatal("the leecher received other bytes")
	}
}

func TestSeedIndexSeedsOnlyWhatItShould(t *testing.T) {
	st, err := lmget.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	put := func(n int) (lmget.IndexFile, *lmget.BlobTorrent) {
		p, sha := blob(t, n)
		b, _ := os.ReadFile(p)
		os.MkdirAll(filepath.Dir(st.BlobPath(sha)), 0o755)
		os.WriteFile(st.BlobPath(sha), b, 0o444)
		bt, _ := lmget.NewBlobTorrent(p, sha)
		return lmget.IndexFile{LockFile: lmget.LockFile{Path: "f" + sha[:6], Size: int64(n), SHA256: sha}, BTIH2: bt.Infohash}, bt
	}
	ok, _ := put(100_000)
	wrong, _ := put(50_000)
	wrong.BTIH2 = ok.BTIH2 // an infohash that is not this blob's
	_, sha := blob(t, 10)  // indexed, never mirrored into this store
	absent := lmget.IndexFile{LockFile: lmget.LockFile{Path: "absent", Size: 10, SHA256: sha}, BTIH2: ok.BTIH2}
	gated, _ := put(20_000)
	ix := &lmget.Index{Models: []lmget.IndexModel{
		{Ref: "hf:org/a", License: "apache-2.0", Files: []lmget.IndexFile{ok, wrong, absent}},
		{Ref: "hf:org/b", License: "llama3", Files: []lmget.IndexFile{gated}},
	}}
	s, err := New(Config{NoDHT: true, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n, errs := SeedIndex(st, ix, s)
	if n != 1 || len(errs) != 3 {
		t.Fatalf("seeded %d, %d refusals (%v); want 1 seeded, 3 refused", n, len(errs), errs)
	}
	said := false
	for _, e := range errs {
		said = said || strings.Contains(e.Error(), "absent is not mirrored")
	}
	if !said {
		t.Fatalf("the blob missing from the store is not reported as such: %v", errs)
	}
}
