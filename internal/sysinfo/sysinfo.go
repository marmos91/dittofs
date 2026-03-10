// Package sysinfo provides platform-aware system resource detection.
//
// It detects available memory and CPU count using OS-specific methods.
// The detected values are used to derive block store sizing defaults.
package sysinfo

import (
	"fmt"
	"runtime"

	"github.com/marmos91/dittofs/internal/logger"
)

// Detector provides system resource information.
type Detector interface {
	// AvailableMemory returns the total physical memory in bytes.
	AvailableMemory() uint64
	// AvailableCPUs returns the number of CPUs available to the process.
	AvailableCPUs() int
	// MemorySource returns a human-readable description of how memory was detected.
	MemorySource() string
}

const defaultMemory = 4 * 1024 * 1024 * 1024 // 4 GiB fallback

type defaultDetector struct {
	memory uint64
	cpus   int
	source string
}

// NewDetector creates a Detector that probes the current system.
// Memory detection is platform-specific (darwin: sysctl, linux: cgroup/meminfo,
// windows: GlobalMemoryStatusEx). CPU count uses runtime.GOMAXPROCS(0).
func NewDetector() Detector {
	mem, source, err := availableMemory()
	if err != nil {
		logger.Warn("Failed to detect system memory, using 4 GiB fallback",
			"error", err,
			"source", source,
		)
		mem = defaultMemory
		source = "fallback (detection error)"
	}

	cpus := runtime.GOMAXPROCS(0)

	logger.Info("System detected",
		"memory_bytes", mem,
		"memory_human", formatBytes(mem),
		"source", source,
		"cpus", cpus,
	)

	return &defaultDetector{
		memory: mem,
		cpus:   cpus,
		source: source,
	}
}

func (d *defaultDetector) AvailableMemory() uint64 { return d.memory }
func (d *defaultDetector) AvailableCPUs() int      { return d.cpus }
func (d *defaultDetector) MemorySource() string    { return d.source }

func formatBytes(b uint64) string {
	const gib = 1024 * 1024 * 1024
	const mib = 1024 * 1024
	if b >= gib {
		v := float64(b) / float64(gib)
		if v == float64(uint64(v)) {
			return fmt.Sprintf("%d GiB", uint64(v))
		}
		return fmt.Sprintf("%.1f GiB", v)
	}
	v := float64(b) / float64(mib)
	if v == float64(uint64(v)) {
		return fmt.Sprintf("%d MiB", uint64(v))
	}
	return fmt.Sprintf("%.1f MiB", v)
}
