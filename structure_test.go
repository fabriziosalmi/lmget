package lmget

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

// gguf builds GGUF headers for tests, field by field.
type gguf struct{ bytes.Buffer }

func (g *gguf) u32(v uint32) *gguf { binary.Write(g, binary.LittleEndian, v); return g }
func (g *gguf) u64(v uint64) *gguf { binary.Write(g, binary.LittleEndian, v); return g }
func (g *gguf) str(s string) *gguf { g.u64(uint64(len(s))); g.WriteString(s); return g }

// pad aligns the buffer to a, as the data section must start.
func (g *gguf) pad(a int) *gguf {
	for g.Len()%a != 0 {
		g.WriteByte(0)
	}
	return g
}

// ggufTensor writes one tensor info.
func (g *gguf) tensor(name string, typ uint32, off uint64, dims ...uint64) *gguf {
	g.str(name).u32(uint32(len(dims)))
	for _, d := range dims {
		g.u64(d)
	}
	return g.u32(typ).u64(off)
}

// fakeGGUF is a well-formed GGUF v3 file: one string key and one I8 tensor
// of n random bytes.
func fakeGGUF(n int) []byte { return ggufAround(randBytes(n)) }

// ggufAround is a well-formed GGUF v3 file whose one I8 tensor is data.
func ggufAround(data []byte) []byte {
	g := &gguf{}
	g.WriteString("GGUF")
	g.u32(3).u64(1).u64(1)
	g.str("general.architecture").u32(ggufString).str("llama")
	g.tensor("w", 24, 0, uint64(len(data)))
	g.pad(32)
	g.Write(data)
	return g.Bytes()
}

// stAround is a well-formed safetensors file whose one U8 tensor is data.
func stAround(data []byte) []byte {
	b := stFile(fmt.Sprintf(`{"w":{"dtype":"U8","shape":[%d],"data_offsets":[0,%d]}}`, len(data), len(data)), 0)
	return append(b, data...)
}

// stFile is a safetensors file with this header and n data bytes.
func stFile(header string, n int) []byte {
	b := binary.LittleEndian.AppendUint64(nil, uint64(len(header)))
	return append(append(b, header...), make([]byte, n)...)
}

func TestSafetensorsStructure(t *testing.T) {
	cases := []struct {
		name, header string
		n            int
		why          string // "" when the file is well formed
	}{
		{"one tensor", `{"w":{"dtype":"F32","shape":[2,3],"data_offsets":[0,24]}}`, 24, ""},
		{"any order, metadata, padding", `{"__metadata__":{"format":"pt"},"b":{"dtype":"BF16","shape":[4],"data_offsets":[8,16]},"a":{"dtype":"U8","shape":[8],"data_offsets":[0,8]}}   `, 16, ""},
		{"null metadata", `{"__metadata__":null,"w":{"dtype":"I8","shape":[1],"data_offsets":[0,1]}}`, 1, ""},
		{"empty tensor and scalar", `{"e":{"dtype":"F32","shape":[0,7],"data_offsets":[0,0]},"s":{"dtype":"F64","shape":[],"data_offsets":[0,8]}}`, 8, ""},
		{"sub-byte dtype", `{"w":{"dtype":"F4","shape":[4],"data_offsets":[0,2]}}`, 2, ""},
		{"a dtype newer than the table", `{"w":{"dtype":"F2_NEW","shape":[3],"data_offsets":[0,5]}}`, 5, ""},
		{"no tensors", `{}`, 0, ""},
		{"a gap between tensors", `{"a":{"dtype":"U8","shape":[4],"data_offsets":[0,4]},"b":{"dtype":"U8","shape":[4],"data_offsets":[8,12]}}`, 12, "does not start where"},
		{"overlapping tensors", `{"a":{"dtype":"U8","shape":[8],"data_offsets":[0,8]},"b":{"dtype":"U8","shape":[4],"data_offsets":[4,8]}}`, 8, "does not start where"},
		{"bytes after the last tensor", `{"w":{"dtype":"U8","shape":[4],"data_offsets":[0,4]}}`, 9, "cover 4 bytes"},
		{"a tensor past the end", `{"w":{"dtype":"U8","shape":[64],"data_offsets":[0,64]}}`, 4, "cover 64 bytes"},
		{"shape and size disagree", `{"w":{"dtype":"F32","shape":[2],"data_offsets":[0,4]}}`, 4, "need 8"},
		{"shape overflows", `{"w":{"dtype":"F32","shape":[4294967296,4294967296],"data_offsets":[0,0]}}`, 0, "overflows"},
		{"not a whole byte", `{"w":{"dtype":"F4","shape":[3],"data_offsets":[0,1]}}`, 1, "byte"},
		{"ends before it starts", `{"w":{"dtype":"U8","shape":[0],"data_offsets":[4,0]}}`, 4, "ends before"},
		{"a key given twice", `{"w":{"dtype":"U8","shape":[1],"data_offsets":[0,1]},"w":{"dtype":"U8","shape":[2],"data_offsets":[0,2]}}`, 2, "twice"},
		{"a field given twice", `{"w":{"dtype":"U8","dtype":"F32","shape":[1],"data_offsets":[0,1]}}`, 1, "dtype"},
		{"a field in other case", `{"w":{"DTYPE":"U8","shape":[1],"data_offsets":[0,1]}}`, 1, "no valid dtype"},
		{"missing offsets", `{"w":{"dtype":"U8","shape":[1]}}`, 1, "no valid dtype"},
		{"negative shape", `{"w":{"dtype":"U8","shape":[-1],"data_offsets":[0,1]}}`, 1, "no valid dtype"},
		{"metadata not strings", `{"__metadata__":{"a":1}}`, 0, "map of strings"},
		{"not an object", `[1,2]`, 0, "not a JSON object"},
		{"trailing data", `{} {}`, 0, "after its JSON object"},
		{"not UTF-8", "{\"\xff\":1}", 0, "UTF-8"},
	}
	for _, tc := range cases {
		b := stFile(tc.header, tc.n)
		why := checkSafetensors(bytes.NewReader(b), int64(len(b)))
		if tc.why == "" && why != "" || tc.why != "" && !strings.Contains(why, tc.why) {
			t.Errorf("%s: got %q, want %q", tc.name, why, tc.why)
		}
	}
	for _, b := range [][]byte{nil, {1, 2, 3}, binary.LittleEndian.AppendUint64(nil, 1<<40), binary.LittleEndian.AppendUint64(nil, 200_000_000)} {
		if why := checkSafetensors(bytes.NewReader(b), int64(len(b))); why == "" {
			t.Errorf("% x was accepted", b)
		}
	}
	if b := fakeSafetensors(1000); checkSafetensors(bytes.NewReader(b), int64(len(b))) != "" {
		t.Error("the test fixture is refused")
	}
}

func TestGGUFStructure(t *testing.T) {
	head := func(version uint32, nT, nKV uint64) *gguf {
		g := &gguf{}
		g.WriteString("GGUF")
		return g.u32(version).u64(nT).u64(nKV)
	}
	// two Q4_0 tensors (32 elements = 18 bytes each, padded to 32), data
	// of the given length; off2 is the second tensor's offset
	twoQ4 := func(off2 uint64, data int, align uint32) []byte {
		g := head(3, 2, 2)
		g.str("general.architecture").u32(ggufString).str("llama")
		g.str("general.alignment").u32(4).u32(align)
		g.tensor("a", 2, 0, 32)
		g.tensor("b", 2, off2, 32, 1)
		g.pad(int(align))
		g.Write(make([]byte, data))
		return g.Bytes()
	}
	cases := []struct {
		name string
		b    []byte
		why  string
	}{
		{"fixture", fakeGGUF(100), ""},
		{"two tensors, padded", twoQ4(32, 50, 32), ""},
		{"alignment 64", twoQ4(64, 82, 64), ""},
		{"no tensors", head(3, 0, 0).Bytes(), ""},
		{"version 1 is not walked", append(head(1, 1<<40, 1<<40).Bytes(), 0), ""},
		{"a newer version is not walked", head(4, 1<<40, 1<<40).Bytes(), ""},
		{"a newer tensor type is not sized", func() []byte {
			g := head(3, 1, 0)
			g.tensor("w", 99, 0, 5)
			return g.pad(32).Bytes()
		}(), ""},
		{"arrays of numbers and strings", func() []byte {
			g := head(3, 0, 2)
			g.str("a").u32(ggufArray).u32(5).u64(3).u32(1).u32(2).u32(3)
			g.str("tokens").u32(ggufArray).u32(ggufString).u64(2).str("x").str("yz")
			return g.Bytes()
		}(), ""},
		{"wrong magic", []byte("GGUX\x03\x00\x00\x00" + strings.Repeat("\x00", 16)), "magic"},
		{"too short", []byte("GGUF"), "too short"},
		{"second tensor not where expected", twoQ4(18, 50, 32), "expects 32"},
		{"second tensor past the end", twoQ4(32, 40, 32), "past the end"},
		{"alignment not a power of 2", twoQ4(48, 80, 48), "power of 2"},
		{"counts beyond the file", head(3, 1<<40, 0).Bytes(), "more than the file"},
		{"string past the end", func() []byte {
			g := head(3, 0, 1)
			g.u64(1 << 20).WriteString("abc")
			return append(g.Bytes(), make([]byte, 16)...)
		}(), "past the end"},
		{"string over 1 GiB", func() []byte {
			g := head(3, 0, 1)
			g.u64(1<<30 + 1)
			return append(g.Bytes(), make([]byte, 32)...)
		}(), "claims"},
		{"empty key", func() []byte {
			g := head(3, 0, 1)
			g.str("").u32(4).u32(1)
			return g.Bytes()
		}(), "empty"},
		{"repeated key", func() []byte {
			g := head(3, 0, 2)
			g.str("k").u32(4).u32(1).str("k").u32(4).u32(2)
			return g.Bytes()
		}(), "twice"},
		{"nested array", func() []byte {
			g := head(3, 0, 1)
			g.str("n").u32(ggufArray).u32(ggufArray).u64(1).u32(4).u64(0)
			return g.Bytes()
		}(), "nested"},
		{"array past the end", func() []byte {
			g := head(3, 0, 1)
			g.str("n").u32(ggufArray).u32(12).u64(1 << 29)
			return append(g.Bytes(), make([]byte, 64)...)
		}(), "past the end"},
		{"unknown value type", func() []byte {
			g := head(3, 0, 1)
			g.str("k").u32(13).u32(0)
			return g.Bytes()
		}(), "unknown GGUF type"},
		{"alignment of the wrong type", func() []byte {
			g := head(3, 0, 1)
			g.str("general.alignment").u32(10).u64(32)
			return g.Bytes()
		}(), "not a uint32"},
		{"long tensor name", func() []byte {
			g := head(3, 1, 0)
			g.tensor(strings.Repeat("n", 64), 0, 0, 1)
			return g.pad(32).u32(0).Bytes()
		}(), "64 bytes"},
		{"repeated tensor", func() []byte {
			g := head(3, 2, 0)
			g.tensor("w", 0, 0, 1).tensor("w", 0, 32, 1)
			return g.pad(32).Write64(64).Bytes()
		}(), "twice"},
		{"five dimensions", func() []byte {
			g := head(3, 1, 0)
			g.tensor("w", 0, 0, 1, 1, 1, 1, 1)
			return g.pad(32).Write64(4).Bytes()
		}(), "dimensions"},
		{"negative dimension", func() []byte {
			g := head(3, 1, 0)
			g.tensor("w", 0, 0, 1<<63)
			return g.pad(32).Bytes()
		}(), "negative"},
		{"elements overflow", func() []byte {
			g := head(3, 1, 0)
			g.tensor("w", 0, 0, 1<<32, 1<<32)
			return g.pad(32).Bytes()
		}(), "overflows"},
		{"bytes overflow", func() []byte {
			g := head(3, 1, 0)
			g.tensor("w", 27, 0, 1<<62) // I64: 2^62 elements of 8 bytes
			return g.pad(32).Bytes()
		}(), "overflows"},
		{"row not whole blocks", func() []byte {
			g := head(3, 1, 0)
			g.tensor("w", 2, 0, 31)
			return g.pad(32).Write64(32).Bytes()
		}(), "whole blocks"},
		{"a removed type is not sized", func() []byte {
			g := head(3, 1, 0)
			g.tensor("w", 4, 7, 32)
			return g.pad(32).Bytes()
		}(), ""},
	}
	for _, tc := range cases {
		why := checkGGUF(bytes.NewReader(tc.b), int64(len(tc.b)))
		if tc.why == "" && why != "" || tc.why != "" && !strings.Contains(why, tc.why) {
			t.Errorf("%s: got %q, want %q", tc.name, why, tc.why)
		}
	}
}

// Write64 appends n zero bytes of tensor data.
func (g *gguf) Write64(n int) *gguf { g.Write(make([]byte, n)); return g }

// the sizes in ggmlTypes match the block arithmetic llama.cpp uses for the
// K-quants (QK_K = 256), so a typo in the table shows here
func TestGGMLTypeSizes(t *testing.T) {
	const k = 256
	want := map[uint32]uint64{10: 2 + 2 + k/16 + k/4, 11: 2 + k/4 + k/8 + 12, 12: 2 + 2 + k/2 + 12, 13: 2 + 2 + k/2 + k/8 + 12,
		14: 2 + k/2 + k/4 + k/16, 15: 4 + k + k/8, 16: 2 + k/4, 17: 2 + k/4 + k/32, 18: 2 + k/4 + k/8, 19: 2 + k/8 + k/16,
		21: 2 + k/4 + k/8 + k/32 + 4, 22: 2 + k/4 + k/16, 23: 2 + 2 + k/2 + k/64, 29: k/8 + k/16 + k/32, 34: 2 + 4*13, 35: 2 + 64}
	for id, sz := range want {
		if ggmlTypes[id] != [2]uint64{k, sz} {
			t.Errorf("type %d: %v, want {%d %d}", id, ggmlTypes[id], k, sz)
		}
	}
	if len(ggmlTypes) != 36+3 {
		t.Errorf("%d types, gguf-py lists 36, plus the three Q4_0 repacks", len(ggmlTypes))
	}
}
