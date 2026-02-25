package gss

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// GSSMetrics tracks Prometheus metrics for RPCSEC_GSS operations.
//
// All metrics use the "dittofs_gss_" prefix to distinguish them from
// NFS/NLM/NSM metrics. Methods handle nil receiver gracefully, so a nil
// *GSSMetrics acts as a no-op (zero overhead when metrics are disabled).
//
// Metrics tracked:
//   - Context creations (success/failure)
//   - Context destructions
//   - Active context count (gauge)
//   - Authentication failures by reason
//   - Data requests by service level
//   - Request duration by operation type
type GSSMetrics struct {
	// ContextCreations counts GSS context creation attempts by result.
	// Labels: result=[success, failure]
	ContextCreations *prometheus.CounterVec

	// ContextDestructions counts GSS context teardowns.
	ContextDestructions prometheus.Counter

	// ActiveContexts tracks the current number of active GSS contexts.
	ActiveContexts prometheus.Gauge

	// AuthFailures counts authentication failures by reason.
	// Labels: reason=[credential_problem, context_problem, sequence_violation,
	//                  integrity_failure, privacy_failure]
	AuthFailures *prometheus.CounterVec

	// DataRequests counts DATA requests by service level.
	// Labels: service=[none, integrity, privacy]
	DataRequests *prometheus.CounterVec

	// RequestDuration tracks request processing time by operation.
	// Labels: operation=[init, data, destroy]
	RequestDuration *prometheus.HistogramVec
}

var (
	// gssMetricsOnce ensures GSS metrics are registered exactly once.
	gssMetricsOnce sync.Once
	// gssMetricsInstance holds the singleton GSS metrics instance.
	gssMetricsInstance *GSSMetrics
)

// NewGSSMetrics creates and registers GSS Prometheus metrics.
//
// If registerer is nil, prometheus.DefaultRegisterer is used.
// This function is idempotent - uses sync.Once to ensure metrics are
// registered exactly once, even if called multiple times (e.g., when
// adapters are restarted).
//
// Parameters:
//   - registerer: Prometheus registerer (nil = DefaultRegisterer)
//
// Returns:
//   - *GSSMetrics: Configured metrics struct with all metrics registered
func NewGSSMetrics(registerer prometheus.Registerer) *GSSMetrics {
	gssMetricsOnce.Do(func() {
		if registerer == nil {
			registerer = prometheus.DefaultRegisterer
		}

		m := &GSSMetrics{
			ContextCreations: prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "dittofs_gss_context_creations_total",
					Help: "Total GSS context creation attempts by result",
				},
				[]string{"result"},
			),
			ContextDestructions: prometheus.NewCounter(
				prometheus.CounterOpts{
					Name: "dittofs_gss_context_destructions_total",
					Help: "Total GSS context destructions",
				},
			),
			ActiveContexts: prometheus.NewGauge(
				prometheus.GaugeOpts{
					Name: "dittofs_gss_active_contexts",
					Help: "Current number of active GSS contexts",
				},
			),
			AuthFailures: prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "dittofs_gss_auth_failures_total",
					Help: "Total GSS authentication failures by reason",
				},
				[]string{"reason"},
			),
			DataRequests: prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "dittofs_gss_data_requests_total",
					Help: "Total GSS DATA requests by service level",
				},
				[]string{"service"},
			),
			RequestDuration: prometheus.NewHistogramVec(
				prometheus.HistogramOpts{
					Name:    "dittofs_gss_request_duration_seconds",
					Help:    "GSS request processing duration in seconds",
					Buckets: prometheus.DefBuckets,
				},
				[]string{"operation"},
			),
		}

		// Register all metrics
		registerer.MustRegister(
			m.ContextCreations,
			m.ContextDestructions,
			m.ActiveContexts,
			m.AuthFailures,
			m.DataRequests,
			m.RequestDuration,
		)

		gssMetricsInstance = m
	})

	return gssMetricsInstance
}

// RecordContextCreation records a GSS context creation attempt.
//
// Parameters:
//   - success: true if context was created successfully, false on failure
func (m *GSSMetrics) RecordContextCreation(success bool) {
	if m == nil {
		return
	}
	if success {
		m.ContextCreations.WithLabelValues("success").Inc()
		m.ActiveContexts.Inc()
	} else {
		m.ContextCreations.WithLabelValues("failure").Inc()
	}
}

// RecordContextDestruction records a GSS context teardown.
func (m *GSSMetrics) RecordContextDestruction() {
	if m == nil {
		return
	}
	m.ContextDestructions.Inc()
	m.ActiveContexts.Dec()
}

// RecordAuthFailure records a GSS authentication failure.
//
// Parameters:
//   - reason: Failure reason (credential_problem, context_problem,
//     sequence_violation, integrity_failure, privacy_failure)
func (m *GSSMetrics) RecordAuthFailure(reason string) {
	if m == nil {
		return
	}
	m.AuthFailures.WithLabelValues(reason).Inc()
}

// RecordDataRequest records a GSS DATA request with service level and duration.
//
// Parameters:
//   - service: Service level name (none, integrity, privacy)
//   - duration: Request processing time
func (m *GSSMetrics) RecordDataRequest(service string, duration time.Duration) {
	if m == nil {
		return
	}
	m.DataRequests.WithLabelValues(service).Inc()
	m.RequestDuration.WithLabelValues("data").Observe(duration.Seconds())
}

// RecordInitDuration records the duration of a GSS INIT operation.
//
// Parameters:
//   - duration: INIT processing time
func (m *GSSMetrics) RecordInitDuration(duration time.Duration) {
	if m == nil {
		return
	}
	m.RequestDuration.WithLabelValues("init").Observe(duration.Seconds())
}

// RecordDestroyDuration records the duration of a GSS DESTROY operation.
//
// Parameters:
//   - duration: DESTROY processing time
func (m *GSSMetrics) RecordDestroyDuration(duration time.Duration) {
	if m == nil {
		return
	}
	m.RequestDuration.WithLabelValues("destroy").Observe(duration.Seconds())
}

// serviceLevelName returns the string name for a GSS service level.
func serviceLevelName(service uint32) string {
	switch service {
	case RPCGSSSvcNone:
		return "none"
	case RPCGSSSvcIntegrity:
		return "integrity"
	case RPCGSSSvcPrivacy:
		return "privacy"
	default:
		return "unknown"
	}
}
