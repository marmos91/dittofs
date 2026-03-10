package blockstore

import "fmt"

// SystemDetector provides system resource information for deduction.
// This mirrors sysinfo.Detector but lives in pkg/blockstore to avoid
// importing internal/ from pkg/. The sysinfo.Detector satisfies this
// interface structurally (duck typing).
type SystemDetector interface {
	AvailableMemory() uint64
	AvailableCPUs() int
}

// Minimum floor values for deduced defaults.
const (
	MinLocalStoreSize      uint64 = 256 << 20 // 256 MiB
	MinL1CacheSize         int64  = 64 << 20  // 64 MiB
	MinParallelSyncs              = 4
	MinParallelFetches            = 8
	DefaultPrefetchWorkers        = 4
)

// DeducedDefaults holds block store sizing values derived from system resources.
type DeducedDefaults struct {
	LocalStoreSize  uint64 // 25% of memory, floor 256 MiB
	L1CacheSize     int64  // 12.5% of memory, floor 64 MiB
	MaxPendingSize  uint64 // 50% of LocalStoreSize
	ParallelSyncs   int    // max(4, cpus)
	ParallelFetches int    // max(8, cpus*2)
	PrefetchWorkers int    // fixed at DefaultPrefetchWorkers
}

// DeduceDefaults derives block store sizing from detected system resources.
func DeduceDefaults(d SystemDetector) *DeducedDefaults {
	mem := d.AvailableMemory()
	cpus := d.AvailableCPUs()

	localStoreSize := mem / 4
	if localStoreSize < MinLocalStoreSize {
		localStoreSize = MinLocalStoreSize
	}

	l1CacheSize := int64(mem / 8)
	if l1CacheSize < MinL1CacheSize {
		l1CacheSize = MinL1CacheSize
	}

	maxPendingSize := localStoreSize / 2

	parallelSyncs := cpus
	if parallelSyncs < MinParallelSyncs {
		parallelSyncs = MinParallelSyncs
	}

	parallelFetches := cpus * 2
	if parallelFetches < MinParallelFetches {
		parallelFetches = MinParallelFetches
	}

	return &DeducedDefaults{
		LocalStoreSize:  localStoreSize,
		L1CacheSize:     l1CacheSize,
		MaxPendingSize:  maxPendingSize,
		ParallelSyncs:   parallelSyncs,
		ParallelFetches: parallelFetches,
		PrefetchWorkers: DefaultPrefetchWorkers,
	}
}

// String returns a human-readable summary of deduced defaults.
func (d *DeducedDefaults) String() string {
	return fmt.Sprintf(
		"LocalStoreSize=%s, L1CacheSize=%s, ParallelSyncs=%d, ParallelFetches=%d, MaxPendingSize=%s, PrefetchWorkers=%d",
		FormatBytes(d.LocalStoreSize),
		FormatBytes(uint64(d.L1CacheSize)),
		d.ParallelSyncs,
		d.ParallelFetches,
		FormatBytes(d.MaxPendingSize),
		d.PrefetchWorkers,
	)
}

const (
	gib = 1024 * 1024 * 1024
	mib = 1024 * 1024
)

// FormatBytes formats a byte count as a human-readable string (e.g., "2 GiB", "512 MiB").
func FormatBytes(b uint64) string {
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
