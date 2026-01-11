package metrics

import (
	"time"

	"github.com/marmos91/dittofs/pkg/content/store/s3"
)

// NewS3Metrics creates a new Prometheus-backed S3Metrics instance.
//
// Returns nil if metrics are not enabled (InitRegistry not called).
// When nil is returned, callers should pass nil to S3 content stores,
// which results in zero overhead.
//
// Example usage:
//
//	// With metrics enabled
//	metrics.InitRegistry()
//	s3Metrics := metrics.NewS3Metrics()
//	store := s3.New(config, s3Metrics)
//
//	// Without metrics (zero overhead)
//	store := s3.New(config, nil)
func NewS3Metrics() s3.S3Metrics {
	if !IsEnabled() {
		return nil
	}

	// Import prometheus package to access implementation
	// This breaks the import cycle by using interface return type
	return newPrometheusS3Metrics()
}

// newPrometheusS3Metrics is implemented in pkg/metrics/prometheus/s3.go
// This indirection avoids import cycles while keeping the API clean
var newPrometheusS3Metrics func() s3.S3Metrics

// RegisterS3MetricsConstructor registers the Prometheus S3 metrics constructor.
// Called by pkg/metrics/prometheus/s3.go during package initialization.
func RegisterS3MetricsConstructor(constructor func() s3.S3Metrics) {
	newPrometheusS3Metrics = constructor
}

// S3MetricsAdapter adapts the s3.S3Metrics interface for external use.
// This type is provided for documentation and testing purposes.
type S3MetricsAdapter interface {
	s3.S3Metrics
}

// ObserveOperation records an S3 operation with its duration and outcome.
//
// Parameters:
//   - operation: S3 operation name (e.g., "PutObject", "GetObject", "CreateMultipartUpload")
//   - duration: Time taken to perform the operation
//   - err: Error if operation failed, nil if successful
//
// Example usage:
//
//	start := time.Now()
//	err := s3Client.PutObject(ctx, key, data)
//	metrics.ObserveOperation("PutObject", time.Since(start), err)
func ObserveOperation(m s3.S3Metrics, operation string, duration time.Duration, err error) {
	if m != nil {
		m.ObserveOperation(operation, duration, err)
	}
}

// RecordBytes records bytes transferred for read/write operations.
//
// Parameters:
//   - operation: Operation type (e.g., "read", "write", "upload_part")
//   - bytes: Number of bytes transferred
//
// Example usage:
//
//	n, err := reader.Read(buf)
//	if n > 0 {
//		metrics.RecordBytes("read", int64(n))
//	}
func RecordBytes(m s3.S3Metrics, operation string, bytes int64) {
	if m != nil {
		m.RecordBytes(operation, bytes)
	}
}

// ObserveFlushPhase records duration of individual flush phases.
//
// Parameters:
//   - phase: Flush phase name (e.g., "cache_read", "s3_upload", "cache_clear")
//   - duration: Time taken for this phase
//   - bytes: Number of bytes processed in this phase
//
// Example usage:
//
//	start := time.Now()
//	data, err := cache.Read(contentID)
//	metrics.ObserveFlushPhase("cache_read", time.Since(start), int64(len(data)))
func ObserveFlushPhase(m s3.S3Metrics, phase string, duration time.Duration, bytes int64) {
	if m != nil {
		m.ObserveFlushPhase(phase, duration, bytes)
	}
}

// RecordFlushOperation records a complete flush operation.
//
// Parameters:
//   - reason: Flush reason (e.g., "stable_write", "commit", "timeout", "threshold")
//   - bytes: Total bytes flushed
//   - duration: Total time taken for flush
//   - err: Error if flush failed, nil if successful
//
// Example usage:
//
//	start := time.Now()
//	err := store.FlushIncremental(ctx, contentID)
//	metrics.RecordFlushOperation("commit", bytesWritten, time.Since(start), err)
func RecordFlushOperation(m s3.S3Metrics, reason string, bytes int64, duration time.Duration, err error) {
	if m != nil {
		m.RecordFlushOperation(reason, bytes, duration, err)
	}
}
