//go:build linux

package sysinfo

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// availableMemory detects available memory on Linux with two-stage detection:
// 1. Try cgroup v2 memory.max (container-aware)
// 2. Fallback to /proc/meminfo (physical memory)
func availableMemory() (uint64, string, error) {
	// Stage 1: cgroup v2 memory limit
	if mem, err := readCgroupMemory(); err == nil && mem > 0 {
		return mem, "cgroup v2 memory.max", nil
	}

	// Stage 2: /proc/meminfo
	mem, err := readProcMeminfo()
	if err != nil {
		return 0, "/proc/meminfo", err
	}
	return mem, "/proc/meminfo", nil
}

// readCgroupMemory reads the cgroup v2 memory limit.
// Returns 0 if the file does not exist or the value is "max" (unlimited).
func readCgroupMemory() (uint64, error) {
	data, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	if s == "max" {
		// "max" means unlimited -- fall through to /proc/meminfo
		return 0, nil
	}
	return strconv.ParseUint(s, 10, 64)
}

// readProcMeminfo reads MemTotal from /proc/meminfo.
func readProcMeminfo() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			// Format: "MemTotal:       16384000 kB"
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb * 1024, nil // kB -> bytes
		}
	}
	return 0, scanner.Err()
}
