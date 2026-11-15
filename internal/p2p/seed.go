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
	return &Seeder{cl: cl}, nil
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

// SeedIndex seeds every file of ix that has a torrent identity, whose blob is
// in the store, and whose model is on the license allowlist. It returns how
// many blobs it seeds and why the others are not.
func SeedIndex(st *lmget.Store, ix *lmget.Index, s *Seeder) (int, []error) {
	n := 0
	var errs []error
	seen := map[string]bool{}
	for _, m := range ix.Models {
		if !lmget.LicenseAllowed(m.License) {
			errs = append(errs, fmt.Errorf("%s: license %q is not on the allowlist", m.Ref, m.License))
			continue
		}
		for _, f := range m.Files {
			if f.BTIH2 == "" || seen[f.SHA256] {
				continue
			}
			if !st.Has(f.SHA256, f.Size) {
				errs = append(errs, fmt.Errorf("%s: %s is not mirrored", m.Ref, f.Path))
				continue
			}
			if _, err := s.Add(st.BlobPath(f.SHA256), f.SHA256, f.BTIH2); err != nil {
				errs = append(errs, fmt.Errorf("%s: %s: %v", m.Ref, f.Path, err))
				continue
			}
			seen[f.SHA256] = true
			n++
		}
	}
	return n, errs
}
