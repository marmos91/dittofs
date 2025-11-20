package metrics

import (
	"time"

	"github.com/marmos91/dittofs/pkg/store/content/cache"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// cacheMetrics is the Prometheus implementation of cache.CacheMetrics interface.
//
// This implementation collects metrics about write cache operations including:
//   - Write/read operation counts and latencies
//   - Cache size and utilization
//   - Buffer counts
//   - Throughput measurements
type cacheMetrics struct {
	writeOperations *prometheus.CounterVec
	writeDuration   prometheus.Histogram
	writeBytes      prometheus.Counter
	readOperations  *prometheus.CounterVec
	readDuration    prometheus.Histogram
	readBytes       prometheus.Counter
	cacheSize       *prometheus.GaugeVec
	bufferCount     prometheus.Gauge
	resetOperations prometheus.Counter
}

// NewCacheMetrics creates a new Prometheus-backed CacheMetrics instance.
//
// Returns nil if metrics are not enabled (InitRegistry not called), which
// causes the cache to use the built-in no-op implementation.
//
// This implements the cache.CacheMetrics interface from pkg/content/cache/cache_metrics.go.
func NewCacheMetrics() cache.CacheMetrics {
	if !IsEnabled() {
		return nil // Cache will use noopCacheMetrics
	}

	reg := GetRegistry()

	return &cacheMetrics{
		writeOperations: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_cache_write_operations_total",
				Help: "Total number of cache write operations",
			},
			[]string{"status"},
		),
		writeDuration: promauto.With(reg).NewHistogram(
			prometheus.HistogramOpts{
				Name: "dittofs_cache_write_duration_seconds",
				Help: "Duration of cache write operations in seconds",
				Buckets: []float64{
					0.00001, // 10µs
					0.00005, // 50µs
					0.0001,  // 100µs
					0.0005,  // 500µs
					0.001,   // 1ms
					0.005,   // 5ms
					0.01,    // 10ms
					0.05,    // 50ms
					0.1,     // 100ms
					0.5,     // 500ms
				},
			},
		),
		writeBytes: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Name: "dittofs_cache_write_bytes_total",
				Help: "Total bytes written to cache",
			},
		),
		readOperations: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_cache_read_operations_total",
				Help: "Total number of cache read operations",
			},
			[]string{"status"},
		),
		readDuration: promauto.With(reg).NewHistogram(
			prometheus.HistogramOpts{
				Name: "dittofs_cache_read_duration_seconds",
				Help: "Duration of cache read operations in seconds",
				Buckets: []float64{
					0.00001, // 10µs
					0.00005, // 50µs
					0.0001,  // 100µs
					0.0005,  // 500µs
					0.001,   // 1ms
					0.005,   // 5ms
					0.01,    // 10ms
					0.05,    // 50ms
					0.1,     // 100ms
					0.5,     // 500ms
				},
			},
		),
		readBytes: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Name: "dittofs_cache_read_bytes_total",
				Help: "Total bytes read from cache",
			},
		),
		cacheSize: promauto.With(reg).NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "dittofs_cache_size_bytes",
				Help: "Current size of cached content in bytes per content ID",
			},
			[]string{"content_id"},
		),
		bufferCount: promauto.With(reg).NewGauge(
			prometheus.GaugeOpts{
				Name: "dittofs_cache_buffer_count",
				Help: "Current number of active cache buffers",
			},
		),
		resetOperations: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Name: "dittofs_cache_reset_operations_total",
				Help: "Total number of cache reset operations",
			},
		),
	}
}

// ObserveWrite implements cache.CacheMetrics.ObserveWrite
func (m *cacheMetrics) ObserveWrite(bytes int64, duration time.Duration) {
	m.writeOperations.WithLabelValues("success").Inc()
	m.writeDuration.Observe(duration.Seconds())
	m.writeBytes.Add(float64(bytes))
}

// ObserveRead implements cache.CacheMetrics.ObserveRead
func (m *cacheMetrics) ObserveRead(bytes int64, duration time.Duration) {
	m.readOperations.WithLabelValues("success").Inc()
	m.readDuration.Observe(duration.Seconds())
	m.readBytes.Add(float64(bytes))
}

// RecordCacheSize implements cache.CacheMetrics.RecordCacheSize
func (m *cacheMetrics) RecordCacheSize(contentID string, bytes int64) {
	if bytes == 0 {
		// Remove gauge when cache is cleared
		m.cacheSize.DeleteLabelValues(contentID)
	} else {
		m.cacheSize.WithLabelValues(contentID).Set(float64(bytes))
	}
}

// RecordCacheReset implements cache.CacheMetrics.RecordCacheReset
func (m *cacheMetrics) RecordCacheReset(contentID string) {
	m.resetOperations.Inc()
	m.cacheSize.DeleteLabelValues(contentID)
}

// RecordBufferCount implements cache.CacheMetrics.RecordBufferCount
func (m *cacheMetrics) RecordBufferCount(count int) {
	m.bufferCount.Set(float64(count))
}
