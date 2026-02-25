// Package nlm provides NLM (Network Lock Manager) protocol implementation.
package nlm

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics tracks NLM-specific Prometheus metrics.
//
// All metrics use the nlm_ prefix to distinguish them from NFS metrics.
// Metrics are designed for observability into NLM lock operations without
// affecting performance when not used.
type Metrics struct {
	// RequestsTotal counts NLM requests by procedure and status
	RequestsTotal *prometheus.CounterVec

	// RequestDuration tracks latency distribution
	RequestDuration *prometheus.HistogramVec

	// BlockingQueueSize tracks current queue depth (total waiters)
	BlockingQueueSize prometheus.Gauge

	// CallbacksTotal counts NLM_GRANTED callbacks by result
	CallbacksTotal *prometheus.CounterVec

	// CallbackDuration tracks callback latency
	CallbackDuration prometheus.Histogram

	// LocksHeld tracks current number of locks held
	LocksHeld prometheus.Gauge
}

// NewMetrics creates NLM metrics with nlm_ prefix.
//
// Parameters:
//   - reg: Prometheus registerer (typically prometheus.DefaultRegisterer)
//
// Returns a configured Metrics struct with all metrics registered.
// Panics if registration fails (expected during initialization only).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		RequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "nlm_requests_total",
				Help: "Total NLM requests by procedure and status",
			},
			[]string{"procedure", "status"},
		),
		RequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "nlm_request_duration_seconds",
				Help:    "NLM request duration in seconds",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"procedure"},
		),
		BlockingQueueSize: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "nlm_blocking_queue_size",
				Help: "Current number of waiting lock requests across all files",
			},
		),
		CallbacksTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "nlm_callbacks_total",
				Help: "Total NLM_GRANTED callbacks by result",
			},
			[]string{"result"}, // "success", "failed", "cancelled"
		),
		CallbackDuration: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "nlm_callback_duration_seconds",
				Help:    "NLM_GRANTED callback duration in seconds",
				Buckets: prometheus.DefBuckets,
			},
		),
		LocksHeld: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "nlm_locks_held",
				Help: "Current number of NLM locks held across all files",
			},
		),
	}

	// Register all metrics
	reg.MustRegister(
		m.RequestsTotal,
		m.RequestDuration,
		m.BlockingQueueSize,
		m.CallbacksTotal,
		m.CallbackDuration,
		m.LocksHeld,
	)

	return m
}

// RecordRequest records an NLM request completion.
//
// Parameters:
//   - procedure: Procedure name (e.g., "LOCK", "UNLOCK", "TEST")
//   - status: Status name (e.g., "NLM4_GRANTED", "NLM4_DENIED")
//   - durationSeconds: Request duration in seconds
func (m *Metrics) RecordRequest(procedure, status string, durationSeconds float64) {
	if m == nil {
		return
	}
	m.RequestsTotal.WithLabelValues(procedure, status).Inc()
	m.RequestDuration.WithLabelValues(procedure).Observe(durationSeconds)
}

// RecordCallback records an NLM_GRANTED callback completion.
//
// Parameters:
//   - result: Result of callback ("success", "failed", "cancelled")
//   - durationSeconds: Callback duration in seconds
func (m *Metrics) RecordCallback(result string, durationSeconds float64) {
	if m == nil {
		return
	}
	m.CallbacksTotal.WithLabelValues(result).Inc()
	m.CallbackDuration.Observe(durationSeconds)
}

// SetBlockingQueueSize updates the blocking queue size gauge.
//
// Parameters:
//   - size: Current number of waiters in the blocking queue
func (m *Metrics) SetBlockingQueueSize(size int) {
	if m == nil {
		return
	}
	m.BlockingQueueSize.Set(float64(size))
}

// SetLocksHeld updates the locks held gauge.
//
// Parameters:
//   - count: Current number of NLM locks held
func (m *Metrics) SetLocksHeld(count int) {
	if m == nil {
		return
	}
	m.LocksHeld.Set(float64(count))
}

// NullMetrics returns nil, which acts as a no-op metrics collector.
// All Metrics methods handle nil receiver gracefully.
func NullMetrics() *Metrics {
	return nil
}
