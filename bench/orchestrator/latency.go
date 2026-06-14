package orchestrator

import "slices"

// LatencyFromSamples computes p50/p95/p99 from a set of per-op nanosecond
// latencies. It returns nil for an empty input (no per-op timings → no latency
// block, which is the omitempty signal in Metrics).
//
// The percentile is the "nearest-rank" value: for percentile p over n sorted
// samples it returns the sample at 1-based rank ceil(p/100 * n), i.e. the
// smallest sample at or above which p% of observations fall. This is the same
// definition the runner's percentile tests assert against, and it never
// interpolates — every reported number is an actually-observed latency.
//
// The input slice is copied before sorting so the caller's slice is left
// untouched.
func LatencyFromSamples(samplesNs []int64) *Latency {
	if len(samplesNs) == 0 {
		return nil
	}
	s := make([]int64, len(samplesNs))
	copy(s, samplesNs)
	slices.Sort(s)
	return &Latency{
		P50Ns: percentile(s, 50),
		P95Ns: percentile(s, 95),
		P99Ns: percentile(s, 99),
	}
}

// percentile returns the nearest-rank percentile p (1..100) of a slice that is
// already sorted ascending. sorted must be non-empty.
func percentile(sorted []int64, p int) int64 {
	n := len(sorted)
	// rank = ceil(p/100 * n), 1-based, clamped to [1, n].
	rank := (p*n + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}
