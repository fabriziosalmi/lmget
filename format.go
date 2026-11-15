package lmget

import (
	"archive/zip"
	"encoding/binary"
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
	if looksPickle(f, head, size) && !declaredPickle {
		return bad("its content is a Python pickle, which runs code when loaded")
	}
	switch ext {
	case ".safetensors":
		if len(head) < 9 {
			return bad("too short for a safetensors header")
		}
		hlen := binary.LittleEndian.Uint64(head[:8])
		if hlen == 0 || hlen > 100<<20 || int64(hlen) > size-8 || head[8] != '{' {
			return bad("the safetensors header is missing or malformed")
		}
	case ".gguf":
		if len(head) < 4 || string(head[:4]) != "GGUF" {
			return bad("it does not start with the GGUF magic")
		}
	}
	return nil
}

// looksPickle recognises a raw pickle (protocol 2 to 5 starts with the PROTO
// opcode 0x80) and the zip archives torch.save writes, which hold data.pkl.
func looksPickle(f *os.File, head []byte, size int64) bool {
	if len(head) >= 2 && head[0] == 0x80 && head[1] >= 2 && head[1] <= 5 {
		return true
	}
	if len(head) >= 4 && string(head[:4]) == "PK\x03\x04" {
		zr, err := zip.NewReader(f, size)
		if err != nil {
			return false
		}
		for _, zf := range zr.File {
			if strings.HasSuffix(strings.ToLower(zf.Name), ".pkl") {
				return true
			}
		}
	}
	return false
}
