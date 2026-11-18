package lmget

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path"
	"strings"
)

// checkFormat looks at the content of a verified file before it enters the
// store. A matching hash proves the bytes are what the source publishes, not
// that they are what the name claims: a pickle renamed to .safetensors would
// pass the hash and still run code when loaded.
// declaredPickle is whether the plan treated the file as a pickle by name
// (pickle extension, not OpenVINO IR weights). Pickle content in any other
// file is refused, so a .bin next to a decoy .xml cannot slip through.
func checkFormat(p, name string, declaredPickle bool) error {
	f, err := os.Open(p)
	if err != nil {
		return diskErr(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return diskErr(err)
	}
	size := fi.Size()
	head := make([]byte, 16)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	bad := func(why string) error {
		return newErr(ExitVerify, "format", name+" is not what its name says").
			why("%s", why).
			fix("", "the file was discarded; report it to the publisher")
	}
	ext := strings.ToLower(path.Ext(name))
	if looksPickle(f, head, size, ext) && !declaredPickle {
		return bad("its content is a Python pickle, which runs code when loaded")
	}
	switch ext {
	case ".safetensors":
		if why := checkSafetensors(f, size); why != "" {
			return bad(why)
		}
	case ".gguf":
		if why := checkGGUF(f, size); why != "" {
			return bad(why)
		}
	}
	return nil
}

// textExt are formats no loader unpickles; the opcode walk skips them, so a
// word list is never read as opcodes.
var textExt = map[string]bool{".txt": true, ".json": true, ".md": true, ".yaml": true, ".yml": true,
	".jinja": true, ".html": true, ".csv": true, ".tsv": true, ".cfg": true, ".ini": true, ".toml": true, ".py": true}

// looksPickle recognises a pickle of any protocol (0 to 5) by walking its
// opcodes, and the zip archives torch.save writes. A zip Go cannot read is
// treated as one: a loader with another zip parser might still open it.
func looksPickle(f *os.File, head []byte, size int64, ext string) bool {
	if len(head) >= 4 && string(head[:4]) == "PK\x03\x04" {
		zr, err := readZip(f, size)
		if err != nil {
			return true
		}
		for _, zf := range zr.File {
			if strings.HasSuffix(strings.ToLower(zf.Name), ".pkl") {
				return true
			}
		}
		return false
	}
	if textExt[ext] {
		return false
	}
	if zipAtTail(f, size) {
		return true
	}
	if len(head) >= 2 && head[0] == 0x80 && head[1] >= 2 && head[1] <= 5 {
		return true // a PROTO header: treated as a pickle even if the rest is malformed
	}
	return walkPickle(io.NewSectionReader(f, 0, size), size)
}

// zipAtTail reports a zip archive appended to a file that starts as
// something else (a GGUF or safetensors header, say): zip readers find an
// archive from its end, so a loader that opens zips could find a pickle in
// it. When the tail holds an end-of-archive signature, Go's zip reader is
// asked first: an archive it reads counts when it holds a .pkl entry. One it
// cannot read counts when another reader would still follow it (see
// tailLooksLikeArchive): the same rule as a file that starts with a zip. The
// four bytes of the signature alone also occur by chance in weights and do
// not count.
func zipAtTail(f *os.File, size int64) bool {
	const maxTail = 22 + 65535 // end of central directory record and its longest comment
	n := min(size, maxTail)
	if n < 22 {
		return false
	}
	tail := make([]byte, n)
	if _, err := f.ReadAt(tail, size-n); err != nil {
		return false
	}
	if !bytes.Contains(tail, []byte("PK\x05\x06")) {
		return false
	}
	// Go's reader first: it finds an end record followed by trailing bytes too
	if zr, err := readZip(f, size); err == nil {
		for _, zf := range zr.File {
			if strings.HasSuffix(strings.ToLower(zf.Name), ".pkl") {
				return true
			}
		}
		return false
	}
	return tailLooksLikeArchive(f, tail, size-n)
}

// tailLooksLikeArchive reports an end record Go cannot read that another zip
// reader would still follow: its comment ends at the end of the file, or the
// central directory it names (by declared offset, or by its size counted back
// from the record, as Python's zipfile does) starts with a PK\x01\x02 header.
// A chance end signature in weights has neither.
func tailLooksLikeArchive(f *os.File, tail []byte, base int64) bool {
	sig := make([]byte, 4)
	for i := bytes.LastIndex(tail, []byte("PK\x05\x06")); i >= 0; i = bytes.LastIndex(tail[:i], []byte("PK\x05\x06")) {
		if i+22 > len(tail) {
			continue
		}
		if int(binary.LittleEndian.Uint16(tail[i+20:i+22])) == len(tail)-i-22 {
			return true
		}
		end := base + int64(i)
		cdSize := int64(binary.LittleEndian.Uint32(tail[i+12:]))
		cdOff := int64(binary.LittleEndian.Uint32(tail[i+16:]))
		for _, at := range []int64{end - cdSize, cdOff} {
			if at >= 0 && at+4 <= end {
				if _, err := f.ReadAt(sig, at); err == nil && string(sig) == "PK\x01\x02" {
					return true
				}
			}
		}
	}
	return false
}

// zipReadBudget bounds what Go's zip reader may read to list an archive. It
// keeps every central directory entry in memory (about 4 bytes of heap per
// byte read), and reads entries until the first that is not one, whatever
// the end record declares: an archive past this budget counts as one Go
// cannot read, which is a pickle. A torch.save archive lists one entry per
// tensor storage, tens of KB for real checkpoints (audit run 6).
const zipReadBudget = 16 << 20

var errZipTooLarge = errors.New("zip directory larger than the read budget")

type budgetReader struct {
	r    io.ReaderAt
	left int64
}

func (b *budgetReader) ReadAt(p []byte, off int64) (int, error) {
	if int64(len(p)) > b.left {
		return 0, errZipTooLarge
	}
	b.left -= int64(len(p))
	return b.r.ReadAt(p, off)
}

// readZip opens an archive with Go's reader within zipReadBudget.
func readZip(f io.ReaderAt, size int64) (*zip.Reader, error) {
	return zip.NewReader(&budgetReader{r: f, left: zipReadBudget}, size)
}

// pickle opcode argument kinds
const (
	argNone     = iota
	argLine     // up to and including a newline
	argTwoLines // GLOBAL and INST: module and name
	argFixed    // n bytes
	argLen1     // 1-byte length, then data
	argLen4     // 4-byte little-endian length, then data
	argLen8     // 8-byte little-endian length, then data
)

type pickleOp struct {
	kind int
	n    int  // argFixed size
	code bool // imports or calls code
}

// pickleOps covers every opcode of protocols 0 to 5 (Python's pickletools).
var pickleOps = map[byte]pickleOp{
	'(': {}, '.': {}, '0': {}, '1': {}, '2': {}, 'a': {}, 'b': {}, 'd': {}, 'e': {}, 'l': {},
	's': {}, 't': {}, 'u': {}, ')': {}, ']': {}, '}': {}, 'N': {},
	0x85: {}, 0x86: {}, 0x87: {}, 0x88: {}, 0x89: {}, 0x8f: {}, 0x90: {}, 0x91: {}, 0x94: {}, 0x97: {}, 0x98: {},
	'I': {kind: argLine}, 'L': {kind: argLine}, 'F': {kind: argLine}, 'S': {kind: argLine}, 'V': {kind: argLine},
	'P': {kind: argLine}, 'p': {kind: argLine}, 'g': {kind: argLine},
	'c': {kind: argTwoLines, code: true}, 'i': {kind: argTwoLines, code: true},
	'R': {code: true}, 'o': {code: true}, 0x81: {code: true}, 0x92: {code: true}, 0x93: {code: true}, 'Q': {code: true},
	'J': {kind: argFixed, n: 4}, 'K': {kind: argFixed, n: 1}, 'M': {kind: argFixed, n: 2}, 'G': {kind: argFixed, n: 8},
	'h': {kind: argFixed, n: 1}, 'j': {kind: argFixed, n: 4}, 'q': {kind: argFixed, n: 1}, 'r': {kind: argFixed, n: 4},
	0x80: {kind: argFixed, n: 1}, 0x95: {kind: argFixed, n: 8},
	0x82: {kind: argFixed, n: 1, code: true}, 0x83: {kind: argFixed, n: 2, code: true}, 0x84: {kind: argFixed, n: 4, code: true},
	'U': {kind: argLen1}, 'C': {kind: argLen1}, 0x8c: {kind: argLen1}, 0x8a: {kind: argLen1},
	'T': {kind: argLen4}, 'X': {kind: argLen4}, 'B': {kind: argLen4}, 0x8b: {kind: argLen4},
	0x8d: {kind: argLen8}, 0x8e: {kind: argLen8}, 0x96: {kind: argLen8},
}

// walkPickle reports whether r starts with a well-formed opcode stream that
// reaches STOP and imports or calls code, or that is still well-formed after
// the opcode limit. Bytes after STOP are ignored, as
// pickle.load ignores them.
func walkPickle(r *io.SectionReader, size int64) bool {
	const maxLine = 1 << 16
	var off int64
	one := make([]byte, 1)
	readLine := func() bool {
		for n := 0; n < maxLine; n++ {
			if _, err := r.ReadAt(one, off); err != nil {
				return false
			}
			off++
			if one[0] == '\n' {
				return true
			}
		}
		return false
	}
	code, steps := false, 0
	for steps < 1<<20 {
		if _, err := r.ReadAt(one, off); err != nil {
			return false
		}
		op, ok := pickleOps[one[0]]
		if !ok {
			return false
		}
		off++
		steps++
		code = code || op.code
		var n int64
		switch op.kind {
		case argLine:
			if !readLine() {
				return false
			}
		case argTwoLines:
			if !readLine() || !readLine() {
				return false
			}
		case argFixed:
			n = int64(op.n)
		case argLen1, argLen4, argLen8:
			w := map[int]int{argLen1: 1, argLen4: 4, argLen8: 8}[op.kind]
			buf := make([]byte, 8)
			if _, err := r.ReadAt(buf[:w], off); err != nil {
				return false
			}
			off += int64(w)
			n = int64(binary.LittleEndian.Uint64(buf))
			if n < 0 {
				return false
			}
		}
		if n > size-off {
			return false
		}
		off += n
		if one[0] == '.' { // STOP
			return code && steps > 1
		}
	}
	// a million well-formed opcodes and still no STOP: no data format reads
	// like that, and a pickle can pad its harmless start to push the call it
	// makes past any limit, so this counts as one
	return true
}
