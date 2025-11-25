package prometheus

import (
	"time"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// cacheMetrics is the Prometheus implementation of cache.CacheMetrics.
type cacheMetrics struct {
	writeOperations *prometheus.CounterVec
	writeDuration   *prometheus.HistogramVec
	writeBytes      *prometheus.HistogramVec
	readOperations  *prometheus.CounterVec
	readDuration    *prometheus.HistogramVec
	readBytes       *prometheus.HistogramVec
	cacheSize       *prometheus.GaugeVec
	totalCacheSize  prometheus.Gauge
	resetOperations prometheus.Counter
	activeBuffers   prometheus.Gauge
	hitRate         *prometheus.GaugeVec
	evictions       *prometheus.CounterVec
}

// NewCacheMetrics creates a new Prometheus-backed CacheMetrics instance.
//
// Returns nil if metrics are not enabled (InitRegistry not called).
func NewCacheMetrics() cache.CacheMetrics {
	if !metrics.IsEnabled() {
		return nil
	}

	reg := metrics.GetRegistry()

	return &cacheMetrics{
		writeOperations: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_cache_write_operations_total",
				Help: "Total number of cache write operations by cache type",
			},
			[]string{"cache_type"}, // "write", "read"
		),
		writeDuration: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "dittofs_cache_write_duration_milliseconds",
				Help: "Duration of cache write operations in milliseconds",
				Buckets: []float64{
					0.1,  // 100us - small writes
					0.5,  // 500us
					1,    // 1ms
					5,    // 5ms
					10,   // 10ms
					50,   // 50ms
					100,  // 100ms
					500,  // 500ms - large writes
					1000, // 1s
				},
			},
			[]string{"cache_type"},
		),
		writeBytes: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "dittofs_cache_write_bytes",
				Help: "Distribution of bytes written to cache",
				Buckets: []float64{
					4096,     // 4KB - small writes
					32768,    // 32KB
					131072,   // 128KB
					524288,   // 512KB
					1048576,  // 1MB
					4194304,  // 4MB - typical NFS write size
					10485760, // 10MB
				},
			},
			[]string{"cache_type"},
		),
		readOperations: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_cache_read_operations_total",
				Help: "Total number of cache read operations by cache type and status",
			},
			[]string{"cache_type", "status"}, // status: "hit", "miss"
		),
		readDuration: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "dittofs_cache_read_duration_milliseconds",
				Help: "Duration of cache read operations in milliseconds",
				Buckets: []float64{
					0.1,  // 100us - cache hits
					0.5,  // 500us
					1,    // 1ms
					5,    // 5ms
					10,   // 10ms
					50,   // 50ms
					100,  // 100ms
					500,  // 500ms
					1000, // 1s
				},
			},
			[]string{"cache_type"},
		),
		readBytes: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "dittofs_cache_read_bytes",
				Help: "Distribution of bytes read from cache",
				Buckets: []float64{
					4096,     // 4KB
					32768,    // 32KB
					131072,   // 128KB
					524288,   // 512KB
					1048576,  // 1MB
					4194304,  // 4MB
					10485760, // 10MB
				},
			},
			[]string{"cache_type"},
		),
		cacheSize: promauto.With(reg).NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "dittofs_cache_size_bytes",
				Help: "Current cache size in bytes per content ID",
			},
			[]string{"cache_type", "content_id"},
		),
		totalCacheSize: promauto.With(reg).NewGauge(
			prometheus.GaugeOpts{
				Name: "dittofs_cache_total_size_bytes",
				Help: "Total cache size across all content IDs",
			},
		),
		resetOperations: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Name: "dittofs_cache_reset_operations_total",
				Help: "Total number of cache reset operations",
			},
		),
		activeBuffers: promauto.With(reg).NewGauge(
			prometheus.GaugeOpts{
				Name: "dittofs_cache_active_buffers",
				Help: "Current number of active cache buffers",
			},
		),
		hitRate: promauto.With(reg).NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "dittofs_cache_hit_rate",
				Help: "Cache hit rate (hits / total reads) per cache type",
			},
			[]string{"cache_type"},
		),
		evictions: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_cache_evictions_total",
				Help: "Total number of cache evictions by cache type and reason",
			},
			[]string{"cache_type", "reason"}, // reason: "size_limit", "timeout", "explicit"
		),
	}
}

func (m *cacheMetrics) ObserveWrite(bytes int64, duration time.Duration) {
	if m == nil {
		return
	}

	cacheType := "write" // Default to write cache
	m.writeOperations.WithLabelValues(cacheType).Inc()
	m.writeDuration.WithLabelValues(cacheType).Observe(duration.Seconds() * 1000)

	if bytes > 0 {
		m.writeBytes.WithLabelValues(cacheType).Observe(float64(bytes))
	}
}

func (m *cacheMetrics) ObserveRead(bytes int64, duration time.Duration) {
	if m == nil {
		return
	}

	cacheType := "write" // Default to write cache
	status := "hit"

	m.readOperations.WithLabelValues(cacheType, status).Inc()
	m.readDuration.WithLabelValues(cacheType).Observe(duration.Seconds() * 1000)

	if bytes > 0 {
		m.readBytes.WithLabelValues(cacheType).Observe(float64(bytes))
	}
}

func (m *cacheMetrics) RecordCacheSize(contentID string, bytes int64) {
	if m == nil {
		return
	}

	cacheType := "write" // Default to write cache
	m.cacheSize.WithLabelValues(cacheType, contentID).Set(float64(bytes))
}

func (m *cacheMetrics) RecordCacheReset(contentID string) {
	if m == nil {
		return
	}

	cacheType := "write"
	m.resetOperations.Inc()
	m.cacheSize.WithLabelValues(cacheType, contentID).Set(0)
}

func (m *cacheMetrics) RecordBufferCount(count int) {
	if m == nil {
		return
	}

	m.activeBuffers.Set(float64(count))
}

// RecordCacheMiss records a cache miss.
// This is an extension method not in the interface for internal use.
func (m *cacheMetrics) RecordCacheMiss(cacheType string) {
	if m == nil {
		return
	}
	m.readOperations.WithLabelValues(cacheType, "miss").Inc()
}

// UpdateHitRate calculates and updates the cache hit rate.
// This should be called periodically to maintain accurate hit rate metrics.
func (m *cacheMetrics) UpdateHitRate(cacheType string, hits, total float64) {
	if m == nil || total == 0 {
		return
	}
	rate := hits / total
	m.hitRate.WithLabelValues(cacheType).Set(rate)
}

// RecordTotalCacheSize records the total cache size across all content IDs.
func (m *cacheMetrics) RecordTotalCacheSize(bytes int64) {
	if m == nil {
		return
	}
	m.totalCacheSize.Set(float64(bytes))
}

// RecordEviction records a cache eviction.
// reason can be: "size_limit", "timeout", "explicit"
func (m *cacheMetrics) RecordEviction(cacheType, reason string) {
	if m == nil {
		return
	}
	m.evictions.WithLabelValues(cacheType, reason).Inc()
}
