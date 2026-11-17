//go:build netbsd

package lmget

import "golang.org/x/sys/unix"

func diskFree(dir string) int64 {
	var s unix.Statvfs_t
	if err := unix.Statvfs1(dir, &s, unix.ST_WAIT); err != nil {
		return -1
	}
	return int64(s.Bavail) * int64(s.Frsize)
}
