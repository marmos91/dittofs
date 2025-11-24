package metrics

import (
	"time"

	"github.com/marmos91/dittofs/pkg/cache"
)

// NewCacheMetrics creates a new Prometheus-backed CacheMetrics instance.
//
// Returns nil if metrics are not enabled (InitRegistry not called).
// When nil is returned, callers should pass nil to cache implementations,
// which results in zero overhead.
//
// Example usage:
//
//	// With metrics enabled
//	metrics.InitRegistry()
//	cacheMetrics := metrics.NewCacheMetrics()
//	writeCache := cache.NewWriteCache(config, cacheMetrics)
//
//	// Without metrics (zero overhead)
//	writeCache := cache.NewWriteCache(config, nil)
func NewCacheMetrics() cache.CacheMetrics {
	if !IsEnabled() {
		return nil
	}

	// Import prometheus package to access implementation
	// This breaks the import cycle by using interface return type
	return newPrometheusCacheMetrics()
}

// newPrometheusCacheMetrics is implemented in pkg/metrics/prometheus/cache.go
// This indirection avoids import cycles while keeping the API clean
var newPrometheusCacheMetrics func() cache.CacheMetrics

// RegisterCacheMetricsConstructor registers the Prometheus cache metrics constructor.
// Called by pkg/metrics/prometheus/cache.go during package initialization.
func RegisterCacheMetricsConstructor(constructor func() cache.CacheMetrics) {
	newPrometheusCacheMetrics = constructor
}

// CacheMetricsAdapter adapts the cache.CacheMetrics interface for external use.
// This type is provided for documentation and testing purposes.
type CacheMetricsAdapter interface {
	cache.CacheMetrics
}

// ObserveWrite records a cache write operation.
//
// Parameters:
//   - bytes: Number of bytes written to cache
//   - duration: Time taken to write
//
// Example usage:
//
//	start := time.Now()
//	err := cache.Write(contentID, data)
//	metrics.ObserveWrite(int64(len(data)), time.Since(start))
func ObserveWrite(m cache.CacheMetrics, bytes int64, duration time.Duration) {
	if m != nil {
		m.ObserveWrite(bytes, duration)
	}
}

// ObserveRead records a cache read operation.
//
// Parameters:
//   - bytes: Number of bytes read from cache
//   - duration: Time taken to read
//
// Example usage:
//
//	start := time.Now()
//	data, err := cache.Read(contentID)
//	if err == nil {
//		metrics.ObserveRead(int64(len(data)), time.Since(start))
//	}
func ObserveRead(m cache.CacheMetrics, bytes int64, duration time.Duration) {
	if m != nil {
		m.ObserveRead(bytes, duration)
	}
}

// RecordCacheSize records current cache size in bytes.
//
// Parameters:
//   - contentID: Content identifier (for per-file tracking)
//   - bytes: Current size in bytes
//
// Example usage:
//
//	metrics.RecordCacheSize(contentID, int64(len(buffer)))
func RecordCacheSize(m cache.CacheMetrics, contentID string, bytes int64) {
	if m != nil {
		m.RecordCacheSize(contentID, bytes)
	}
}

// RecordCacheReset records a cache reset operation.
//
// Parameters:
//   - contentID: Content identifier that was reset
//
// Example usage:
//
//	cache.Remove(contentID)
//	metrics.RecordCacheReset(contentID)
func RecordCacheReset(m cache.CacheMetrics, contentID string) {
	if m != nil {
		m.RecordCacheReset(contentID)
	}
}

// RecordBufferCount records the total number of active buffers.
//
// Parameters:
//   - count: Number of active buffers
//
// Example usage:
//
//	metrics.RecordBufferCount(cache.ActiveBufferCount())
func RecordBufferCount(m cache.CacheMetrics, count int) {
	if m != nil {
		m.RecordBufferCount(count)
	}
}
