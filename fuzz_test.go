package lmget

// Native fuzz targets for the parsers that read untrusted input. Their seeds
// run as ordinary tests; to search, for example:
//   go test -run '^$' -fuzz '^FuzzWalkPickle$' -fuzztime 60s .

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pickle opcode walk reads any bytes without panicking and terminates.
func FuzzWalkPickle(f *testing.F) {
	for _, p := range realPickles {
		f.Add(p)
	}
	f.Add([]byte("cat\ndog\n(\n."))
	f.Add([]byte{0x80, 0x05, 0x95})
	f.Fuzz(func(t *testing.T, data []byte) {
		walkPickle(io.NewSectionReader(bytes.NewReader(data), 0, int64(len(data))), int64(len(data)))
	})
}

// The format check of a downloaded file never panics, whatever its content
// and name.
func FuzzCheckFormat(f *testing.F) {
	f.Add([]byte("GGUF\x03\x00\x00\x00"), ".gguf")
	f.Add(fakeGGUF(64), ".gguf")
	f.Add(fakeSafetensors(16), ".safetensors")
	f.Add([]byte("\x02\x00\x00\x00\x00\x00\x00\x00{}"), ".safetensors")
	f.Add(realPickles["proto4"], ".bin")
	f.Add([]byte("PK\x03\x04"), ".pt")
	f.Fuzz(func(t *testing.T, data []byte, ext string) {
		if strings.ContainsAny(ext, "/\\\x00") || len(ext) > 16 {
			return
		}
		name := "f" + ext
		p := filepath.Join(t.TempDir(), "x")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Skip(err)
		}
		checkFormat(p, name, false)
	})
}

// A reference that parses prints a canonical form (source, repo, revision:
// the key of the lockfile and the index; a file path and a #sha256 pin are
// not part of it) that parses back to the same key.
func FuzzParseRef(f *testing.F) {
	for _, s := range []string{"Qwen/Qwen3-0.6B", "hf:org/m@v1", "https://huggingface.co/a/b/resolve/main/sub/m.gguf",
		"https://example.com/m.gguf#sha256=" + strings.Repeat("a", 64), "ollama:smollm2:135m", "oci://docker.io/ai/smollm2:latest",
		"oci://ghcr.io/o/m@sha256:" + strings.Repeat("b", 64)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, err := ParseRef(s)
		if err != nil {
			return
		}
		back, err := ParseRef(r.String())
		key := Ref{Source: r.Source, Repo: r.Repo, Revision: r.Revision}
		if err != nil || back != key {
			t.Fatalf("ParseRef(%q) = %+v, String %q parses to %+v (%v)", s, r, r.String(), back, err)
		}
	})
}

// A lockfile, as a cloned repository may carry it, never crashes the reader.
func FuzzReadLock(f *testing.F) {
	f.Add([]byte("version = 1\n[[model]]\nref = \"hf:org/m\"\ncommit = \"" + strings.Repeat("a", 40) + "\"\nto = \"none\"\n" +
		"[[model.file]]\npath = \"config.json\"\nsize = 3\nsha256 = \"" + strings.Repeat("b", 64) + "\"\n"))
	f.Add([]byte("version = 1\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		p := filepath.Join(t.TempDir(), DefaultLockName)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Skip(err)
		}
		ReadLock(p)
	})
}

// An index body (the signature aside) never crashes decoding and validation.
func FuzzIndexValidate(f *testing.F) {
	b, _ := json.Marshal(sampleIndex(1))
	f.Add(b)
	f.Add([]byte(`{"version":1,"sequence":1,"models":[{"ref":"hf:a/b","files":[{"path":"../x"}]}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var ix Index
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if dec.Decode(&ix) == nil {
			if ix.validate() == nil && !ix.Expires.After(ix.Issued) {
				t.Fatal("validate accepted an index that expires before it is issued")
			}
		}
	})
}

// The minisign signature parser never panics on what a server sends.
func FuzzVerifyMinisign(f *testing.F) {
	s := testSigner(f)
	data := []byte("index")
	f.Add(data, s.sign(data))
	f.Add(data, "untrusted comment: x\nRUQ=\ntrusted comment: t\nAAAA\n")
	f.Fuzz(func(t *testing.T, data []byte, sig string) {
		verifyMinisign(s.key, data, sig)
	})
}

// A path accepted as local stays inside any root it is joined to.
func FuzzIsLocalSlash(f *testing.F) {
	for _, p := range []string{"config.json", "sub/model.gguf", "../x", "a/../../b", ".git/config", "a/./b", "/abs"} {
		f.Add(p)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if !isLocalSlash(p) {
			return
		}
		root := filepath.Join("/", "store", "root")
		j := filepath.Join(root, filepath.FromSlash(p))
		if j != root && !strings.HasPrefix(j, root+string(filepath.Separator)) {
			t.Fatalf("isLocalSlash(%q) but it resolves to %q outside %q", p, j, root)
		}
	})
}
