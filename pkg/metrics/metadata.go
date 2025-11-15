package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// MetadataMetrics provides observability for metadata store operations.
//
// Implementations can collect metrics about metadata operations including
// lookups, file creation, directory operations, cache performance, and
// resource utilization.
//
// This interface is optional - if not provided to metadata stores, operations
// proceed without metrics collection (zero overhead).
//
// Example usage:
//
//	// With metrics enabled
//	metrics := metrics.NewMetadataMetrics("badger")
//	store := badger.New(config, metrics)
//
//	// Without metrics (no-op)
//	store := badger.New(config, nil)
type MetadataMetrics interface {
	// RecordOperation records a completed metadata operation with its name,
	// duration, and outcome.
	//
	// Parameters:
	//   - operation: Operation name (e.g., "Lookup", "CreateFile", "ReadDir")
	//   - duration: Time taken to complete the operation
	//   - err: Error if operation failed, nil if successful
	RecordOperation(operation string, duration time.Duration, err error)

	// RecordCacheHit records a cache hit.
	//
	// Parameters:
	//   - cacheType: Type of cache (e.g., "file_handle", "path_lookup")
	RecordCacheHit(cacheType string)

	// RecordCacheMiss records a cache miss.
	//
	// Parameters:
	//   - cacheType: Type of cache (e.g., "file_handle", "path_lookup")
	RecordCacheMiss(cacheType string)

	// SetActiveHandles updates the count of active file handles.
	//
	// Parameters:
	//   - count: Current number of active file handles
	SetActiveHandles(count int64)

	// SetActiveMounts updates the count of active mounts.
	//
	// Parameters:
	//   - count: Current number of active mounts
	SetActiveMounts(count int64)

	// RecordStorageOperation records a low-level storage operation.
	// This is useful for database-backed stores (BadgerDB, etc.)
	//
	// Parameters:
	//   - operation: Storage operation (e.g., "get", "set", "delete", "scan")
	//   - duration: Time taken
	//   - err: Error if failed
	RecordStorageOperation(operation string, duration time.Duration, err error)
}

// metadataMetrics is the Prometheus implementation of MetadataMetrics.
type metadataMetrics struct {
	storeType          string
	operationsTotal    *prometheus.CounterVec
	operationDuration  *prometheus.HistogramVec
	cacheHits          *prometheus.CounterVec
	cacheMisses        *prometheus.CounterVec
	activeHandles      prometheus.Gauge
	activeMounts       prometheus.Gauge
	storageOpsTotal    *prometheus.CounterVec
	storageOpsDuration *prometheus.HistogramVec
}

// NewMetadataMetrics creates a new Prometheus-backed MetadataMetrics instance.
//
// Parameters:
//   - storeType: Type of metadata store (e.g., "memory", "badger")
//     Used as a label to distinguish metrics from different store implementations.
//
// Returns nil if metrics are not enabled (InitRegistry not called).
func NewMetadataMetrics(storeType string) MetadataMetrics {
	if !IsEnabled() {
		return &noopMetadataMetrics{}
	}

	reg := GetRegistry()

	return &metadataMetrics{
		storeType: storeType,
		operationsTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_metadata_operations_total",
				Help: "Total number of metadata operations by store type, operation, and status",
			},
			[]string{"store_type", "operation", "status"},
		),
		operationDuration: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "dittofs_metadata_operation_duration_seconds",
				Help: "Duration of metadata operations in seconds",
				Buckets: []float64{
					0.0001, // 100µs
					0.0005, // 500µs
					0.001,  // 1ms
					0.005,  // 5ms
					0.01,   // 10ms
					0.025,  // 25ms
					0.05,   // 50ms
					0.1,    // 100ms
					0.25,   // 250ms
					0.5,    // 500ms
					1.0,    // 1s
				},
			},
			[]string{"store_type", "operation"},
		),
		cacheHits: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_metadata_cache_hits_total",
				Help: "Total number of metadata cache hits by store type and cache type",
			},
			[]string{"store_type", "cache_type"},
		),
		cacheMisses: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_metadata_cache_misses_total",
				Help: "Total number of metadata cache misses by store type and cache type",
			},
			[]string{"store_type", "cache_type"},
		),
		activeHandles: promauto.With(reg).NewGauge(
			prometheus.GaugeOpts{
				Name: "dittofs_metadata_active_file_handles",
				Help: "Current number of active file handles",
				ConstLabels: prometheus.Labels{
					"store_type": storeType,
				},
			},
		),
		activeMounts: promauto.With(reg).NewGauge(
			prometheus.GaugeOpts{
				Name: "dittofs_metadata_active_mounts",
				Help: "Current number of active mounts",
				ConstLabels: prometheus.Labels{
					"store_type": storeType,
				},
			},
		),
		storageOpsTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "dittofs_metadata_storage_operations_total",
				Help: "Total number of low-level storage operations (get, set, delete, scan)",
			},
			[]string{"store_type", "operation", "status"},
		),
		storageOpsDuration: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "dittofs_metadata_storage_operation_duration_seconds",
				Help: "Duration of low-level storage operations in seconds",
				Buckets: []float64{
					0.0001, // 100µs
					0.0005, // 500µs
					0.001,  // 1ms
					0.005,  // 5ms
					0.01,   // 10ms
					0.025,  // 25ms
					0.05,   // 50ms
					0.1,    // 100ms
				},
			},
			[]string{"store_type", "operation"},
		),
	}
}

func (m *metadataMetrics) RecordOperation(operation string, duration time.Duration, err error) {
	status := "success"
	if err != nil {
		status = "error"
	}

	m.operationsTotal.WithLabelValues(m.storeType, operation, status).Inc()
	m.operationDuration.WithLabelValues(m.storeType, operation).Observe(duration.Seconds())
}

func (m *metadataMetrics) RecordCacheHit(cacheType string) {
	m.cacheHits.WithLabelValues(m.storeType, cacheType).Inc()
}

func (m *metadataMetrics) RecordCacheMiss(cacheType string) {
	m.cacheMisses.WithLabelValues(m.storeType, cacheType).Inc()
}

func (m *metadataMetrics) SetActiveHandles(count int64) {
	m.activeHandles.Set(float64(count))
}

func (m *metadataMetrics) SetActiveMounts(count int64) {
	m.activeMounts.Set(float64(count))
}

func (m *metadataMetrics) RecordStorageOperation(operation string, duration time.Duration, err error) {
	status := "success"
	if err != nil {
		status = "error"
	}

	m.storageOpsTotal.WithLabelValues(m.storeType, operation, status).Inc()
	m.storageOpsDuration.WithLabelValues(m.storeType, operation).Observe(duration.Seconds())
}

// noopMetadataMetrics is a no-op implementation of MetadataMetrics with zero overhead.
type noopMetadataMetrics struct{}

func (noopMetadataMetrics) RecordOperation(operation string, duration time.Duration, err error) {}
func (noopMetadataMetrics) RecordCacheHit(cacheType string)                                     {}
func (noopMetadataMetrics) RecordCacheMiss(cacheType string)                                    {}
func (noopMetadataMetrics) SetActiveHandles(count int64)                                        {}
func (noopMetadataMetrics) SetActiveMounts(count int64)                                         {}
func (noopMetadataMetrics) RecordStorageOperation(operation string, duration time.Duration, err error) {
}
