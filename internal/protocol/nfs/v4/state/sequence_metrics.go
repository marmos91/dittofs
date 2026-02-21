package state

import (
	"github.com/prometheus/client_golang/prometheus"
)

// ============================================================================
// Prometheus Metrics for NFSv4.1 SEQUENCE Operations
// ============================================================================

// SequenceMetrics provides Prometheus metrics for SEQUENCE operation tracking.
// All methods are nil-safe: calls on a nil *SequenceMetrics are no-ops.
type SequenceMetrics struct {
	// SequenceTotal counts the total number of SEQUENCE operations processed.
	SequenceTotal prometheus.Counter

	// ErrorsTotal counts SEQUENCE errors by error type.
	// Label values: "bad_session", "seq_misordered", "replay_hit",
	// "slot_busy", "bad_xdr", "bad_slot", "retry_uncached".
	ErrorsTotal *prometheus.CounterVec

	// ReplayHitsTotal counts successful replay cache hits (cached response returned).
	ReplayHitsTotal prometheus.Counter

	// SlotsInUse tracks the number of slots currently in use per session.
	// Label: session_id (hex string). Limited cardinality (max ~16 sessions/client).
	SlotsInUse *prometheus.GaugeVec

	// ReplayCacheBytes tracks total bytes consumed by cached responses across all sessions.
	ReplayCacheBytes prometheus.Gauge
}

// NewSequenceMetrics creates and registers SEQUENCE metrics with the given
// Prometheus registerer. If reg is nil, metrics are created but not registered
// (useful for testing).
func NewSequenceMetrics(reg prometheus.Registerer) *SequenceMetrics {
	m := &SequenceMetrics{
		SequenceTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "dittofs",
			Subsystem: "nfs",
			Name:      "sequence_total",
			Help:      "Total number of NFSv4.1 SEQUENCE operations processed",
		}),
		ErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dittofs",
			Subsystem: "nfs",
			Name:      "sequence_errors_total",
			Help:      "Total number of NFSv4.1 SEQUENCE errors by type",
		}, []string{"error_type"}),
		ReplayHitsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "dittofs",
			Subsystem: "nfs",
			Name:      "replay_hits_total",
			Help:      "Total number of successful replay cache hits",
		}),
		SlotsInUse: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "dittofs",
			Subsystem: "nfs",
			Name:      "slots_in_use",
			Help:      "Number of slots currently in use per session",
		}, []string{"session_id"}),
		ReplayCacheBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "dittofs",
			Subsystem: "nfs",
			Name:      "replay_cache_bytes",
			Help:      "Total bytes consumed by cached responses across all sessions",
		}),
	}

	if reg != nil {
		reg.MustRegister(
			m.SequenceTotal,
			m.ErrorsTotal,
			m.ReplayHitsTotal,
			m.SlotsInUse,
			m.ReplayCacheBytes,
		)
	}

	return m
}

// RecordSequence increments the total SEQUENCE counter.
func (m *SequenceMetrics) RecordSequence() {
	if m == nil {
		return
	}
	m.SequenceTotal.Inc()
}

// RecordError increments the error counter for the given error type.
func (m *SequenceMetrics) RecordError(errType string) {
	if m == nil {
		return
	}
	m.ErrorsTotal.WithLabelValues(errType).Inc()
}

// RecordReplayHit increments the replay cache hit counter.
func (m *SequenceMetrics) RecordReplayHit() {
	if m == nil {
		return
	}
	m.ReplayHitsTotal.Inc()
}

// SetSlotsInUse sets the number of slots in use for a given session.
func (m *SequenceMetrics) SetSlotsInUse(sessionID string, count float64) {
	if m == nil {
		return
	}
	m.SlotsInUse.WithLabelValues(sessionID).Set(count)
}

// SetReplayCacheBytes sets the total bytes consumed by cached responses.
func (m *SequenceMetrics) SetReplayCacheBytes(bytes float64) {
	if m == nil {
		return
	}
	m.ReplayCacheBytes.Set(bytes)
}
