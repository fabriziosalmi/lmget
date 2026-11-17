package lmget

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Machine is what lmget knows about this computer when it picks a variant.
// Zero means unknown: every field is read locally, without the network, and
// a reading that fails is left out rather than guessed.
type Machine struct {
	OS           string `json:"os"`
	Arch         string `json:"arch"`          // of the CPU, not of this binary (Rosetta)
	AppleSilicon bool   `json:"apple_silicon"` // unified memory: the GPU uses the RAM
	TotalMem     int64  `json:"total_mem"`     // RAM, or the container's limit when lower
	AvailMem     int64  `json:"avail_mem"`     // an estimate of what a model could use now
	GPU          string `json:"gpu,omitempty"` // NVIDIA only
	VRAMTotal    int64  `json:"vram_total,omitempty"`
	VRAMFree     int64  `json:"vram_free,omitempty"`
	ContainerCap bool   `json:"container_cap,omitempty"` // TotalMem is a cgroup limit
}

// DetectMachine reads the machine. It takes a few milliseconds, or up to two
// seconds when nvidia-smi is installed and slow.
func DetectMachine() Machine {
	m := Machine{OS: runtime.GOOS, Arch: runtime.GOARCH}
	readMemory(&m)
	readNVIDIA(&m)
	return m
}

// readNVIDIA asks nvidia-smi for the first GPU's memory, in MiB.
func readNVIDIA(m *Machine) {
	bin, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--query-gpu=name,memory.total,memory.free", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return
	}
	line, _, _ := strings.Cut(string(out), "\n")
	f := strings.Split(line, ",")
	if len(f) != 3 {
		return
	}
	total, err1 := strconv.ParseInt(strings.TrimSpace(f[1]), 10, 64)
	free, err2 := strconv.ParseInt(strings.TrimSpace(f[2]), 10, 64)
	if err1 != nil || err2 != nil || total <= 0 {
		return
	}
	m.GPU = printable(strings.TrimSpace(f[0]))
	m.VRAMTotal, m.VRAMFree = total<<20, free<<20
}

// parseMeminfo reads MemTotal and MemAvailable (kB) from /proc/meminfo.
func parseMeminfo(b []byte) (total, avail int64) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "MemTotal":
			total = n << 10
		case "MemAvailable":
			avail = n << 10
		}
	}
	return
}

// parseCgroupMax reads a cgroup v2 memory.max: a byte count, or "max".
func parseCgroupMax(b []byte) int64 {
	s := strings.TrimSpace(string(b))
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// parseVMStat estimates the memory available on macOS from vm_stat: free,
// speculative and purgeable pages, and half the inactive ones. Inactive
// pages are reclaimable but not free, and on a machine that is already
// compressing and swapping they are the first to be fought over, so only
// half count (measured 2026-11-17: with 2.3 GB inactive and 7.5 GB
// compressed, oMLX judged 0.37 GB usable; free+inactive would say 2.4 GB).
func parseVMStat(b []byte) int64 {
	text := string(b)
	page := int64(16384)
	if i := strings.Index(text, "page size of "); i >= 0 {
		f := strings.Fields(text[i+len("page size of "):])
		if len(f) > 0 {
			if n, err := strconv.ParseInt(f[0], 10, 64); err == nil && n > 0 {
				page = n
			}
		}
	}
	pages := map[string]int64{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), "."), 10, 64)
		if err == nil {
			pages[strings.TrimSpace(k)] = n
		}
	}
	if _, ok := pages["Pages free"]; !ok {
		return 0
	}
	return (pages["Pages free"] + pages["Pages speculative"] + pages["Pages purgeable"] + pages["Pages inactive"]/2) * page
}

func runQuick(name string, args ...string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return nil
	}
	return out
}

func readLinuxMemory(m *Machine) {
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		m.TotalMem, m.AvailMem = parseMeminfo(b)
	}
	// in a container the cgroup limit is the memory there is, whatever the host has
	if b, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		if limit := parseCgroupMax(b); limit > 0 && (m.TotalMem == 0 || limit < m.TotalMem) {
			m.TotalMem, m.ContainerCap = limit, true
			if cur, err := os.ReadFile("/sys/fs/cgroup/memory.current"); err == nil {
				if used := parseCgroupMax(cur); used > 0 && limit-used < m.AvailMem {
					m.AvailMem = max(limit-used, 0)
				}
			}
		}
	}
}
