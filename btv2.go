package lmget

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
)

// BitTorrent v2 (BEP 52) identity of a blob, computed here without a torrent
// library so the CLI stays small. Every blob is its own torrent: name and
// file key are its sha256, and the piece length is fixed, so the v2 infohash
// depends only on the content. lmget-node seeds these torrents; the signed
// index publishes their infohashes.

const (
	btBlock       = 16 << 10 // BEP 52 leaf block
	BTPieceLength = 4 << 20  // fixed: the infohash must not depend on a choice
)

// BlobTorrent is the v2 torrent of one blob.
type BlobTorrent struct {
	Infohash    string            // hex of sha256(InfoBytes)
	InfoBytes   []byte            // bencoded info dictionary
	PieceLayers map[string]string // pieces root -> concatenated piece hashes (files over one piece)
}

// NewBlobTorrent hashes the blob at path, whose sha256 is sha, into its v2
// torrent. Memory: 32 bytes per 16 KiB of content.
func NewBlobTorrent(path, sha string) (*BlobTorrent, error) {
	if !hex64.MatchString(sha) {
		return nil, fmt.Errorf("not a sha256: %q", sha)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var leaves [][32]byte
	content := sha256.New()
	buf := make([]byte, btBlock)
	var size int64
	for {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			content.Write(buf[:n])
			leaves = append(leaves, sha256.Sum256(buf[:n]))
			size += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	if hex.EncodeToString(content.Sum(nil)) != sha {
		return nil, fmt.Errorf("%s does not hash to %s", path, sha[:12])
	}
	bt := &BlobTorrent{PieceLayers: map[string]string{}}
	var root [32]byte
	if size > 0 {
		root = merkleRoot(leaves, nextPow2(len(leaves)), [32]byte{})
		if size > BTPieceLength {
			per := BTPieceLength / btBlock
			var layer []byte
			for i := 0; i < len(leaves); i += per {
				ph := merkleRoot(leaves[i:min(i+per, len(leaves))], per, [32]byte{})
				layer = append(layer, ph[:]...)
			}
			bt.PieceLayers[string(root[:])] = string(layer)
		}
	}
	leaf := map[string]any{"length": size}
	if size > 0 {
		leaf["pieces root"] = string(root[:])
	}
	info := map[string]any{
		"file tree":    map[string]any{sha: map[string]any{"": leaf}},
		"meta version": int64(2),
		"name":         sha,
		"piece length": int64(BTPieceLength),
	}
	var b bytes.Buffer
	bencode(&b, info)
	bt.InfoBytes = b.Bytes()
	ih := sha256.Sum256(bt.InfoBytes)
	bt.Infohash = hex.EncodeToString(ih[:])
	return bt, nil
}

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// merkleRoot hashes leaves padded with pad up to width (a power of two).
func merkleRoot(leaves [][32]byte, width int, pad [32]byte) [32]byte {
	level := make([][32]byte, width)
	copy(level, leaves)
	for i := len(leaves); i < width; i++ {
		level[i] = pad
	}
	for len(level) > 1 {
		next := make([][32]byte, len(level)/2)
		for i := range next {
			next[i] = sha256.Sum256(append(level[2*i][:], level[2*i+1][:]...))
		}
		level = next
	}
	return level[0]
}

// bencode writes v (string, int64, map[string]any) in canonical form.
func bencode(w *bytes.Buffer, v any) {
	switch t := v.(type) {
	case string:
		w.WriteString(strconv.Itoa(len(t)) + ":" + t)
	case int64:
		w.WriteString("i" + strconv.FormatInt(t, 10) + "e")
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // bencode sorts keys as raw bytes
		w.WriteByte('d')
		for _, k := range keys {
			bencode(w, k)
			bencode(w, t[k])
		}
		w.WriteByte('e')
	default:
		panic(fmt.Sprintf("bencode: %T", v))
	}
}
