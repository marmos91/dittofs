package blockstore

import (
	"fmt"
	"math"
)

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

	// Internal: track whether clamping actually occurred.
	localStoreClamped      bool
	l1CacheClamped         bool
	parallelSyncsClamped   bool
	parallelFetchesClamped bool
}

// DeduceDefaults derives block store sizing from detected system resources.
func DeduceDefaults(d SystemDetector) *DeducedDefaults {
	mem := d.AvailableMemory()
	cpus := d.AvailableCPUs()

	localStoreSize := mem / 4
	localStoreClamped := localStoreSize < MinLocalStoreSize
	if localStoreClamped {
		localStoreSize = MinLocalStoreSize
	}

	l1Raw := mem / 8
	if l1Raw > uint64(math.MaxInt64) {
		l1Raw = uint64(math.MaxInt64)
	}
	l1CacheSize := int64(l1Raw)
	l1CacheClamped := l1CacheSize < MinL1CacheSize
	if l1CacheClamped {
		l1CacheSize = MinL1CacheSize
	}

	maxPendingSize := localStoreSize / 2

	parallelSyncs := cpus
	parallelSyncsClamped := parallelSyncs < MinParallelSyncs
	if parallelSyncsClamped {
		parallelSyncs = MinParallelSyncs
	}

	parallelFetches := cpus * 2
	parallelFetchesClamped := parallelFetches < MinParallelFetches
	if parallelFetchesClamped {
		parallelFetches = MinParallelFetches
	}

	return &DeducedDefaults{
		LocalStoreSize:         localStoreSize,
		L1CacheSize:            l1CacheSize,
		MaxPendingSize:         maxPendingSize,
		ParallelSyncs:          parallelSyncs,
		ParallelFetches:        parallelFetches,
		PrefetchWorkers:        DefaultPrefetchWorkers,
		localStoreClamped:      localStoreClamped,
		l1CacheClamped:         l1CacheClamped,
		parallelSyncsClamped:   parallelSyncsClamped,
		parallelFetchesClamped: parallelFetchesClamped,
	}
}

// HitFloors returns a list of human-readable descriptions for any deduced
// values that were clamped to their minimum floor. An empty slice means no
// floors were hit. Only reports values that were actually clamped (not those
// that naturally computed to the minimum).
func (d *DeducedDefaults) HitFloors() []string {
	var floors []string
	if d.localStoreClamped {
		floors = append(floors, fmt.Sprintf("local_store_size floored at %s", FormatBytes(MinLocalStoreSize)))
	}
	if d.l1CacheClamped {
		floors = append(floors, fmt.Sprintf("l1_cache_size floored at %s", FormatBytes(uint64(MinL1CacheSize))))
	}
	if d.parallelSyncsClamped {
		floors = append(floors, fmt.Sprintf("parallel_syncs floored at %d", MinParallelSyncs))
	}
	if d.parallelFetchesClamped {
		floors = append(floors, fmt.Sprintf("parallel_fetches floored at %d", MinParallelFetches))
	}
	return floors
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

// ClampToInt64 safely converts a uint64 to int64, clamping at math.MaxInt64.
func ClampToInt64(v uint64) int64 {
	if v > uint64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(v)
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
