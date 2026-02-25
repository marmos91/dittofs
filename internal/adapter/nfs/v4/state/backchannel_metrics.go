package state

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ============================================================================
// Prometheus Metrics for NFSv4.1 Backchannel Operations
// ============================================================================

// BackchannelMetrics provides Prometheus metrics for backchannel callback tracking.
// All methods are nil-safe: calls on a nil *BackchannelMetrics are no-ops.
type BackchannelMetrics struct {
	// callbackTotal counts the total number of backchannel callbacks attempted.
	callbackTotal prometheus.Counter

	// callbackFailures counts the total number of backchannel callback failures.
	callbackFailures prometheus.Counter

	// callbackDuration observes the duration of backchannel callback round-trips.
	callbackDuration prometheus.Histogram

	// callbackRetries counts the total number of backchannel callback retries.
	callbackRetries prometheus.Counter
}

// NewBackchannelMetrics creates and registers backchannel metrics with the given
// Prometheus registerer. If reg is nil, metrics are created but not registered
// (useful for testing).
//
// On re-registration (server restart), existing collectors from the registry
// are reused so that metrics continue to be exported correctly.
func NewBackchannelMetrics(reg prometheus.Registerer) *BackchannelMetrics {
	m := &BackchannelMetrics{
		callbackTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "dittofs",
			Subsystem: "nfs_backchannel",
			Name:      "callbacks_total",
			Help:      "Total number of backchannel callbacks attempted",
		}),
		callbackFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "dittofs",
			Subsystem: "nfs_backchannel",
			Name:      "callback_failures_total",
			Help:      "Total number of backchannel callback failures",
		}),
		callbackDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "dittofs",
			Subsystem: "nfs_backchannel",
			Name:      "callback_duration_seconds",
			Help:      "Duration of backchannel callback round-trips in seconds",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10},
		}),
		callbackRetries: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "dittofs",
			Subsystem: "nfs_backchannel",
			Name:      "callback_retries_total",
			Help:      "Total number of backchannel callback retries",
		}),
	}

	if reg != nil {
		m.callbackTotal = registerOrReuse(reg, m.callbackTotal).(prometheus.Counter)
		m.callbackFailures = registerOrReuse(reg, m.callbackFailures).(prometheus.Counter)
		m.callbackDuration = registerOrReuse(reg, m.callbackDuration).(prometheus.Histogram)
		m.callbackRetries = registerOrReuse(reg, m.callbackRetries).(prometheus.Counter)
	}

	return m
}

// RecordCallback increments the total callback counter.
func (m *BackchannelMetrics) RecordCallback() {
	if m == nil {
		return
	}
	m.callbackTotal.Inc()
}

// RecordFailure increments the callback failure counter.
func (m *BackchannelMetrics) RecordFailure() {
	if m == nil {
		return
	}
	m.callbackFailures.Inc()
}

// RecordRetry increments the callback retry counter.
func (m *BackchannelMetrics) RecordRetry() {
	if m == nil {
		return
	}
	m.callbackRetries.Inc()
}

// ObserveDuration observes a callback round-trip duration.
func (m *BackchannelMetrics) ObserveDuration(d time.Duration) {
	if m == nil {
		return
	}
	m.callbackDuration.Observe(d.Seconds())
}
