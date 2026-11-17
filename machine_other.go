//go:build !darwin && !windows

package lmget

import "runtime"

func readMemory(m *Machine) {
	if runtime.GOOS == "linux" {
		readLinuxMemory(m)
	}
}
