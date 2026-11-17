//go:build !(linux || darwin || freebsd || dragonfly || openbsd || netbsd || windows)

package lmget

func diskFree(string) int64 { return -1 } // unknown: the free-space check is skipped
