package metrics

import (
	"time"

	"github.com/marmos91/dittofs/pkg/store/content/s3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// s3Metrics is the Prometheus implementation of s3.S3Metrics interface.
//
// This implementation collects metrics about S3 operations including:
//   - Operation counts (GetObject, PutObject, etc.)
//   - Operation latency
//   - Bytes transferred
//   - Error rates
//   - Multipart upload statistics
//   - Cache hit/miss rates
type s3Metrics struct {
	operationsTotal    *prometheus.CounterVec
	operationDuration  *prometheus.HistogramVec
	bytesTransferred   *prometheus.CounterVec
	multipartUploads   *prometheus.CounterVec
	statsCacheHits     prometheus.Counter
	statsCacheMisses   prometheus.Counter
	errorsTotal        *prometheus.CounterVec
	flushPhaseDuration *prometheus.HistogramVec
	flushOperations    *prometheus.CounterVec
	flushBytes         *prometheus.HistogramVec
}

// NewS3Metrics creates a new Prometheus-backed S3Metrics instance.
//
// Returns nil if metrics are not enabled (InitRegistry not called), which
// causes the S3 content store to use the built-in no-op implementation.
//
// This implements the s3.S3Metrics interface from pkg/content/s3/s3_metrics.go.
func NewS3Metrics() s3.S3Metrics {
	if !IsEnabled() {
		return nil // S3 content store will use noopMetrics
	}

	reg := GetRegistry()

	return &s3Metrics{
		operationsTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_s3_operations_total",
				Help: "Total number of S3 operations by operation type and status",
			},
			[]string{"operation", "status"},
		),
		operationDuration: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "dittofs_s3_operation_duration_seconds",
				Help: "Duration of S3 operations in seconds",
				Buckets: []float64{
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
					30.0,  // 30s
				},
			},
			[]string{"operation"},
		),
		bytesTransferred: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_s3_bytes_transferred_total",
				Help: "Total bytes transferred in S3 operations",
			},
			[]string{"operation"}, // read or write
		),
		multipartUploads: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_s3_multipart_uploads_total",
				Help: "Total number of S3 multipart uploads by status",
			},
			[]string{"status"}, // initiated, completed, aborted
		),
		statsCacheHits: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Name: "dittofs_s3_stats_cache_hits_total",
				Help: "Total number of S3 storage stats cache hits",
			},
		),
		statsCacheMisses: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Name: "dittofs_s3_stats_cache_misses_total",
				Help: "Total number of S3 storage stats cache misses",
			},
		),
		errorsTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_s3_errors_total",
				Help: "Total number of S3 operation errors by operation type",
			},
			[]string{"operation"},
		),
		flushPhaseDuration: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "dittofs_s3_flush_phase_duration_seconds",
				Help: "Duration of individual flush phases (cache_read, s3_upload, cache_clear)",
				Buckets: []float64{
					0.001, // 1ms
					0.01,  // 10ms
					0.1,   // 100ms
					0.5,   // 500ms
					1.0,   // 1s
					2.5,   // 2.5s
					5.0,   // 5s
					10.0,  // 10s
					30.0,  // 30s
					60.0,  // 1min
				},
			},
			[]string{"phase"},
		),
		flushOperations: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_s3_flush_operations_total",
				Help: "Total number of flush operations by reason and status",
			},
			[]string{"reason", "status"},
		),
		flushBytes: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "dittofs_s3_flush_bytes",
				Help: "Size of flushed content in bytes by reason",
				Buckets: []float64{
					4096,       // 4KB
					65536,      // 64KB
					524288,     // 512KB
					1048576,    // 1MB
					5242880,    // 5MB
					10485760,   // 10MB
					52428800,   // 50MB
					104857600,  // 100MB
					524288000,  // 500MB
					1073741824, // 1GB
				},
			},
			[]string{"reason"},
		),
	}
}

// ObserveOperation implements s3.S3Metrics.ObserveOperation
func (m *s3Metrics) ObserveOperation(operation string, duration time.Duration, err error) {
	status := "success"
	if err != nil {
		status = "error"
		m.errorsTotal.WithLabelValues(operation).Inc()
	}

	m.operationsTotal.WithLabelValues(operation, status).Inc()
	m.operationDuration.WithLabelValues(operation).Observe(duration.Seconds())
}

// RecordBytes implements s3.S3Metrics.RecordBytes
func (m *s3Metrics) RecordBytes(operation string, bytes int64) {
	m.bytesTransferred.WithLabelValues(operation).Add(float64(bytes))
}

// RecordMultipartUpload records a multipart upload event.
//
// This is an extension method not required by the s3.S3Metrics interface,
// but useful for tracking multipart upload lifecycle.
//
// Parameters:
//   - status: "initiated", "completed", or "aborted"
func (m *s3Metrics) RecordMultipartUpload(status string) {
	m.multipartUploads.WithLabelValues(status).Inc()
}

// RecordStatsCacheHit records a hit in the storage stats cache.
//
// This is an extension method not required by the s3.S3Metrics interface,
// but useful for tracking cache effectiveness.
func (m *s3Metrics) RecordStatsCacheHit() {
	m.statsCacheHits.Inc()
}

// RecordStatsCacheMiss records a miss in the storage stats cache.
//
// This is an extension method not required by the s3.S3Metrics interface,
// but useful for tracking cache effectiveness.
func (m *s3Metrics) RecordStatsCacheMiss() {
	m.statsCacheMisses.Inc()
}

// ObserveFlushPhase implements s3.S3Metrics.ObserveFlushPhase
//
// Records the duration of individual phases during a flush operation:
//   - cache_read: Time spent reading from write cache
//   - s3_upload: Time spent uploading to S3 (PutObject or multipart)
//   - cache_clear: Time spent clearing cache after successful upload
func (m *s3Metrics) ObserveFlushPhase(phase string, duration time.Duration, bytes int64) {
	m.flushPhaseDuration.WithLabelValues(phase).Observe(duration.Seconds())
	// Also record bytes for upload phase
	if phase == "s3_upload" && bytes > 0 {
		m.bytesTransferred.WithLabelValues("flush_upload").Add(float64(bytes))
	}
}

// RecordFlushOperation implements s3.S3Metrics.RecordFlushOperation
//
// Records a complete flush operation with:
//   - reason: Why the flush was triggered (stable_write, commit, timeout, threshold)
//   - bytes: Total bytes flushed
//   - duration: Total flush duration
//   - err: Error if flush failed
func (m *s3Metrics) RecordFlushOperation(reason string, bytes int64, duration time.Duration, err error) {
	status := "success"
	if err != nil {
		status = "error"
	}

	m.flushOperations.WithLabelValues(reason, status).Inc()
	m.flushBytes.WithLabelValues(reason).Observe(float64(bytes))

	// Record overall flush duration as an operation
	m.operationDuration.WithLabelValues("flush").Observe(duration.Seconds())
}
