package journal

import "time"

// MetricsRecorder is the sink for the two observations this store makes. It is
// declared here because this package imports nothing outside the standard
// library, so a type its call sites can name cannot live anywhere else.
type MetricsRecorder interface {
	// RecordBackpressure records one append — a client write or a cold-read
	// fault-in — that was held waiting for the store to free space, and for how
	// long.
	RecordBackpressure(d time.Duration)
	// RecordEviction records one segment reclaimed under disk pressure and the
	// on-disk bytes it freed. The operator drain reclaims segments too and is
	// deliberately not routed here.
	RecordEviction(bytes int64)
}

// SetMetrics installs the metrics recorder, safely against a store that is
// already serving. Passing nil, or a recorder wrapping a nil handle, disables
// recording.
func (s *Store) SetMetrics(rec MetricsRecorder) {
	s.metrics.Store(&rec)
}

// recordEviction and recordBackpressure are the nil-tolerant call-site wrappers.
// Two nils reach here and both mean "no recorder": no SetMetrics call yet (a nil
// cell), and SetMetrics(nil) (a cell holding a nil interface). A third shape — a
// non-nil interface holding a nil handle, which is what a server started without
// metrics installs — is the recorder's own to absorb, and *metrics.Metrics does.
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
