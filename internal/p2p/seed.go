// Package p2p seeds lmget's blobs as BitTorrent v2 torrents, one torrent per
// blob. It is used by lmget-node only: the lmget CLI downloads from nodes over
// HTTP and does not link a torrent client.
package p2p

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"

	"github.com/fabriziosalmi/lmget"
)

// Config configures a Seeder.
type Config struct {
	ListenPort int    // 0 picks a free port
	NoDHT      bool   // announce on the Mainline DHT unless set
	DataDir    string // client state; blobs are read from the store, never written
}

// Seeder serves blobs to BitTorrent v2 peers.
type Seeder struct {
	cl *torrent.Client
	on map[string]seeded // seeded blobs by sha256
}

type seeded struct {
	t  *torrent.Torrent
	ih string // the v2 infohash it is seeded under
}

// New starts a seeding client.
func New(c Config) (*Seeder, error) {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = c.DataDir
	cfg.Seed = true
	cfg.NoDHT = c.NoDHT
	cfg.DisableTrackers = true
	cfg.ListenPort = c.ListenPort
	cfg.NoDefaultPortForwarding = true
	cfg.DefaultStorage = readOnly{} // a torrent without its blob writes nothing
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Seeder{cl: cl, on: map[string]seeded{}}, nil
}

// Close stops seeding.
func (s *Seeder) Close() { s.cl.Close() }

// Client exposes the torrent client (listen addresses, stats).
func (s *Seeder) Client() *torrent.Client { return s.cl }

// Add seeds the blob at path, whose sha256 is sha, after checking that its v2
// infohash is the one the signed index publishes.
func (s *Seeder) Add(path, sha, wantInfohash string) (*torrent.Torrent, error) {
	bt, err := lmget.NewBlobTorrent(path, sha)
	if err != nil {
		return nil, err
	}
	if bt.Infohash != wantInfohash {
		return nil, fmt.Errorf("blob %s: infohash %s, the index says %s", sha[:12], bt.Infohash[:12], wantInfohash)
	}
	raw, err := hex.DecodeString(bt.Infohash)
	if err != nil {
		return nil, err
	}
	var ih infohash_v2.T
	copy(ih[:], raw)
	spec := &torrent.TorrentSpec{AddTorrentOpts: torrent.AddTorrentOpts{
		InfoHashV2: g.Some(ih), InfoBytes: bt.InfoBytes, PieceLayers: bt.PieceLayers,
		Storage: blobStorage{path: path},
	}}
	t, _, err := s.cl.AddTorrentSpec(spec)
	if err != nil {
		return nil, err
	}
	<-t.GotInfo()
	return t, nil
}

// blobStorage reads one verified blob of the store. Its pieces are complete
// by construction (the blob matched its sha256 when it entered the store), and
// nothing is ever written to it.
type blobStorage struct{ path string }

func (b blobStorage) OpenTorrent(ctx context.Context, info *metainfo.Info, ih metainfo.Hash) (storage.TorrentImpl, error) {
	f, err := os.Open(b.path)
	if err != nil {
		return storage.TorrentImpl{}, err
	}
	return storage.TorrentImpl{
		Piece: func(p metainfo.Piece) storage.PieceImpl { return blobPiece{f: f, off: p.Offset()} },
		Close: f.Close,
	}, nil
}

type blobPiece struct {
	f   *os.File
	off int64
}

var errReadOnly = errors.New("lmget blobs are read-only")

func (p blobPiece) ReadAt(b []byte, off int64) (int, error) { return p.f.ReadAt(b, p.off+off) }
func (p blobPiece) WriteAt([]byte, int64) (int, error)      { return 0, errReadOnly }
func (p blobPiece) MarkComplete() error                     { return nil }
func (p blobPiece) MarkNotComplete() error                  { return nil }
func (p blobPiece) Completion() storage.Completion {
	return storage.Completion{Ok: true, Complete: true}
}

type readOnly struct{}

func (readOnly) OpenTorrent(context.Context, *metainfo.Info, metainfo.Hash) (storage.TorrentImpl, error) {
	return storage.TorrentImpl{}, errReadOnly
}

// SeedIndex makes the seeder serve exactly the blobs of ix: every blob of a
// model whose license is on the allowlist, mirrored in st, under the
// infohash the index signs. Blobs seeded by an earlier call that ix no
// longer names are dropped; a nil ix (no valid index) drops them all. It
// returns how many blobs are seeded and why the others are not.
func SeedIndex(st *lmget.Store, ix *lmget.Index, s *Seeder) (int, []error) {
	var errs []error
	want := map[string]bool{}
	if ix != nil {
		for _, m := range ix.Models {
			if !lmget.LicenseAllowed(m.License) {
				errs = append(errs, fmt.Errorf("%s: license %q is not on the allowlist", m.Ref, m.License))
				continue
			}
			for _, f := range m.Files {
				if f.BTIH2 == "" || want[f.SHA256] {
					continue
				}
				if s.on[f.SHA256].ih == f.BTIH2 {
					want[f.SHA256] = true // already seeded under this infohash
					continue
				}
				if !st.Has(f.SHA256, f.Size) {
					errs = append(errs, fmt.Errorf("%s: %s is not mirrored", m.Ref, f.Path))
					continue
				}
				t, err := s.Add(st.BlobPath(f.SHA256), f.SHA256, f.BTIH2)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %s: %v", m.Ref, f.Path, err))
					continue
				}
				if old := s.on[f.SHA256].t; old != nil && old != t {
					old.Drop()
				}
				s.on[f.SHA256] = seeded{t, f.BTIH2}
				want[f.SHA256] = true
			}
		}
	}
	for sha, b := range s.on {
		if !want[sha] {
			b.t.Drop()
			delete(s.on, sha)
		}
	}
	return len(s.on), errs
}
