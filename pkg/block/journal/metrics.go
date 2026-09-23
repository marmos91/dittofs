package journal

import "time"

// MetricsRecorder is the narrow sink the store's eviction and write-path
// backpressure emit to. It is declared here, in the store's own package, so the
// store can name it: the recorder has to be a type the eviction and ensureSpace
// call sites can reference, and this package imports nothing outside the
// standard library, so the type cannot live anywhere the store also imports.
//
// It carries only the two observations this store makes. A recorder that wraps a
// nil handle is still safe to install — the implementation's methods no-op — but
// call sites go through recordEviction/recordBackpressure, which also tolerate no
// recorder at all.
type MetricsRecorder interface {
	// RecordBackpressure records one write that stalled waiting for the local
	// store to free space, and how long it waited.
	RecordBackpressure(d time.Duration)
	// RecordEviction records one segment reclaimed under storage pressure and
	// the on-disk bytes it freed.
	RecordEviction(bytes int64)
}

// SetMetrics installs the metrics recorder. It is safe to call while the store
// is already serving: the handle arrives after construction (a share's store is
// built before the process has a metrics registry), so installation races the
// write and eviction paths and the cell is swapped atomically rather than
// assigned. Passing nil, or a recorder wrapping a nil handle, disables recording.
func (s *Store) SetMetrics(rec MetricsRecorder) {
	s.metrics.Store(&rec)
}

// recordEviction and recordBackpressure are the nil-tolerant call-site wrappers.
// Two nils are possible and both mean "no recorder": no SetMetrics call yet (nil
// cell), and SetMetrics(nil) (a cell holding a nil interface).
func (s *Store) recordEviction(bytes int64) {
	if rec := s.metrics.Load(); rec != nil && *rec != nil {
		(*rec).RecordEviction(bytes)
	}
}

func (s *Store) recordBackpressure(d time.Duration) {
	if rec := s.metrics.Load(); rec != nil && *rec != nil {
		(*rec).RecordBackpressure(d)
	}
}
