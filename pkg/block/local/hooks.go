package local

import "time"

// MetricsRecorder is the narrow inline-metrics seam the local-store
// eviction/backpressure path emits to. *metrics.Metrics satisfies it.
// Defining it here (not in pkg/metrics) keeps the low-level block stores free
// of the prometheus dependency and avoids any import cycle: the runtime hands
// a recorder down via MetricsAware after construction. A recorder whose
// underlying handle is nil makes every method a no-op.
type MetricsRecorder interface {
	// RecordBackpressure records one write stall under local-cache
	// backpressure and the duration it waited for space.
	RecordBackpressure(d time.Duration)
	// RecordEviction records one evicted local-cache chunk and the bytes it
	// reclaimed.
	RecordEviction(bytes int64)
}

// MetricsAware is the optional capability a [LocalStore] exposes to receive
// the inline metrics recorder. The engine probes for it and forwards the
// handle the runtime hands down (shares are constructed before the metrics
// registry exists, so the handle arrives post-construction). Stores that emit
// no inline metrics simply don't implement it.
type MetricsAware interface {
	// SetMetrics installs the inline metrics recorder. Safe to call after the
	// store is serving; the hot path reads it atomically. A nil recorder (or
	// one with a nil underlying handle) disables recording.
	SetMetrics(rec MetricsRecorder)
}
