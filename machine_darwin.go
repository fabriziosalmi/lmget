//go:build darwin

package lmget

import "golang.org/x/sys/unix"

func readMemory(m *Machine) {
	if n, err := unix.SysctlUint64("hw.memsize"); err == nil {
		m.TotalMem = int64(n)
	}
	// true on Apple Silicon even for an amd64 binary under Rosetta
	if n, err := unix.SysctlUint32("hw.optional.arm64"); err == nil && n == 1 {
		m.AppleSilicon, m.Arch = true, "arm64"
	}
	if b := runQuick("/usr/bin/vm_stat"); b != nil {
		m.AvailMem = parseVMStat(b)
	}
}
