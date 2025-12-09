package telemetry

import (
	"fmt"
	"runtime"

	"github.com/grafana/pyroscope-go"
)

// ProfilingConfig contains configuration for Pyroscope continuous profiling.
type ProfilingConfig struct {
	// Enabled controls whether profiling is enabled
	Enabled bool

	// ServiceName is the application name shown in Pyroscope
	ServiceName string

	// ServiceVersion is the application version
	ServiceVersion string

	// Endpoint is the Pyroscope server URL (e.g., "http://localhost:4040")
	Endpoint string

	// ProfileTypes specifies which profile types to collect
	// Valid values: cpu, alloc_objects, alloc_space, inuse_objects, inuse_space,
	//               goroutines, mutex_count, mutex_duration, block_count, block_duration
	ProfileTypes []string
}

var (
	// profiler is the global Pyroscope profiler instance
	profiler *pyroscope.Profiler

	// profilingEnabled indicates whether profiling is active
	profilingEnabled bool
)

// InitProfiling initializes Pyroscope continuous profiling.
// Returns a shutdown function that should be called to stop profiling.
func InitProfiling(cfg ProfilingConfig) (shutdown func() error, err error) {
	if !cfg.Enabled {
		profilingEnabled = false
		return func() error { return nil }, nil
	}

	profilingEnabled = true

	// Convert profile type strings to Pyroscope profile types
	profileTypes := make([]pyroscope.ProfileType, 0, len(cfg.ProfileTypes))
	for _, pt := range cfg.ProfileTypes {
		profileType, err := parseProfileType(pt)
		if err != nil {
			return nil, fmt.Errorf("invalid profile type %q: %w", pt, err)
		}
		profileTypes = append(profileTypes, profileType)
	}

	// Enable mutex and block profiling if requested
	for _, pt := range cfg.ProfileTypes {
		switch pt {
		case "mutex_count", "mutex_duration":
			runtime.SetMutexProfileFraction(5)
		case "block_count", "block_duration":
			runtime.SetBlockProfileRate(5)
		}
	}

	// Create Pyroscope profiler
	profiler, err = pyroscope.Start(pyroscope.Config{
		ApplicationName: cfg.ServiceName,
		ServerAddress:   cfg.Endpoint,
		Tags: map[string]string{
			"version": cfg.ServiceVersion,
		},
		ProfileTypes: profileTypes,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to start Pyroscope profiler: %w", err)
	}

	shutdown = func() error {
		if profiler != nil {
			return profiler.Stop()
		}
		return nil
	}

	return shutdown, nil
}

// IsProfilingEnabled returns whether profiling is enabled
func IsProfilingEnabled() bool {
	return profilingEnabled
}

// parseProfileType converts a string profile type to Pyroscope ProfileType.
func parseProfileType(pt string) (pyroscope.ProfileType, error) {
	switch pt {
	case "cpu":
		return pyroscope.ProfileCPU, nil
	case "alloc_objects":
		return pyroscope.ProfileAllocObjects, nil
	case "alloc_space":
		return pyroscope.ProfileAllocSpace, nil
	case "inuse_objects":
		return pyroscope.ProfileInuseObjects, nil
	case "inuse_space":
		return pyroscope.ProfileInuseSpace, nil
	case "goroutines":
		return pyroscope.ProfileGoroutines, nil
	case "mutex_count":
		return pyroscope.ProfileMutexCount, nil
	case "mutex_duration":
		return pyroscope.ProfileMutexDuration, nil
	case "block_count":
		return pyroscope.ProfileBlockCount, nil
	case "block_duration":
		return pyroscope.ProfileBlockDuration, nil
	default:
		return pyroscope.ProfileCPU, fmt.Errorf("unknown profile type: %s", pt)
	}
}
