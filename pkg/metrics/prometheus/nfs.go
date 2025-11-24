package prometheus

import (
	"time"

	"github.com/marmos91/dittofs/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// nfsMetrics is the Prometheus implementation of metrics.NFSMetrics.
type nfsMetrics struct {
	requestsTotal          *prometheus.CounterVec
	requestDuration        *prometheus.HistogramVec
	requestsInFlight       *prometheus.GaugeVec
	bytesTransferred       *prometheus.CounterVec
	operationSize          *prometheus.HistogramVec
	activeConnections      prometheus.Gauge
	connectionsAccepted    prometheus.Counter
	connectionsClosed      prometheus.Counter
	connectionsForceClosed prometheus.Counter
}

// NewNFSMetrics creates a new Prometheus-backed NFSMetrics instance.
//
// Returns nil if metrics are not enabled (InitRegistry not called).
func NewNFSMetrics() metrics.NFSMetrics {
	if !metrics.IsEnabled() {
		return nil
	}

	reg := metrics.GetRegistry()

	return &nfsMetrics{
		requestsTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_nfs_requests_total",
				Help: "Total number of NFS requests by procedure, share, and status",
			},
			[]string{"procedure", "share", "status", "error_code"},
		),
		requestDuration: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "dittofs_nfs_request_duration_milliseconds",
				Help: "Duration of NFS requests in milliseconds",
				Buckets: []float64{
					1,     // 1ms
					10,    // 10ms
					100,   // 100ms
					1000,  // 1s
					10000, // 10s
				},
			},
			[]string{"procedure", "share"},
		),
		requestsInFlight: promauto.With(reg).NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "dittofs_nfs_requests_in_flight",
				Help: "Current number of NFS requests being processed",
			},
			[]string{"procedure", "share"},
		),
		bytesTransferred: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_nfs_bytes_transferred_total",
				Help: "Total bytes transferred via NFS operations",
			},
			[]string{"procedure", "share", "direction"},
		),
		operationSize: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "dittofs_nfs_operation_size_bytes",
				Help: "Distribution of READ/WRITE operation sizes",
				Buckets: []float64{
					4096,     // 4KB
					65536,    // 64KB
					1048576,  // 1MB
					10485760, // 10MB
				},
			},
			[]string{"operation", "share"},
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
	}
}

func (m *nfsMetrics) RecordRequest(procedure string, share string, duration time.Duration, errorCode string) {
	status := "success"
	if errorCode != "" {
		status = "error"
	}

	m.requestsTotal.WithLabelValues(procedure, share, status, errorCode).Inc()
	m.requestDuration.WithLabelValues(procedure, share).Observe(duration.Seconds() * 1000) // Convert to milliseconds
}

func (m *nfsMetrics) RecordRequestStart(procedure string, share string) {
	m.requestsInFlight.WithLabelValues(procedure, share).Inc()
}

func (m *nfsMetrics) RecordRequestEnd(procedure string, share string) {
	m.requestsInFlight.WithLabelValues(procedure, share).Dec()
}

func (m *nfsMetrics) RecordBytesTransferred(procedure string, share string, direction string, bytes uint64) {
	m.bytesTransferred.WithLabelValues(procedure, share, direction).Add(float64(bytes))
}

func (m *nfsMetrics) RecordOperationSize(operation string, share string, bytes uint64) {
	m.operationSize.WithLabelValues(operation, share).Observe(float64(bytes))
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
