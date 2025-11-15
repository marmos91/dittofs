package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// NFSMetrics provides observability for NFS adapter operations.
//
// Implementations can collect metrics about NFS requests, connection lifecycle,
// throughput, and errors. This interface is optional - if not provided to the
// NFS adapter, a no-op implementation is used with zero overhead.
//
// Example usage:
//
//	// With metrics enabled
//	metrics := metrics.NewNFSMetrics()
//	adapter := nfs.New(config, metrics)
//
//	// Without metrics (no-op)
//	adapter := nfs.New(config, nil)
type NFSMetrics interface {
	// RecordRequest records a completed NFS request with its procedure name,
	// duration, and outcome.
	//
	// Parameters:
	//   - procedure: NFS procedure name (e.g., "LOOKUP", "READ", "WRITE")
	//   - duration: Time taken to process the request
	//   - err: Error if request failed, nil if successful
	RecordRequest(procedure string, duration time.Duration, err error)

	// RecordRequestStart increments the in-flight request counter.
	// Should be called when starting to process a request.
	//
	// Parameters:
	//   - procedure: NFS procedure name
	RecordRequestStart(procedure string)

	// RecordRequestEnd decrements the in-flight request counter.
	// Should be called when request processing completes.
	//
	// Parameters:
	//   - procedure: NFS procedure name
	RecordRequestEnd(procedure string)

	// RecordBytesTransferred records bytes read or written.
	//
	// Parameters:
	//   - direction: "read" or "write"
	//   - bytes: Number of bytes transferred
	RecordBytesTransferred(direction string, bytes int64)

	// SetActiveConnections updates the current connection count.
	//
	// Parameters:
	//   - count: Current number of active connections
	SetActiveConnections(count int32)

	// RecordConnectionAccepted increments the total accepted connections counter.
	RecordConnectionAccepted()

	// RecordConnectionClosed increments the total closed connections counter.
	RecordConnectionClosed()

	// RecordConnectionForceClosed increments the force-closed connections counter.
	// Called when connections are forcibly closed after shutdown timeout.
	RecordConnectionForceClosed()

	// RecordRateLimitExceeded increments the rate limit exceeded counter.
	// Called when a request is rejected due to rate limiting.
	RecordRateLimitExceeded()
}

// nfsMetrics is the Prometheus implementation of NFSMetrics.
type nfsMetrics struct {
	requestsTotal          *prometheus.CounterVec
	requestDuration        *prometheus.HistogramVec
	requestsInFlight       *prometheus.GaugeVec
	bytesTransferred       *prometheus.CounterVec
	activeConnections      prometheus.Gauge
	connectionsAccepted    prometheus.Counter
	connectionsClosed      prometheus.Counter
	connectionsForceClosed prometheus.Counter
	rateLimitExceeded      prometheus.Counter
}

// NewNFSMetrics creates a new Prometheus-backed NFSMetrics instance.
//
// Returns nil if metrics are not enabled (InitRegistry not called), which
// causes the NFS adapter to use a no-op implementation.
func NewNFSMetrics() NFSMetrics {
	if !IsEnabled() {
		return &noopNFSMetrics{}
	}

	reg := GetRegistry()

	return &nfsMetrics{
		requestsTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_nfs_requests_total",
				Help: "Total number of NFS requests by procedure and status",
			},
			[]string{"procedure", "status"},
		),
		requestDuration: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "dittofs_nfs_request_duration_seconds",
				Help: "Duration of NFS requests in seconds",
				Buckets: []float64{
					0.001, // 1ms
					0.005, // 5ms
					0.01,  // 10ms
					0.025, // 25ms
					0.05,  // 50ms
					0.1,   // 100ms
					0.25,  // 250ms
					0.5,   // 500ms
					1.0,   // 1s
					2.5,   // 2.5s
					5.0,   // 5s
					10.0,  // 10s
				},
			},
			[]string{"procedure"},
		),
		requestsInFlight: promauto.With(reg).NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "dittofs_nfs_requests_in_flight",
				Help: "Current number of NFS requests being processed",
			},
			[]string{"procedure"},
		),
		bytesTransferred: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_nfs_bytes_transferred_total",
				Help: "Total bytes transferred via NFS operations",
			},
			[]string{"direction"}, // read or write
		),
		activeConnections: promauto.With(reg).NewGauge(
			prometheus.GaugeOpts{
				Name: "dittofs_nfs_active_connections",
				Help: "Current number of active NFS connections",
			},
		),
		connectionsAccepted: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Name: "dittofs_nfs_connections_accepted_total",
				Help: "Total number of NFS connections accepted",
			},
		),
		connectionsClosed: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Name: "dittofs_nfs_connections_closed_total",
				Help: "Total number of NFS connections closed",
			},
		),
		connectionsForceClosed: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Name: "dittofs_nfs_connections_force_closed_total",
				Help: "Total number of NFS connections force-closed during shutdown timeout",
			},
		),
		rateLimitExceeded: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Name: "dittofs_nfs_rate_limit_exceeded_total",
				Help: "Total number of NFS requests rejected due to rate limiting",
			},
		),
	}
}

func (m *nfsMetrics) RecordRequest(procedure string, duration time.Duration, err error) {
	status := "success"
	if err != nil {
		status = "error"
	}

	m.requestsTotal.WithLabelValues(procedure, status).Inc()
	m.requestDuration.WithLabelValues(procedure).Observe(duration.Seconds())
}

func (m *nfsMetrics) RecordRequestStart(procedure string) {
	m.requestsInFlight.WithLabelValues(procedure).Inc()
}

func (m *nfsMetrics) RecordRequestEnd(procedure string) {
	m.requestsInFlight.WithLabelValues(procedure).Dec()
}

func (m *nfsMetrics) RecordBytesTransferred(direction string, bytes int64) {
	m.bytesTransferred.WithLabelValues(direction).Add(float64(bytes))
}

func (m *nfsMetrics) SetActiveConnections(count int32) {
	m.activeConnections.Set(float64(count))
}

func (m *nfsMetrics) RecordConnectionAccepted() {
	m.connectionsAccepted.Inc()
}

func (m *nfsMetrics) RecordConnectionClosed() {
	m.connectionsClosed.Inc()
}

func (m *nfsMetrics) RecordConnectionForceClosed() {
	m.connectionsForceClosed.Inc()
}

func (m *nfsMetrics) RecordRateLimitExceeded() {
	m.rateLimitExceeded.Inc()
}

// noopNFSMetrics is a no-op implementation of NFSMetrics with zero overhead.
type noopNFSMetrics struct{}

func (noopNFSMetrics) RecordRequest(procedure string, duration time.Duration, err error) {}
func (noopNFSMetrics) RecordRequestStart(procedure string)                               {}
func (noopNFSMetrics) RecordRequestEnd(procedure string)                                 {}
func (noopNFSMetrics) RecordBytesTransferred(direction string, bytes int64)              {}
func (noopNFSMetrics) SetActiveConnections(count int32)                                  {}
func (noopNFSMetrics) RecordConnectionAccepted()                                         {}
func (noopNFSMetrics) RecordConnectionClosed()                                           {}
func (noopNFSMetrics) RecordConnectionForceClosed()                                      {}
func (noopNFSMetrics) RecordRateLimitExceeded()                                          {}

// NewNoopNFSMetrics returns a no-op implementation of NFSMetrics.
// This is useful for testing or when metrics collection is disabled.
func NewNoopNFSMetrics() NFSMetrics {
	return &noopNFSMetrics{}
}
