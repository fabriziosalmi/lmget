package lmget

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
)

// digest is the hash of a file computed while it downloads (issue #1): the
// final re-read of a large file costs seconds of CPU after the last byte, so
// the chunks are hashed in order as soon as all the ones before them are on
// disk, while later chunks still arrive. complete is set only when every
// byte of the file, in order, went through the hash.
type digest struct {
	sha256, git string
	complete    bool
}

// stream hashes chunk after chunk in file order: a chunk is read back from
// the partial file (just written, so from the page cache) once it and every
// chunk before it are done. already marks the chunks a resumed download had
// done before; done delivers the others as they complete, and is closed when
// the download ends. It reports whether the whole file was hashed.
func (d *digest) stream(out *os.File, f File, chunk int64, already []bool, done <-chan int) bool {
	n := len(already)
	have := append([]bool(nil), already...)
	s256 := sha256.New()
	var git hash.Hash
	w := io.Writer(s256)
	if f.GitSHA1 != "" {
		git = sha1.New()
		fmt.Fprintf(git, "blob %d\x00", f.Size)
		w = io.MultiWriter(s256, git)
	}
	next, failed := 0, false
	advance := func() {
		for !failed && next < n && have[next] {
			off := int64(next) * chunk
			size := min(chunk, f.Size-off)
			if m, err := io.Copy(w, io.NewSectionReader(out, off, size)); err != nil || m != size {
				failed = true // the caller re-reads the whole file instead
				return
			}
			next++
		}
	}
	advance()
	for i := range done {
		if i >= 0 && i < n {
			have[i] = true
		}
		advance()
	}
	if failed || next != n {
		return false
	}
	d.sha256 = hex.EncodeToString(s256.Sum(nil))
	if git != nil {
		d.git = hex.EncodeToString(git.Sum(nil))
	}
	d.complete = true
	return true
}

// check compares a complete digest with the hashes the source published, as
// verify does after reading the file again, and returns the sha256. The part
// file must hold exactly f.Size bytes.
func (d *digest) check(part string, f *File) (string, error) {
	fi, err := os.Stat(part)
	if err != nil {
		return "", diskErr(err)
	}
	bad := func(want, got, algo string) error {
		return newErr(ExitVerify, "verify", "verification failed for "+f.Path).
			why("%s published %s, received bytes hash to %s (%s)", "the source", short(want), short(got), algo).
			fix("", "the partial file was discarded: run the same command again").
			fix("", "if it fails again, the source is serving different bytes than it publishes")
	}
	switch {
	case fi.Size() != f.Size:
		return "", bad(fmt.Sprint(f.Size), fmt.Sprint(fi.Size()), "size")
	case f.SHA256 != "" && d.sha256 != f.SHA256:
		return "", bad(f.SHA256, d.sha256, "sha256")
	case f.GitSHA1 != "" && d.git != f.GitSHA1:
		return "", bad(f.GitSHA1, d.git, "git-sha1")
	}
	return d.sha256, nil
}
