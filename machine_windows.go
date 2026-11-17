//go:build windows

package lmget

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// memoryStatusEx is MEMORYSTATUSEX; x/sys/windows has no wrapper for
// GlobalMemoryStatusEx.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

var globalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

func readMemory(m *Machine) {
	var s memoryStatusEx
	s.length = uint32(unsafe.Sizeof(s))
	if r, _, _ := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&s))); r == 0 {
		return
	}
	m.TotalMem, m.AvailMem = int64(s.totalPhys), int64(s.availPhys)
}
