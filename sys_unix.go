//go:build !windows

package lmget

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// The operating-system calls lmget needs, on Unix: file locks, file
// identity for hard links, an open that refuses a symlink. sys_windows.go
// has the same functions for Windows.

func inodeOf(_ string, fi os.FileInfo) (inode, bool) {
	s, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return inode{}, false
	}
	return inode{uint64(s.Dev), uint64(s.Ino)}, true
}

// linkCount is how many names a file has.
func linkCount(_ string, fi os.FileInfo) uint64 {
	if s, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(s.Nlink)
	}
	return 1
}

// allocated is the space a file really uses: partial downloads are sparse.
func allocated(fi os.FileInfo) int64 {
	if s, ok := fi.Sys().(*syscall.Stat_t); ok {
		return s.Blocks * 512
	}
	return fi.Size()
}

func sameDevice(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	if err1 != nil || err2 != nil {
		return false
	}
	sa, ok1 := fa.Sys().(*syscall.Stat_t)
	sb, ok2 := fb.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && sa.Dev == sb.Dev
}

// lockFile takes an advisory lock on f; with wait=false a busy lock is errBusy.
func lockFile(f *os.File, exclusive, wait bool) error {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errBusy
		}
		return err
	}
	return nil
}

func unlockFile(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// lockDir locks a directory exclusively, waiting for it.
func lockDir(dir string) (func(), error) {
	d, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := lockFile(d, true, true); err != nil {
		d.Close()
		return nil, err
	}
	return func() { unlockFile(d); d.Close() }, nil
}

// openNoFollow opens path for reading, refusing a symlink (errIsSymlink) in
// the same call, so nothing can be swapped between a check and the open.
func openNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, errIsSymlink
	}
	return f, err
}

// cannotHardlink reports a link refused because hard links are impossible
// here (another filesystem, too many links, not supported): the caller then
// makes a symlink into the store.
func cannotHardlink(err error) bool {
	return errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EMLINK) ||
		errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}

// linkFallback makes dst point at src where a hard link is impossible.
func linkFallback(src, dst string) error { return os.Symlink(src, dst) }

// reusePort lets lmget serve's mDNS socket share port 5353 with the
// system's own responder (mDNSResponder, avahi).
func reusePort(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		if serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); serr == nil {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}
	})
	if err != nil {
		return err
	}
	return serr
}
