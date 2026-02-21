package state

import (
	"github.com/prometheus/client_golang/prometheus"
)

// ============================================================================
// Prometheus Metrics for NFSv4.1 Sessions
// ============================================================================

// SessionMetrics provides Prometheus metrics for session lifecycle tracking.
// All methods are nil-safe: calls on a nil *SessionMetrics are no-ops.
type SessionMetrics struct {
	// CreatedTotal counts the total number of sessions created.
	CreatedTotal prometheus.Counter

	// DestroyedTotal counts sessions destroyed, labeled by reason.
	// Reason values: "client_request", "admin_evict", "lease_expired".
	DestroyedTotal *prometheus.CounterVec

	// ActiveGauge tracks the current number of active sessions.
	ActiveGauge prometheus.Gauge

	// DurationHistogram observes session lifetimes in seconds.
	DurationHistogram prometheus.Histogram
}

// NewSessionMetrics creates and registers session metrics with the given
// Prometheus registerer. If reg is nil, metrics are created but not registered
// (useful for testing).
func NewSessionMetrics(reg prometheus.Registerer) *SessionMetrics {
	m := &SessionMetrics{
		CreatedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "dittofs",
			Subsystem: "nfs_sessions",
			Name:      "created_total",
			Help:      "Total number of NFSv4.1 sessions created",
		}),
		DestroyedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dittofs",
			Subsystem: "nfs_sessions",
			Name:      "destroyed_total",
			Help:      "Total number of NFSv4.1 sessions destroyed",
		}, []string{"reason"}),
		ActiveGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "dittofs",
			Subsystem: "nfs_sessions",
			Name:      "active",
			Help:      "Current number of active NFSv4.1 sessions",
		}),
		DurationHistogram: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "dittofs",
			Subsystem: "nfs_sessions",
			Name:      "duration_seconds",
			Help:      "Lifetime of NFSv4.1 sessions in seconds",
			Buckets:   prometheus.ExponentialBuckets(1, 2, 20), // 1s to ~145 hours
		}),
	}

	if reg != nil {
		collectors := []prometheus.Collector{
			m.CreatedTotal,
			m.DestroyedTotal,
			m.ActiveGauge,
			m.DurationHistogram,
		}
		for _, c := range collectors {
			if err := reg.Register(c); err != nil {
				// Ignore AlreadyRegisteredError (server restart re-registers).
				if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
					panic(err)
				}
			}
		}
	}

	return m
}

// recordCreated increments the session created counter and active gauge.
func (m *SessionMetrics) recordCreated() {
	if m == nil {
		return
	}
	m.CreatedTotal.Inc()
	m.ActiveGauge.Inc()
}

// recordDestroyed increments the destroyed counter (with reason label),
// decrements the active gauge, and observes the session duration.
func (m *SessionMetrics) recordDestroyed(reason string, durationSeconds float64) {
	if m == nil {
		return
	}
	m.DestroyedTotal.WithLabelValues(reason).Inc()
	m.ActiveGauge.Dec()
	m.DurationHistogram.Observe(durationSeconds)
}
