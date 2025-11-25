package config

import (
	"github.com/marmos91/dittofs/pkg/metrics"
	promMetrics "github.com/marmos91/dittofs/pkg/metrics/prometheus"
)

// MetricsResult contains all metrics-related components created from configuration.
type MetricsResult struct {
	// Server is the HTTP server exposing Prometheus metrics (nil if disabled)
	Server *metrics.Server

	// NFSMetrics is the metrics collector for NFS adapter (never nil, uses noop if disabled)
	NFSMetrics metrics.NFSMetrics
}

// InitializeMetrics creates and initializes all metrics components based on configuration.
//
// If metrics are enabled in the configuration:
//   - Initializes the global Prometheus registry
//   - Creates the metrics HTTP server
//   - Creates Prometheus-backed metrics instances for all components
//
// If metrics are disabled:
//   - Returns nil server
//   - Returns no-op metrics implementations (zero overhead)
//
// Parameters:
//   - cfg: The complete DittoFS configuration
//
// Returns:
//   - MetricsResult containing all metrics components
func InitializeMetrics(cfg *Config) *MetricsResult {
	if !cfg.Server.Metrics.Enabled {
		// Metrics disabled - return nil for zero overhead
		return &MetricsResult{
			Server:     nil,
			NFSMetrics: nil,
		}
	}

	// Initialize global Prometheus registry
	metrics.InitRegistry()

	// Create metrics HTTP server
	server := metrics.NewServer(metrics.ServerConfig{
		Port: cfg.Server.Metrics.Port,
	})

	// Create Prometheus-backed metrics for NFS adapter
	nfsMetrics := promMetrics.NewNFSMetrics()

	return &MetricsResult{
		Server:     server,
		NFSMetrics: nfsMetrics,
	}
}
