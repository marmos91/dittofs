package nsm

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics provides Prometheus metrics for NSM operations.
//
// All metrics use the nsm_ prefix to distinguish them from other protocol metrics.
// Follows the nil receiver pattern - all methods handle nil gracefully
// for zero overhead when metrics are disabled.
type Metrics struct {
	// RequestsTotal counts NSM requests by procedure and result
	RequestsTotal *prometheus.CounterVec

	// RequestDuration tracks request latency
	RequestDuration *prometheus.HistogramVec

	// ClientsRegistered tracks current number of monitored clients
	ClientsRegistered prometheus.Gauge

	// NotificationsTotal counts SM_NOTIFY callbacks by result
	NotificationsTotal *prometheus.CounterVec

	// NotificationDuration tracks SM_NOTIFY callback latency
	NotificationDuration prometheus.Histogram

	// CrashesDetected counts client crashes detected
	CrashesDetected prometheus.Counter

	// CrashCleanups counts crash cleanup operations
	CrashCleanups prometheus.Counter

	// LocksCleanedOnCrash counts locks released due to crash
	LocksCleanedOnCrash prometheus.Counter
}

// NewMetrics creates and registers NSM metrics.
//
// Parameters:
//   - reg: Prometheus registerer. Pass nil to create metrics without registration
//     (useful for testing or when metrics are disabled).
//
// Returns a configured Metrics struct with all metrics registered.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		RequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "nsm_requests_total",
				Help: "Total NSM requests by procedure and result",
			},
			[]string{"procedure", "result"},
		),

		RequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "nsm_request_duration_seconds",
				Help:    "NSM request duration in seconds",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"procedure"},
		),

		ClientsRegistered: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "nsm_clients_registered",
				Help: "Current number of clients registered for monitoring",
			},
		),

		NotificationsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "nsm_notifications_total",
				Help: "Total SM_NOTIFY callbacks by result (started, success, failed)",
			},
			[]string{"result"},
		),

		NotificationDuration: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "nsm_notification_duration_seconds",
				Help:    "SM_NOTIFY callback duration in seconds",
				Buckets: prometheus.DefBuckets,
			},
		),

		CrashesDetected: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "nsm_crashes_detected_total",
				Help: "Total client crashes detected",
			},
		),

		CrashCleanups: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "nsm_crash_cleanups_total",
				Help: "Total crash cleanup operations performed",
			},
		),

		LocksCleanedOnCrash: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "nsm_locks_cleaned_on_crash_total",
				Help: "Total locks released due to client crashes",
			},
		),
	}

	if reg != nil {
		reg.MustRegister(
			m.RequestsTotal,
			m.RequestDuration,
			m.ClientsRegistered,
			m.NotificationsTotal,
			m.NotificationDuration,
			m.CrashesDetected,
			m.CrashCleanups,
			m.LocksCleanedOnCrash,
		)
	}

	return m
}

// RecordRequest records metrics for a completed NSM request.
//
// Parameters:
//   - procedure: Procedure name (e.g., "MON", "UNMON", "STAT")
//   - success: Whether the request succeeded
//   - duration: Request duration in seconds
//
// Safe to call on nil receiver.
func (m *Metrics) RecordRequest(procedure string, success bool, duration float64) {
	if m == nil {
		return
	}

	result := "success"
	if !success {
		result = "error"
	}

	m.RequestsTotal.WithLabelValues(procedure, result).Inc()
	m.RequestDuration.WithLabelValues(procedure).Observe(duration)
}

// IncrementClients increments the registered client count.
//
// Safe to call on nil receiver.
func (m *Metrics) IncrementClients() {
	if m == nil {
		return
	}
	m.ClientsRegistered.Inc()
}

// DecrementClients decrements the registered client count.
//
// Safe to call on nil receiver.
func (m *Metrics) DecrementClients() {
	if m == nil {
		return
	}
	m.ClientsRegistered.Dec()
}

// SetClients sets the registered client count.
//
// Safe to call on nil receiver.
func (m *Metrics) SetClients(count float64) {
	if m == nil {
		return
	}
	m.ClientsRegistered.Set(count)
}

// RecordNotification records a notification result.
//
// Parameters:
//   - result: "started", "success", or "failed"
//
// Safe to call on nil receiver.
func (m *Metrics) RecordNotification(result string) {
	if m == nil {
		return
	}
	m.NotificationsTotal.WithLabelValues(result).Inc()
}

// ObserveNotificationDuration records notification duration.
//
// Safe to call on nil receiver.
func (m *Metrics) ObserveNotificationDuration(duration float64) {
	if m == nil {
		return
	}
	m.NotificationDuration.Observe(duration)
}

// RecordCrashDetected increments the crash detection counter.
//
// Safe to call on nil receiver.
func (m *Metrics) RecordCrashDetected() {
	if m == nil {
		return
	}
	m.CrashesDetected.Inc()
}

// RecordCrashCleanup increments the crash cleanup counter.
//
// Safe to call on nil receiver.
func (m *Metrics) RecordCrashCleanup() {
	if m == nil {
		return
	}
	m.CrashCleanups.Inc()
}

// RecordLocksCleanedOnCrash adds to the lock cleanup counter.
//
// Parameters:
//   - count: Number of locks released
//
// Safe to call on nil receiver.
func (m *Metrics) RecordLocksCleanedOnCrash(count int) {
	if m == nil {
		return
	}
	m.LocksCleanedOnCrash.Add(float64(count))
}

// NullMetrics returns nil, which acts as a no-op metrics collector.
// All Metrics methods handle nil receiver gracefully.
func NullMetrics() *Metrics {
	return nil
}
