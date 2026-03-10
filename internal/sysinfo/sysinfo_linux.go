//go:build linux

package sysinfo

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
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

// readCgroupMemory reads the cgroup v2 memory limit for this process.
// It resolves the process's actual cgroup path via /proc/self/cgroup
// to correctly detect limits inside containers and systemd slices.
// Returns 0 if the file does not exist or the value is "max" (unlimited).
func readCgroupMemory() (uint64, error) {
	cgroupPath, err := resolveProcessCgroup()
	if err != nil {
		return 0, err
	}
	memoryMaxPath := filepath.Join(cgroupPath, "memory.max")
	data, err := os.ReadFile(memoryMaxPath)
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

// resolveProcessCgroup returns the cgroup v2 directory for this process.
// It parses /proc/self/cgroup to find the unified (v2) hierarchy entry.
func resolveProcessCgroup() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		// cgroup v2 unified hierarchy: "0::/path"
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && parts[0] == "0" && parts[1] == "" {
			// parts[2] is typically an absolute path like "/kubepods/...".
			// Trim the leading "/" so filepath.Join doesn't discard the prefix.
			cgroupRelPath := strings.TrimPrefix(filepath.Clean(parts[2]), "/")
			return filepath.Join("/sys/fs/cgroup", cgroupRelPath), nil
		}
	}
	return "", errors.New("cgroup v2 unified hierarchy not found in /proc/self/cgroup")
}

// readProcMeminfo reads MemTotal from /proc/meminfo.
func readProcMeminfo() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

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
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("MemTotal not found in /proc/meminfo")
}
