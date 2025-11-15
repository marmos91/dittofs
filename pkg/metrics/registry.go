// Package metrics provides Prometheus metrics collection for DittoFS components.
//
// All metrics are optional - if not initialized, components use no-op implementations
// that have zero overhead. This allows DittoFS to run with or without metrics
// collection enabled.
//
// Usage:
//
//	// Initialize global registry (typically in main.go)
//	metrics.InitRegistry()
//
//	// Create metrics instances for components
//	nfsMetrics := metrics.NewNFSMetrics()
//	s3Metrics := metrics.NewS3Metrics()
//
//	// Or use nil for no-op behavior
//	adapter := nfs.New(config, nil) // No metrics
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// registry is the global Prometheus registry for all DittoFS metrics
	registry     *prometheus.Registry
	registryOnce sync.Once
	registryMu   sync.RWMutex
)

// InitRegistry initializes the global Prometheus registry.
//
// This must be called before creating any metrics instances. It's safe to call
// multiple times - subsequent calls are ignored.
//
// If not called, GetRegistry() will return nil and all metrics constructors
// will return no-op implementations.
func InitRegistry() {
	registryOnce.Do(func() {
		registry = prometheus.NewRegistry()
	})
}

// GetRegistry returns the global Prometheus registry.
//
// Returns nil if InitRegistry() has not been called, indicating metrics
// are disabled.
func GetRegistry() *prometheus.Registry {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry
}

// IsEnabled returns true if metrics collection is enabled.
//
// Metrics are enabled if InitRegistry() has been called.
func IsEnabled() bool {
	return GetRegistry() != nil
}
