package lmget

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// zipWithPickle is a zip as torch.save writes it: an archive/data.pkl entry.
func zipWithPickle(t *testing.T) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, err := zw.Create("archive/data.pkl")
	if err != nil {
		t.Fatal(err)
	}
	w.Write(realPickles["proto2"])
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A zip with a pickle appended after a GGUF or safetensors header is a
// pickle: zip readers find the archive from the end of the file.
func TestAZipAppendedToAnotherFormatIsAPickle(t *testing.T) {
	st := make([]byte, 8)
	binary.LittleEndian.PutUint64(st, 2)
	st = append(st, "{}"...)
	for name, prefix := range map[string][]byte{
		"model.gguf":        append([]byte("GGUF\x03\x00\x00\x00"), make([]byte, 4096)...),
		"model.safetensors": st,
	} {
		p := filepath.Join(t.TempDir(), name)
		os.WriteFile(p, append(append([]byte(nil), prefix...), zipWithPickle(t)...), 0o644)
		if err := checkFormat(p, name, false); err == nil {
			t.Errorf("%s with a zip holding data.pkl appended: passed the format check", name)
		}
	}
}

// The end-of-archive signature in ordinary weights, with nothing a zip
// reader can open behind it, is not a reason to refuse a file.
func TestAnEndOfZipSignatureInWeightsIsNotAPickle(t *testing.T) {
	st := make([]byte, 8)
	binary.LittleEndian.PutUint64(st, 2)
	st = append(st, "{}"...)
	random := make([]byte, 1<<20)
	rand.Read(random)
	copy(random[len(random)-5000:], "PK\x05\x06")
	p := filepath.Join(t.TempDir(), "model.safetensors")
	os.WriteFile(p, append(st, random...), 0o644)
	if err := checkFormat(p, "model.safetensors", false); err != nil {
		t.Fatalf("refused: %v", err)
	}
}

// sentence-transformers/all-MiniLM-L6-v2 ships rust_model.ot, a torch zip
// with data.pkl, next to model.safetensors. lmget treats .ot as a pickle
// format: skipped by default, downloaded with AllowPickle, never refused as
// "not what its name says" (which failed the whole pull).
func TestATorchArchiveNamedOtIsADeclaredPickle(t *testing.T) {
	files := append(modelFiles(), fakeFile{path: "rust_model.ot", data: zipWithPickle(t), lfs: true})
	srv := httptest.NewServer((&fakeHF{files: files}).handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	out, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}}, nil)
	if err != nil {
		t.Fatalf("default pull: %v", err)
	}
	for _, f := range out.Plan.Files {
		if f.Path == "rust_model.ot" && f.Selected() {
			t.Fatal("rust_model.ot selected by default although model.safetensors exists")
		}
	}
	out, err = c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Selection: Selection{AllowPickle: true}, Target: Target{Kind: "none"}}, nil)
	if err != nil {
		t.Fatalf("pull with AllowPickle: %v", err)
	}
	if len(out.Plan.Pickle) != 1 || out.Plan.Pickle[0] != "rust_model.ot" {
		t.Fatalf("pickle files = %v; want [rust_model.ot]", out.Plan.Pickle)
	}
}
