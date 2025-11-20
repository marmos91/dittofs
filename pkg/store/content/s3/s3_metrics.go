// Package s3 implements S3-based content storage for DittoFS.
//
// This file contains metrics-related types and implementations for observability
// of S3 operations.
package s3

import (
	"io"
	"time"
)

// S3Metrics provides observability for S3 operations.
//
// Implementations can use this interface to collect metrics about S3 operations,
// latency, throughput, and errors. This is optional - if not provided, metrics
// collection is skipped.
//
// Example implementations:
//   - Prometheus metrics
//   - StatsD metrics
//   - CloudWatch metrics
//   - In-memory counters for testing
type S3Metrics interface {
	// ObserveOperation records an S3 operation with its duration and outcome
	ObserveOperation(operation string, duration time.Duration, err error)

	// RecordBytes records bytes transferred for read/write operations
	RecordBytes(operation string, bytes int64)

	// ObserveFlushPhase records duration of individual flush phases
	// phase can be: "cache_read", "s3_upload", "cache_clear"
	ObserveFlushPhase(phase string, duration time.Duration, bytes int64)

	// RecordFlushOperation records a complete flush operation
	// reason can be: "stable_write", "commit", "timeout", "threshold"
	RecordFlushOperation(reason string, bytes int64, duration time.Duration, err error)
}

// noopMetrics is a default no-op metrics implementation
type noopMetrics struct{}

func (noopMetrics) ObserveOperation(operation string, duration time.Duration, err error) {}
func (noopMetrics) RecordBytes(operation string, bytes int64)                            {}
func (noopMetrics) ObserveFlushPhase(phase string, duration time.Duration, bytes int64)  {}
func (noopMetrics) RecordFlushOperation(reason string, bytes int64, duration time.Duration, err error) {
}

// metricsReadCloser wraps an io.ReadCloser to track bytes read
type metricsReadCloser struct {
	io.ReadCloser
	metrics   S3Metrics
	operation string
	bytesRead int64
}

func (m *metricsReadCloser) Read(p []byte) (n int, err error) {
	n, err = m.ReadCloser.Read(p)
	if n > 0 {
		m.bytesRead += int64(n)
	}
	return n, err
}

func (m *metricsReadCloser) Close() error {
	err := m.ReadCloser.Close()
	// Record bytes read regardless of close error
	if m.bytesRead > 0 {
		m.metrics.RecordBytes(m.operation, m.bytesRead)
	}
	return err
}
