package blockstore

import (
	"sync"
	"time"
)

// LatencyRecorder accumulates per-op wall-clock timings during a timed loop so
// the runner can report p50/p95/p99 alongside the aggregate throughput numbers.
//
// It is concurrency-safe: the concurrent (storm / fan-out) workloads record
// from many goroutines, while the serial workloads record from one. Recording
// is a single append under a mutex — cheap relative to a block-store op — so it
// does not perturb the measured numbers materially.
//
// The recorder stores raw nanosecond samples; percentile computation lives in
// the orchestrator package (LatencyFromSamples) so the math has one home and is
// unit-tested against known inputs.
type LatencyRecorder struct {
	mu        sync.Mutex
	samples   []int64
	succeeded int64
	failed    int64
}

// NewLatencyRecorder returns a recorder pre-sized for the expected op count so
// the timed loop does not reallocate the sample slice. hint <= 0 is tolerated.
func NewLatencyRecorder(hint int) *LatencyRecorder {
	if hint < 0 {
		hint = 0
	}
	return &LatencyRecorder{samples: make([]int64, 0, hint)}
}

// Record adds one op's latency and whether it succeeded. A nil recorder is a
// no-op so callers can record unconditionally.
func (r *LatencyRecorder) Record(d time.Duration, ok bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.samples = append(r.samples, d.Nanoseconds())
	if ok {
		r.succeeded++
	} else {
		r.failed++
	}
	r.mu.Unlock()
}

// Samples returns a copy of the recorded nanosecond latencies. The copy keeps
// the recorder's internal slice from being mutated by a percentile pass that
// sorts in place.
func (r *LatencyRecorder) Samples() []int64 {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int64, len(r.samples))
	copy(out, r.samples)
	return out
}

// Counts returns the succeeded / failed op tallies.
func (r *LatencyRecorder) Counts() (succeeded, failed int64) {
	if r == nil {
		return 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.succeeded, r.failed
}
