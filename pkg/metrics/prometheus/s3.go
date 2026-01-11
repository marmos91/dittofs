package prometheus

import (
	"time"

	"github.com/marmos91/dittofs/pkg/content/store/s3"
	"github.com/marmos91/dittofs/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// s3Metrics is the Prometheus implementation of s3.S3Metrics.
type s3Metrics struct {
	operationsTotal       *prometheus.CounterVec
	operationDuration     *prometheus.HistogramVec
	bytesTransferred      *prometheus.CounterVec
	flushPhaseDuration    *prometheus.HistogramVec
	flushOperations       *prometheus.CounterVec
	flushDuration         *prometheus.HistogramVec
	flushBytes            *prometheus.HistogramVec
	activeUploads         *prometheus.GaugeVec
	multipartPartNumber   prometheus.Histogram
	orphanedUploads       prometheus.Counter
	multipartAbortedTotal prometheus.Counter
}

// NewS3Metrics creates a new Prometheus-backed S3Metrics instance.
//
// Returns nil if metrics are not enabled (InitRegistry not called).
func NewS3Metrics() s3.S3Metrics {
	if !metrics.IsEnabled() {
		return nil
	}

	reg := metrics.GetRegistry()

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
				Name: "dittofs_s3_operation_duration_milliseconds",
				Help: "Duration of S3 operations in milliseconds",
				Buckets: []float64{
					10,    // 10ms - fast metadata operations
					50,    // 50ms - small object operations
					100,   // 100ms
					500,   // 500ms
					1000,  // 1s - medium objects
					5000,  // 5s - large objects
					10000, // 10s - multipart uploads
					30000, // 30s - very large operations
				},
			},
			[]string{"operation"},
		),
		bytesTransferred: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_s3_bytes_transferred_total",
				Help: "Total bytes transferred via S3 operations",
			},
			[]string{"operation", "direction"},
		),
		flushPhaseDuration: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "dittofs_s3_flush_phase_duration_milliseconds",
				Help: "Duration of individual flush phases in milliseconds",
				Buckets: []float64{
					1,    // 1ms - cache reads
					10,   // 10ms
					50,   // 50ms
					100,  // 100ms
					500,  // 500ms - small uploads
					1000, // 1s
					5000, // 5s - large uploads
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
		flushDuration: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "dittofs_s3_flush_duration_milliseconds",
				Help: "Total duration of flush operations in milliseconds",
				Buckets: []float64{
					10,    // 10ms
					50,    // 50ms
					100,   // 100ms
					500,   // 500ms
					1000,  // 1s
					5000,  // 5s
					10000, // 10s
					30000, // 30s
				},
			},
			[]string{"reason"},
		),
		flushBytes: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "dittofs_s3_flush_bytes",
				Help: "Distribution of bytes flushed per operation",
				Buckets: []float64{
					4096,      // 4KB
					65536,     // 64KB
					1048576,   // 1MB
					5242880,   // 5MB - multipart threshold
					10485760,  // 10MB
					52428800,  // 50MB
					104857600, // 100MB
				},
			},
			[]string{"reason"},
		),
		activeUploads: promauto.With(reg).NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "dittofs_s3_active_uploads",
				Help: "Current number of active multipart uploads",
			},
			[]string{"store"},
		),
		multipartPartNumber: promauto.With(reg).NewHistogram(
			prometheus.HistogramOpts{
				Name: "dittofs_s3_multipart_part_number",
				Help: "Distribution of multipart part numbers (indicates file size distribution)",
				Buckets: []float64{
					1,   // Single part (<=5MB)
					2,   // ~10MB
					5,   // ~25MB
					10,  // ~50MB
					20,  // ~100MB
					50,  // ~250MB
					100, // ~500MB
					200, // ~1GB
				},
			},
		),
		orphanedUploads: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Name: "dittofs_s3_multipart_orphaned_total",
				Help: "Total number of orphaned multipart uploads detected and cleaned up",
			},
		),
		multipartAbortedTotal: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Name: "dittofs_s3_multipart_aborted_total",
				Help: "Total number of multipart uploads that were aborted due to errors",
			},
		),
	}
}

func (m *s3Metrics) ObserveOperation(operation string, duration time.Duration, err error) {
	if m == nil {
		return
	}

	status := "success"
	if err != nil {
		status = "error"
	}

	m.operationsTotal.WithLabelValues(operation, status).Inc()
	m.operationDuration.WithLabelValues(operation).Observe(duration.Seconds() * 1000)
}

func (m *s3Metrics) RecordBytes(operation string, bytes int64) {
	if m == nil || bytes <= 0 {
		return
	}

	// Determine direction based on operation
	direction := "write"
	if operation == "read" || operation == "GetObject" || operation == "DownloadPart" {
		direction = "read"
	}

	m.bytesTransferred.WithLabelValues(operation, direction).Add(float64(bytes))
}

func (m *s3Metrics) ObserveFlushPhase(phase string, duration time.Duration, bytes int64) {
	if m == nil {
		return
	}

	m.flushPhaseDuration.WithLabelValues(phase).Observe(duration.Seconds() * 1000)

	// Also record bytes if this is an upload phase
	if phase == "s3_upload" && bytes > 0 {
		m.bytesTransferred.WithLabelValues("flush", "write").Add(float64(bytes))
	}
}

func (m *s3Metrics) RecordFlushOperation(reason string, bytes int64, duration time.Duration, err error) {
	if m == nil {
		return
	}

	status := "success"
	if err != nil {
		status = "error"
	}

	m.flushOperations.WithLabelValues(reason, status).Inc()
	m.flushDuration.WithLabelValues(reason).Observe(duration.Seconds() * 1000)

	if bytes > 0 {
		m.flushBytes.WithLabelValues(reason).Observe(float64(bytes))
	}
}

// RecordActiveUpload tracks active multipart uploads.
// This is an extension method not in the interface for internal use.
func (m *s3Metrics) RecordActiveUpload(store string, delta int) {
	if m == nil {
		return
	}
	m.activeUploads.WithLabelValues(store).Add(float64(delta))
}

// RecordMultipartPartNumber records the part number being uploaded.
// This helps understand file size distributions.
func (m *s3Metrics) RecordMultipartPartNumber(partNumber int) {
	if m == nil {
		return
	}
	m.multipartPartNumber.Observe(float64(partNumber))
}

// RecordOrphanedUpload records an orphaned multipart upload that was cleaned up.
func (m *s3Metrics) RecordOrphanedUpload() {
	if m == nil {
		return
	}
	m.orphanedUploads.Inc()
}

// RecordAbortedUpload records a multipart upload that was aborted due to an error.
func (m *s3Metrics) RecordAbortedUpload() {
	if m == nil {
		return
	}
	m.multipartAbortedTotal.Inc()
}
