// Package cache implements write caching for content stores.
//
// This file contains metrics-related types and implementations for observability
// of cache operations.
package cache

import (
	"time"
)

// CacheMetrics provides observability for write cache operations.
//
// Implementations can use this interface to collect metrics about cache operations,
// latency, throughput, and utilization. This is optional - if not provided, metrics
// collection is skipped.
//
// Example implementations:
//   - Prometheus metrics
//   - StatsD metrics
//   - In-memory counters for testing
type CacheMetrics interface {
	// ObserveWrite records a cache write operation
	ObserveWrite(bytes int64, duration time.Duration)

	// ObserveRead records a cache read operation
	ObserveRead(bytes int64, duration time.Duration)

	// RecordCacheSize records current cache size in bytes
	RecordCacheSize(contentID string, bytes int64)

	// RecordCacheReset records a cache reset operation
	RecordCacheReset(contentID string)

	// RecordBufferCount records the total number of active buffers
	RecordBufferCount(count int)
}

// noopCacheMetrics is a default no-op metrics implementation
type noopCacheMetrics struct{}

func (noopCacheMetrics) ObserveWrite(bytes int64, duration time.Duration) {}
func (noopCacheMetrics) ObserveRead(bytes int64, duration time.Duration)  {}
func (noopCacheMetrics) RecordCacheSize(contentID string, bytes int64)    {}
func (noopCacheMetrics) RecordCacheReset(contentID string)                {}
func (noopCacheMetrics) RecordBufferCount(count int)                      {}
