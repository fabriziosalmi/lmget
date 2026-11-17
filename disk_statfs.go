//go:build linux || darwin || freebsd || dragonfly

package lmget

import "golang.org/x/sys/unix"

// diskFree is the space an unprivileged user can still write at dir, -1
// when unknown.
func diskFree(dir string) int64 {
	var s unix.Statfs_t
	if err := unix.Statfs(dir, &s); err != nil {
		return -1
	}
	return int64(s.Bavail) * int64(s.Bsize)
}
