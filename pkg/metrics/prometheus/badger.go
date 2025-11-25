package prometheus

import (
	"github.com/marmos91/dittofs/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// badgerMetrics is the Prometheus implementation for BadgerDB metrics.
type badgerMetrics struct {
	cacheHitRatio *prometheus.GaugeVec
	cacheMisses   *prometheus.CounterVec
	cacheHits     *prometheus.CounterVec
}

// NewBadgerMetrics creates a new Prometheus-backed BadgerDB metrics instance.
//
// Returns nil if metrics are not enabled (InitRegistry not called).
func NewBadgerMetrics() *badgerMetrics {
	if !metrics.IsEnabled() {
		return nil
	}

	reg := metrics.GetRegistry()

	return &badgerMetrics{
		cacheHitRatio: promauto.With(reg).NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "dittofs_badger_cache_hit_ratio",
				Help: "BadgerDB cache hit ratio (0.0 to 1.0) by cache type",
			},
			[]string{"cache_type"}, // "block", "index"
		),
		cacheMisses: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_badger_cache_misses_total",
				Help: "Total number of BadgerDB cache misses by cache type",
			},
			[]string{"cache_type"}, // "block", "index"
		),
		cacheHits: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_badger_cache_hits_total",
				Help: "Total number of BadgerDB cache hits by cache type",
			},
			[]string{"cache_type"}, // "block", "index"
		),
	}
}

// RecordCacheHitRatio records the cache hit ratio for a specific cache type.
// ratio should be between 0.0 and 1.0
func (m *badgerMetrics) RecordCacheHitRatio(cacheType string, ratio float64) {
	if m == nil {
		return
	}
	m.cacheHitRatio.WithLabelValues(cacheType).Set(ratio)
}

// RecordCacheMiss records a cache miss for a specific cache type.
func (m *badgerMetrics) RecordCacheMiss(cacheType string) {
	if m == nil {
		return
	}
	m.cacheMisses.WithLabelValues(cacheType).Inc()
}

// RecordCacheHit records a cache hit for a specific cache type.
func (m *badgerMetrics) RecordCacheHit(cacheType string) {
	if m == nil {
		return
	}
	m.cacheHits.WithLabelValues(cacheType).Inc()
}
