//go:build windows

package lmget

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// The operating-system calls lmget needs, on Windows (see sys_unix.go).

// fileInfo opens path itself, not what a reparse point leads to, and reads
// its volume, file index and link count.
func fileInfo(path string) (*windows.ByHandleFileInformation, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func inodeOf(path string, _ os.FileInfo) (inode, bool) {
	info, err := fileInfo(path)
	if err != nil {
		return inode{}, false
	}
	return inode{uint64(info.VolumeSerialNumber), uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)}, true
}

func linkCount(path string, _ os.FileInfo) uint64 {
	info, err := fileInfo(path)
	if err != nil {
		return 1
	}
	return uint64(info.NumberOfLinks)
}

func allocated(fi os.FileInfo) int64 { return fi.Size() }

func sameDevice(a, b string) bool {
	ia, err1 := fileInfo(a)
	ib, err2 := fileInfo(b)
	return err1 == nil && err2 == nil && ia.VolumeSerialNumber == ib.VolumeSerialNumber
}

func lockFile(f *os.File, exclusive, wait bool) error {
	var flags uint32
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if !wait {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return errBusy
	}
	return err
}

func unlockFile(f *os.File) {
	windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}

// lockDir: Windows does not lock directories, so a lock file stands for the
// directory, kept in the temporary directory under a name derived from the
// directory's path: nothing is added to the user's project.
func lockDir(dir string) (func(), error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	locks := filepath.Join(os.TempDir(), "lmget-locks")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(locks, hex.EncodeToString(sum[:16])+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f, true, true); err != nil {
		f.Close()
		return nil, err
	}
	return func() { unlockFile(f); f.Close() }, nil
}

// openNoFollow refuses a symlink or other reparse point. Windows has no
// O_NOFOLLOW: the check is on the name just before the open.
func openNoFollow(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return nil, errIsSymlink
	}
	return os.Open(path)
}

func cannotHardlink(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_TOO_MANY_LINKS)
}

// linkFallback copies: a symlink needs Developer Mode or administrator
// rights on Windows. The copy is of a verified blob, checked by size.
func linkFallback(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// the copy lands on another drive than the store, whose space the pull
	// checked: check this one too
	if fi, err := in.Stat(); err == nil {
		if free := diskFree(filepath.Dir(dst)); free >= 0 && fi.Size()+512<<20 > free {
			return errors.New("not enough free space on the drive of " + filepath.Dir(dst) + " for a copy of " + fi.Name())
		}
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if fi, serr := in.Stat(); err == nil && (serr != nil || fi.Size() != n) {
		err = errors.New("the copy is shorter than the blob")
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

// reusePort lets lmget serve's mDNS socket share port 5353 with other
// mDNS responders.
func reusePort(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
	})
	if err != nil {
		return err
	}
	return serr
}
