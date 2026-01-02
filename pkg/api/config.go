package api

import "time"

// APIConfig configures the REST API HTTP server.
//
// The API server provides health check endpoints and will be extended
// with management APIs in future phases.
//
// When Enabled is false, no API server is started (zero overhead).
type APIConfig struct {
	// Enabled controls whether the API server is started.
	// Default: true (API is enabled by default)
	// Use a pointer to distinguish "not set" from "explicitly false"
	Enabled *bool `mapstructure:"enabled" yaml:"enabled"`

	// Port is the HTTP port for the API endpoints.
	// Default: 8080
	Port int `mapstructure:"port" validate:"omitempty,min=1,max=65535" yaml:"port"`

	// ReadTimeout is the maximum duration for reading the entire request,
	// including the body. A zero or negative value means there is no timeout.
	// Default: 10s
	ReadTimeout time.Duration `mapstructure:"read_timeout" yaml:"read_timeout"`

	// WriteTimeout is the maximum duration before timing out writes of the response.
	// A zero or negative value means there is no timeout.
	// Default: 10s
	WriteTimeout time.Duration `mapstructure:"write_timeout" yaml:"write_timeout"`

	// IdleTimeout is the maximum amount of time to wait for the next request
	// when keep-alives are enabled. If zero, the value of ReadTimeout is used.
	// Default: 60s
	IdleTimeout time.Duration `mapstructure:"idle_timeout" yaml:"idle_timeout"`
}

// IsEnabled returns whether the API server is enabled.
// Defaults to true if not explicitly set.
func (c *APIConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return true // Default: enabled
	}
	return *c.Enabled
}

// applyDefaults fills in zero values with sensible defaults.
func (c *APIConfig) applyDefaults() {
	if c.Port <= 0 {
		c.Port = 8080
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 10 * time.Second
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 60 * time.Second
	}
}
