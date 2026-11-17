//go:build openbsd

package lmget

import "golang.org/x/sys/unix"

func diskFree(dir string) int64 {
	var s unix.Statfs_t
	if err := unix.Statfs(dir, &s); err != nil {
		return -1
	}
	return int64(s.F_bavail) * int64(s.F_bsize)
}
