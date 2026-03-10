// Package sysinfo provides platform-aware system resource detection.
//
// It detects available memory and CPU count using OS-specific methods.
// The detected values are used to derive block store sizing defaults.
package sysinfo

import "runtime"

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
		mem = defaultMemory
		source = "fallback (detection error: " + err.Error() + ")"
	}

	cpus := runtime.GOMAXPROCS(0)

	return &defaultDetector{
		memory: mem,
		cpus:   cpus,
		source: source,
	}
}

func (d *defaultDetector) AvailableMemory() uint64 { return d.memory }
func (d *defaultDetector) AvailableCPUs() int      { return d.cpus }
func (d *defaultDetector) MemorySource() string    { return d.source }
