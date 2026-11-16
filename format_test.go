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

func TestIgnorableCodePointsAndUnreadableZips(t *testing.T) {
	// two names that differ only by an ignorable code point collide on HFS+
	if isLocalSlash("model‌.safetensors") {
		t.Error("a file name with an ignorable code point was accepted")
	}
	// a user-chosen export directory is not a lock target: exportContained is the guard
	root := t.TempDir()
	if err := exportContained(root, filepath.Join(root, ".g‌it", "hooks")); err == nil {
		t.Error("an export directory aliasing .git was accepted")
	}
	if err := exportContained(root, filepath.Join(root, "models", "q")); err != nil {
		t.Errorf("a normal export directory was refused: %v", err)
	}
	// a zip Go cannot read may still be opened by torch's reader: fail closed
	p := filepath.Join(t.TempDir(), "model.onnx")
	os.WriteFile(p, []byte("PK\x03\x04 this is not a readable zip archive"), 0o644)
	if err := checkFormat(p, "model.onnx", false); err == nil {
		t.Error("an unreadable zip passed the format check")
	}
}

// A pickle named pytorch_model.bin next to a decoy pytorch_model.xml looks
// like OpenVINO by name; its content must still get it refused.
func TestPickleBehindDecoyXMLIsRefused(t *testing.T) {
	files := append(modelFiles(),
		fakeFile{path: "pytorch_model.bin", data: torchZip(t), lfs: true},
		fakeFile{path: "pytorch_model.xml", data: []byte("<net/>")})
	f := &fakeHF{files: files}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	dir := t.TempDir()
	c := newTestClient(t, srv, t.TempDir())
	_, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "dir", Path: dir}}, nil)
	if e := AsError(err); e == nil || e.Kind != "format" {
		t.Fatalf("got %v, want a format error", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pytorch_model.bin")); err == nil {
		t.Fatal("the pickle was exported")
	}
}

// LOW: protocol 0 and 1 pickles have no PROTO header.
func TestOldProtocolPicklesAreRecognised(t *testing.T) {
	p0 := []byte("cposix\nsystem\np0\n(Vecho pwned\np1\ntp2\nRp3\n.")
	p1 := []byte("cposix\nsystem\nq\x00(X\x0a\x00\x00\x00echo pwnedq\x01tq\x02Rq\x03.")
	cases := []struct {
		name string
		data []byte
		ok   bool
	}{
		{"model.onnx", p0, false},
		{"model.bin", p1, false},                                 // a .bin next to a decoy .xml is not declared pickle
		{"vocab.txt", []byte("cat\ndog\nend."), true},            // a word list is text
		{"weights.onnx", []byte("cabc\x00\x01\x02 data."), true}, // starts with c, ends with ., no GLOBAL
		{"model.pt", p0, true},                                   // declared pickle by name
	}
	for _, tc := range cases {
		dir := t.TempDir()
		path := filepath.Join(dir, tc.name)
		os.WriteFile(path, tc.data, 0o644)
		siblings := map[string]bool{"model.xml": true} // the decoy OpenVINO pair
		err := checkFormat(path, tc.name, isPickle(tc.name, siblings))
		if tc.ok && err != nil {
			t.Errorf("%s: refused: %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: an old-protocol pickle passed", tc.name)
		}
	}
}

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
