package config

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/controlplane/api"
)

// MetricsConfig configures the Prometheus metrics endpoint.
//
// Metrics are served on a dedicated listener separate from the control-plane
// API so scrapers can reach them without API authentication and so the two
// surfaces have independent lifecycles. The endpoint is opt-in (disabled by
// default) and binds to loopback by default — expose it deliberately via Host
// plus a network policy / firewall, or enable token auth.
type MetricsConfig struct {
	// Enabled turns the metrics listener on. Default: false (opt-in).
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`

	// Host is the interface the metrics listener binds to.
	// Default: 127.0.0.1 (loopback only). Set to 0.0.0.0 for off-host scraping
	// (then rely on a NetworkPolicy / firewall, or enable token auth).
	Host string `mapstructure:"host" yaml:"host"`

	// Port is the TCP port for the metrics endpoint. Default: 9090.
	Port int `mapstructure:"port" validate:"omitempty,min=1,max=65535" yaml:"port"`

	// Path is the HTTP path the metrics are served on. Default: /metrics.
	Path string `mapstructure:"path" yaml:"path"`

	// Auth selects the authentication mode for the endpoint:
	//   - "none"  (default): unauthenticated; rely on Host + NetworkPolicy.
	//   - "token": require a Bearer token matching the contents of TokenFile.
	Auth string `mapstructure:"auth" validate:"omitempty,oneof=none token" yaml:"auth"`

	// TokenFile is the path to a file containing the Bearer token required when
	// Auth is "token". The file's trimmed contents are the expected token.
	TokenFile string `mapstructure:"token_file" yaml:"token_file"`

	// TLS optionally serves the endpoint over HTTPS (and supports mTLS). Reuses
	// the control-plane TLS config shape. When unset the endpoint is plain HTTP.
	TLS api.TLSConfig `mapstructure:"tls" yaml:"tls"`
}

// Addr returns the host:port the metrics listener binds to.
func (c *MetricsConfig) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// ApplyDefaults fills any zero-valued field with sensible defaults. Defaults
// are applied regardless of Enabled so a later toggle has coherent settings.
func (c *MetricsConfig) ApplyDefaults() {
	if c.Host == "" {
		c.Host = "127.0.0.1"
	}
	if c.Port == 0 {
		c.Port = 9090
	}
	if c.Path == "" {
		c.Path = "/metrics"
	}
	if c.Auth == "" {
		c.Auth = "none"
	}
}

// Validate checks the metrics config for internally inconsistent settings.
// It is fail-fast and does not read the token/cert files from disk.
func (c *MetricsConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Auth == "token" && c.TokenFile == "" {
		return fmt.Errorf("metrics.auth is \"token\" but metrics.token_file is not set")
	}
	// Reuse the control-plane TLS pairing/min-version rules.
	tlsCheck := api.APIConfig{TLS: c.TLS}
	if err := tlsCheck.Validate(); err != nil {
		return fmt.Errorf("metrics.tls: %w", err)
	}
	return nil
}
