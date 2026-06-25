package block

import (
	"fmt"
	"math"
	"strconv"
)

// SystemDetector provides system resource information for deduction.
// This mirrors sysinfo.Detector but lives in pkg/block to avoid
// importing internal/ from pkg/. The sysinfo.Detector satisfies this
// interface structurally (duck typing).
type SystemDetector interface {
	AvailableMemory() uint64
	AvailableCPUs() int
}

// Minimum floor values for deduced defaults.
const (
	MinLocalStoreSize      uint64 = 256 << 20 // 256 MiB
	MinReadBufferSize      int64  = 64 << 20  // 64 MiB
	MinMaxLogBytes         uint64 = 1 << 30   // 1 GiB
	MinParallelFetches            = 8
	DefaultPrefetchWorkers        = 4

	// AdaptiveUploadDefault is the sentinel for the default upload-concurrency
	// setting: 0 means "auto-tune" — the syncer's adaptive controller ramps the
	// mirror window to saturate the uplink (#1407). Uploads are network-latency
	// bound, NOT CPU bound — a single PUT to a remote region sustains only ~2-3
	// MiB/s, so throughput scales with concurrency until the uplink saturates,
	// and ~5.5% CPU is used even at saturation (#1266/#1398). Deriving
	// concurrency from CPU count (the original behaviour) throttled write→remote
	// bandwidth to roughly cores × per-conn — an 8-core host mirrored at ~14
	// MiB/s while raw parallel PUTs hit ~55 MiB/s. Rather than guess a fixed
	// default, the syncer discovers the right window per remote; operators can
	// still pin it via the parallel_uploads config (--parallel-uploads).
	AdaptiveUploadDefault = 0
)

// DeducedDefaults holds block store sizing values derived from system resources.
type DeducedDefaults struct {
	LocalStoreSize  uint64 // 25% of memory, floor 256 MiB
	ReadBufferSize  int64  // 12.5% of memory, floor 64 MiB
	MaxLogBytes     uint64 // 25% of memory, floor 1 GiB (append-log pressure budget)
	ParallelSyncs   int    // upload concurrency: 0 = adaptive auto-tune (default), >0 = pinned — #1407
	ParallelFetches int    // max(8, cpus*2)
	PrefetchWorkers int    // fixed at DefaultPrefetchWorkers

	// Internal: track whether clamping actually occurred.
	localStoreClamped      bool
	readBufferClamped      bool
	maxLogBytesClamped     bool
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

	rbRaw := mem / 8
	if rbRaw > uint64(math.MaxInt64) {
		rbRaw = uint64(math.MaxInt64)
	}
	readBufferSize := int64(rbRaw)
	readBufferClamped := readBufferSize < MinReadBufferSize
	if readBufferClamped {
		readBufferSize = MinReadBufferSize
	}

	// MaxLogBytes is the per-share append-log pressure budget: the on-disk
	// append log buffers freshly-written bytes before the async rollup folds
	// them into CAS chunks, and AppendWrite stalls (ErrPressureTimeout) once
	// logBytesTotal exceeds this budget. Because the log is disk-backed
	// pre-flush write data whose in-flight working set is bounded by how fast
	// the host can absorb writes, we size it relative to available memory —
	// 25% of RAM, the same fraction used for the local store — with a 1 GiB
	// floor so the budget never drops below the historical fixed default on
	// small machines. Reporters on large-RAM hosts get a proportionally larger
	// budget instead of a hardcoded 1 GiB ceiling.
	maxLogBytes := mem / 4
	maxLogBytesClamped := maxLogBytes < MinMaxLogBytes
	if maxLogBytesClamped {
		maxLogBytes = MinMaxLogBytes
	}

	// Upload concurrency is network-bound, not CPU-bound, so the default is
	// adaptive auto-tuning rather than a function of cpus (#1407 — see
	// AdaptiveUploadDefault). 0 flows through SyncerDefaults → cfg.ParallelUploads
	// unchanged, which the syncer reads as "auto-tune". An explicit
	// per-remote parallel_uploads pins a fixed window instead.
	parallelSyncs := AdaptiveUploadDefault

	parallelFetches := cpus * 2
	parallelFetchesClamped := parallelFetches < MinParallelFetches
	if parallelFetchesClamped {
		parallelFetches = MinParallelFetches
	}

	return &DeducedDefaults{
		LocalStoreSize:         localStoreSize,
		ReadBufferSize:         readBufferSize,
		MaxLogBytes:            maxLogBytes,
		ParallelSyncs:          parallelSyncs,
		ParallelFetches:        parallelFetches,
		PrefetchWorkers:        DefaultPrefetchWorkers,
		localStoreClamped:      localStoreClamped,
		readBufferClamped:      readBufferClamped,
		maxLogBytesClamped:     maxLogBytesClamped,
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
	if d.readBufferClamped {
		floors = append(floors, fmt.Sprintf("read_buffer_size floored at %s", FormatBytes(uint64(MinReadBufferSize))))
	}
	if d.maxLogBytesClamped {
		floors = append(floors, fmt.Sprintf("max_log_bytes floored at %s", FormatBytes(MinMaxLogBytes)))
	}
	if d.parallelFetchesClamped {
		floors = append(floors, fmt.Sprintf("parallel_fetches floored at %d", MinParallelFetches))
	}
	return floors
}

// String returns a human-readable summary of deduced defaults.
func (d *DeducedDefaults) String() string {
	// ParallelSyncs <= 0 is the adaptive sentinel (auto-tune upload concurrency).
	parallelSyncs := strconv.Itoa(d.ParallelSyncs)
	if d.ParallelSyncs <= 0 {
		parallelSyncs = "adaptive"
	}
	return fmt.Sprintf(
		"LocalStoreSize=%s, ReadBufferSize=%s, ParallelSyncs=%s, ParallelFetches=%d, MaxLogBytes=%s, PrefetchWorkers=%d",
		FormatBytes(d.LocalStoreSize),
		FormatBytes(uint64(d.ReadBufferSize)),
		parallelSyncs,
		d.ParallelFetches,
		FormatBytes(d.MaxLogBytes),
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
