package lmget

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"sort"
	"strings"
	"unicode/utf8"
)

// The structural checks below walk the header of a weights file the way the
// reference loader does, and refuse what that loader refuses: a header whose
// counts, lengths or offsets point past the file, or overflow, is how a
// published file attacks the parser that opens it. A matching hash does not
// help there: it proves the bytes are the publisher's, not that they are sane.
// What a newer format adds (a dtype or a tensor type this table does not know)
// is not refused: the walk only stops sizing what it cannot size.

// stBits is the size in bits of each safetensors dtype
// (huggingface/safetensors, Dtype::bitsize).
var stBits = map[string]uint64{
	"BOOL": 8, "F4": 4, "F6_E2M3": 6, "F6_E3M2": 6, "U8": 8, "I8": 8,
	"F8_E5M2": 8, "F8_E4M3": 8, "F8_E8M0": 8, "F8_E4M3FNUZ": 8, "F8_E5M2FNUZ": 8,
	"I16": 16, "U16": 16, "F16": 16, "BF16": 16, "I32": 32, "U32": 32, "F32": 32,
	"C64": 64, "F64": 64, "I64": 64, "U64": 64,
}

const stMaxHeader = 100_000_000 // safetensors MAX_HEADER_SIZE

// checkSafetensors mirrors SafeTensors::read_metadata and Metadata::validate:
// the header is one JSON object of tensors (and an optional __metadata__ map
// of strings), each tensor's bytes are its shape times its dtype, and the
// tensors, sorted by offset, tile the data section exactly, with nothing
// before, between or after them. It also refuses a key given twice, where
// two JSON parsers can disagree about which value counts.
func checkSafetensors(r io.ReaderAt, size int64) string {
	if size < 8 {
		return "too short for a safetensors header"
	}
	var nb [8]byte
	if _, err := r.ReadAt(nb[:], 0); err != nil {
		return "the safetensors header cannot be read"
	}
	n := binary.LittleEndian.Uint64(nb[:])
	if n > stMaxHeader || n > uint64(size-8) {
		return fmt.Sprintf("the safetensors header claims %d bytes, past the end of the file or over 100 MB", n)
	}
	hdr := make([]byte, n)
	if _, err := r.ReadAt(hdr, 8); err != nil {
		return "the safetensors header cannot be read"
	}
	if !utf8.Valid(hdr) {
		return "the safetensors header is not UTF-8"
	}
	type tensor struct {
		name string
		s, e uint64
	}
	var ts []tensor
	dec := json.NewDecoder(bytes.NewReader(hdr))
	err := jsonObject(dec, func(key string) error {
		if key == "__metadata__" {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return err
			}
			var m map[string]string // null is read as no metadata, as the reference does
			if json.Unmarshal(raw, &m) != nil {
				return errors.New("__metadata__ is not a map of strings")
			}
			return nil
		}
		var dtype string
		var shape, offs []uint64
		var have [3]bool
		err := jsonObject(dec, func(k string) error {
			var dst any
			switch k {
			case "dtype":
				dst, have[0] = &dtype, true
			case "shape":
				dst, have[1] = &shape, true
			case "data_offsets":
				dst, have[2] = &offs, true
			default:
				dst = new(json.RawMessage)
			}
			return dec.Decode(dst)
		})
		if err != nil || !have[0] || !have[1] || !have[2] || shape == nil || len(offs) != 2 {
			return fmt.Errorf("tensor %s has no valid dtype, shape and data_offsets", quote(key))
		}
		if offs[1] < offs[0] {
			return fmt.Errorf("tensor %s ends before it starts", quote(key))
		}
		if b, ok := stBits[dtype]; ok {
			nbits, zero, over := b, false, false
			for _, d := range shape {
				if d == 0 {
					zero = true
					continue
				}
				hi, lo := bits.Mul64(nbits, d)
				over = over || hi != 0
				nbits = lo
			}
			switch {
			case over:
				return fmt.Errorf("the shape of tensor %s overflows", quote(key))
			case zero:
				nbits = 0
			case nbits%8 != 0:
				return fmt.Errorf("tensor %s does not end on a byte", quote(key))
			}
			if offs[1]-offs[0] != nbits/8 {
				return fmt.Errorf("tensor %s holds %d bytes, its shape and dtype need %d", quote(key), offs[1]-offs[0], nbits/8)
			}
		}
		ts = append(ts, tensor{key, offs[0], offs[1]})
		return nil
	})
	if err != nil {
		return "the safetensors header is malformed: " + err.Error()
	}
	if _, err := dec.Token(); err != io.EOF {
		return "the safetensors header has data after its JSON object"
	}
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].s != ts[j].s {
			return ts[i].s < ts[j].s
		}
		return ts[i].e < ts[j].e
	})
	var end uint64
	for _, t := range ts {
		if t.s != end {
			return fmt.Sprintf("tensor %s does not start where the previous one ends", quote(t.name))
		}
		end = t.e
	}
	if end != uint64(size)-8-n {
		return fmt.Sprintf("the tensors cover %d bytes, the file holds %d after the header", end, uint64(size)-8-n)
	}
	return ""
}

// jsonObject reads one JSON object from dec, calling each with every key,
// once; each must consume the value. A key given twice is an error.
func jsonObject(dec *json.Decoder, each func(key string) error) error {
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errors.New("not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		key := t.(string)
		if seen[key] {
			return fmt.Errorf("%s is given twice", quote(key))
		}
		seen[key] = true
		if err := each(key); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	return nil
}

// ggmlTypes is each ggml tensor type's elements per block and bytes per block
// (llama.cpp gguf-py GGML_QUANT_SIZES), plus 31 to 33 (Q4_0_4_4, Q4_0_4_8, Q4_0_8_8):
// llama.cpp removed them, files with them are still published, and their
// blocks are Q4_0's (measured: their offsets tile real files exactly). A type
// missing here is not refused, only not sized: the walk stops placing tensors.
var ggmlTypes = map[uint32][2]uint64{
	0: {1, 4}, 1: {1, 2}, 2: {32, 18}, 3: {32, 20}, 6: {32, 22}, 7: {32, 24}, 8: {32, 34}, 9: {32, 36},
	10: {256, 84}, 11: {256, 110}, 12: {256, 144}, 13: {256, 176}, 14: {256, 210}, 15: {256, 292},
	16: {256, 66}, 17: {256, 74}, 18: {256, 98}, 19: {256, 50}, 20: {32, 18}, 21: {256, 110},
	22: {256, 82}, 23: {256, 136}, 24: {1, 1}, 25: {1, 2}, 26: {1, 4}, 27: {1, 8}, 28: {1, 8},
	29: {256, 56}, 30: {1, 2}, 34: {256, 54}, 35: {256, 66}, 39: {32, 17}, 40: {64, 36},
	41: {128, 18}, 42: {64, 18}, 43: {1, 1}, 31: {32, 18}, 32: {32, 18}, 33: {32, 18},
}

// gguf value types and the size of the fixed-size ones (gguf.h)
const (
	ggufString = 8
	ggufArray  = 9
)

// What the walk keeps in memory is bounded by these, not by the file: real
// GGUF files carry tens of keys and at most a few thousand tensors, and
// llama.cpp itself keeps a full tensor per entry (audit run 6).
const (
	ggufMaxTensors  = 1 << 16
	ggufMaxKV       = 1 << 16
	ggufMaxKeyBytes = 16 << 20 // key strings kept for the duplicate check
)

var ggufScalar = map[uint32]uint64{0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 7: 1, 10: 8, 11: 8, 12: 8}

// ggufReader reads a GGUF header, knowing how much of the file is left.
type ggufReader struct {
	r      *bufio.Reader
	pos    uint64
	size   uint64
	failed string
}

func (g *ggufReader) fail(format string, a ...any) {
	if g.failed == "" {
		g.failed = fmt.Sprintf(format, a...)
	}
}

func (g *ggufReader) remain() uint64 { return g.size - g.pos }

func (g *ggufReader) read(n uint64) []byte {
	if g.failed != "" {
		return nil
	}
	if n > g.remain() {
		g.fail("the GGUF header runs past the end of the file")
		return nil
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(g.r, b); err != nil {
		g.fail("the GGUF header cannot be read")
		return nil
	}
	g.pos += n
	return b
}

func (g *ggufReader) skip(n uint64) {
	if g.failed != "" {
		return
	}
	if n > g.remain() {
		g.fail("the GGUF header runs past the end of the file")
		return
	}
	if _, err := g.r.Discard(int(n)); err != nil {
		g.fail("the GGUF header cannot be read")
		return
	}
	g.pos += n
}

func (g *ggufReader) u32() uint32 {
	if b := g.read(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (g *ggufReader) u64() uint64 {
	if b := g.read(8); b != nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

// str reads a GGUF string, at most 1 GiB (GGUF_MAX_STRING_LENGTH), and
// returns it when it is at most keep bytes; a longer one fails the walk with
// tooLong, so a huge name is refused before it is read into memory.
func (g *ggufReader) str(keep uint64, tooLong string) string {
	n := g.u64()
	switch {
	case g.failed != "":
		return ""
	case n > 1<<30:
		g.fail("a GGUF string claims %d bytes", n)
		return ""
	case keep == 0:
		g.skip(n)
		return ""
	case n > keep:
		g.fail("%s", tooLong)
		return ""
	}
	return string(g.read(n))
}

// value skips one value of type t, the way gguf_init_from_file_impl reads it.
func (g *ggufReader) value(t uint32, key string) {
	if sz, ok := ggufScalar[t]; ok {
		g.skip(sz)
		return
	}
	switch t {
	case ggufString:
		g.str(0, "")
	case ggufArray:
		et, n := g.u32(), g.u64()
		if g.failed != "" {
			return
		}
		if n > 1<<30 { // GGUF_MAX_ARRAY_ELEMENTS
			g.fail("the array %s claims %d elements", quote(key), n)
			return
		}
		if sz, ok := ggufScalar[et]; ok {
			if n > g.remain()/sz {
				g.fail("the array %s runs past the end of the file", quote(key))
				return
			}
			g.skip(n * sz)
			return
		}
		if et != ggufString {
			g.fail("the array %s has element type %d; llama.cpp reads no nested arrays", quote(key), et)
			return
		}
		if n > g.remain()/8 {
			g.fail("the array %s runs past the end of the file", quote(key))
			return
		}
		for i := uint64(0); i < n && g.failed == ""; i++ {
			g.str(0, "")
		}
	default:
		g.fail("the key %s has an unknown GGUF type %d", quote(key), t)
	}
}

// checkGGUF mirrors gguf_init_from_file_impl in llama.cpp for versions 2 and
// 3: bounded counts and strings, no empty or repeated keys, no nested arrays,
// a power-of-two alignment, tensor names under 64 bytes and not repeated, at
// most 4 dimensions without overflow, rows that are whole blocks, and tensors
// laid out one after the other from the aligned start of the data section,
// each within the file. Other versions are not walked: no maintained loader
// reads version 1, and a newer one is for a newer loader to judge.
func checkGGUF(r io.ReaderAt, size int64) string {
	if size < 24 {
		return "too short for a GGUF header"
	}
	g := &ggufReader{r: bufio.NewReaderSize(io.NewSectionReader(r, 0, size), 1<<16), size: uint64(size)}
	if string(g.read(4)) != "GGUF" {
		return "it does not start with the GGUF magic"
	}
	if v := g.u32(); v != 2 && v != 3 {
		return ""
	}
	nTensors, nKV := g.u64(), g.u64()
	// a KV takes at least 14 bytes (key length, one byte of key, type, one
	// byte of value), a tensor info at least 25: counts beyond what the file
	// can hold are refused before any loop runs on them
	if nTensors > g.remain()/25 || nKV > g.remain()/14 || nTensors*25+nKV*14 > g.remain() {
		return fmt.Sprintf("the GGUF header claims %d tensors and %d keys, more than the file can hold", nTensors, nKV)
	}
	if nTensors > ggufMaxTensors || nKV > ggufMaxKV {
		return fmt.Sprintf("the GGUF header claims %d tensors and %d keys, more than any model carries (at most %d of each)", nTensors, nKV, ggufMaxTensors)
	}
	align := uint64(32) // GGUF_DEFAULT_ALIGNMENT
	keys := make(map[string]bool, min(nKV, 1<<10))
	var keyBytes uint64
	for i := uint64(0); i < nKV && g.failed == ""; i++ {
		key := g.str(1<<20, fmt.Sprintf("key %d is longer than 1 MiB", i))
		if keyBytes += uint64(len(key)); keyBytes > ggufMaxKeyBytes {
			g.fail("the GGUF keys take more than %d MiB", ggufMaxKeyBytes>>20)
			break
		}
		t := g.u32()
		if g.failed != "" {
			break
		}
		switch {
		case key == "":
			g.fail("key %d is empty", i)
		case strings.IndexByte(key, 0) >= 0:
			// llama.cpp compares keys as C strings: "general.alignment\x00x"
			// would mean general.alignment to it and another key to us
			g.fail("key %d contains a NUL byte", i)
		case keys[key]:
			g.fail("the key %s is given twice", quote(key))
		case key == "general.alignment":
			if t != 4 {
				g.fail("general.alignment is not a uint32")
				break
			}
			align = uint64(g.u32())
			if align == 0 || align&(align-1) != 0 {
				g.fail("the alignment %d is not a power of 2", align)
			}
		default:
			g.value(t, key)
		}
		keys[key] = true
	}
	type tensor struct {
		name    string
		off, nb uint64
		sized   bool
	}
	ts := make([]tensor, 0, min(nTensors, 1<<16))
	names := make(map[string]bool, min(nTensors, 1<<16))
	for i := uint64(0); i < nTensors && g.failed == ""; i++ {
		name := g.str(63, fmt.Sprintf("the name of tensor %d is 64 bytes or longer", i)) // GGML_MAX_NAME
		nd := g.u32()
		if g.failed != "" {
			break
		}
		if names[name] {
			g.fail("the tensor %s is given twice", quote(name))
			break
		}
		if strings.IndexByte(name, 0) >= 0 {
			g.fail("the name of tensor %d contains a NUL byte", i)
			break
		}
		names[name] = true
		if nd > 4 { // GGML_MAX_DIMS
			g.fail("tensor %s has %d dimensions, at most 4 are read", quote(name), nd)
			break
		}
		ne := [4]uint64{1, 1, 1, 1}
		for j := uint32(0); j < nd; j++ {
			ne[j] = g.u64()
			if ne[j] > math.MaxInt64 {
				g.fail("tensor %s has a negative dimension", quote(name))
			}
		}
		typ := g.u32()
		off := g.u64()
		if g.failed != "" {
			break
		}
		elems, over := uint64(1), false
		for _, d := range ne {
			hi, lo := bits.Mul64(elems, d)
			over = over || hi != 0 || lo > math.MaxInt64
			elems = lo
		}
		if over {
			g.fail("the shape of tensor %s overflows", quote(name))
			break
		}
		t := tensor{name: name, off: off}
		if bt, ok := ggmlTypes[typ]; ok {
			if ne[0]%bt[0] != 0 {
				g.fail("tensor %s has rows of %d elements, not whole blocks of %d", quote(name), ne[0], bt[0])
				break
			}
			hi, lo := bits.Mul64(elems/bt[0], bt[1])
			if hi != 0 {
				g.fail("the size of tensor %s overflows", quote(name))
				break
			}
			t.nb, t.sized = lo, true
		}
		ts = append(ts, t)
	}
	if g.failed != "" {
		return g.failed
	}
	if nTensors == 0 {
		return ""
	}
	data := (g.pos + align - 1) / align * align
	if data > g.size {
		return "the GGUF data section starts past the end of the file"
	}
	room := g.size - data
	var next uint64
	for _, t := range ts {
		if !t.sized { // a type this table cannot size: nothing after it can be placed
			break
		}
		if t.off != next {
			return fmt.Sprintf("tensor %s is at offset %d, llama.cpp expects %d", quote(t.name), t.off, next)
		}
		if t.nb > room || t.off > room-t.nb {
			return fmt.Sprintf("tensor %s runs past the end of the file", quote(t.name))
		}
		if t.nb > math.MaxUint64-align {
			return fmt.Sprintf("the size of tensor %s overflows", quote(t.name))
		}
		next = t.off + (t.nb+align-1)/align*align
	}
	return ""
}
