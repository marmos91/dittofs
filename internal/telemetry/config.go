package telemetry

// Config holds OpenTelemetry configuration
type Config struct {
	// Enabled indicates whether tracing is enabled
	Enabled bool

	// ServiceName is the name of the service reported to the trace backend
	ServiceName string

	// ServiceVersion is the version of the service
	ServiceVersion string

	// Endpoint is the OTLP endpoint (e.g., "localhost:4317")
	Endpoint string

	// Insecure indicates whether to use insecure connection (no TLS)
	Insecure bool

	// SampleRate is the trace sampling rate (0.0 to 1.0)
	// 1.0 means sample all traces, 0.5 means sample 50%
	SampleRate float64
}

// DefaultConfig returns a default configuration
func DefaultConfig() Config {
	return Config{
		Enabled:        false,
		ServiceName:    "dittofs",
		ServiceVersion: "dev",
		Endpoint:       "localhost:4317",
		Insecure:       true,
		SampleRate:     1.0,
	}
}
