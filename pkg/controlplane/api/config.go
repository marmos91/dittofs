package api

import (
	"encoding/json"
	"os"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// redactedSecret is the sentinel substituted for sensitive fields when a
// config struct is serialized for display (e.g. `dfs config show`). It is
// never parsed back on the load path, which uses mapstructure/viper rather
// than json/yaml Unmarshal of these types.
const redactedSecret = "********"

// EnvControlPlaneSecret is the name of the environment variable for the control plane's JWT authentication signing secret.
const EnvControlPlaneSecret = "DITTOFS_CONTROLPLANE_SECRET"

// APIConfig configures the REST API HTTP server.
//
// The API server provides health check endpoints, authentication endpoints,
// and user management APIs. The API is always enabled as it is required for
// managing shares, users, and other dynamic configuration.
type APIConfig struct {
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

	// JWT configures JWT authentication for API endpoints.
	JWT JWTConfig `mapstructure:"jwt" yaml:"jwt"`

	// Pprof enables Go pprof profiling endpoints at /debug/pprof/*.
	// Useful for CPU, memory, and goroutine profiling during benchmarks.
	// Default: false
	Pprof bool `mapstructure:"pprof" yaml:"pprof"`

	// PprofMutexRate is the sampling fraction passed to
	// runtime.SetMutexProfileFraction (one mutex contention event sampled per N
	// events). Without it, /debug/pprof/mutex is an empty (header-only) profile
	// even when Pprof is enabled. Only applied when Pprof is true; when Pprof is
	// false it is forced to 0 (sampling off). A zero/unset value with Pprof on
	// falls back to the default 100 — to turn profiling off entirely, set Pprof
	// to false rather than zeroing this.
	PprofMutexRate int `mapstructure:"pprof_mutex_rate" validate:"omitempty,min=0" yaml:"pprof_mutex_rate"`

	// PprofBlockRateNs is the rate (in nanoseconds) passed to
	// runtime.SetBlockProfileRate (one blocking event sampled per N ns blocked).
	// Without it, /debug/pprof/block is an empty (header-only) profile even when
	// Pprof is enabled. Only applied when Pprof is true; when Pprof is false it
	// is forced to 0 (sampling off). A zero/unset value with Pprof on falls back
	// to the default 1_000_000 — to turn profiling off entirely, set Pprof to
	// false rather than zeroing this.
	PprofBlockRateNs int `mapstructure:"pprof_block_rate_ns" validate:"omitempty,min=0" yaml:"pprof_block_rate_ns"`
}

// JWTConfig configures JWT token generation and validation.
type JWTConfig struct {
	// Secret is the HMAC signing key for JWT tokens.
	// Must be at least 32 characters long.
	// Can also be set via DITTOFS_CONTROLPLANE_SECRET environment variable.
	// Environment variable takes precedence over config file.
	Secret string `mapstructure:"secret" yaml:"secret"`

	// AccessTokenDuration is the lifetime of access tokens.
	// Default: 15m
	AccessTokenDuration time.Duration `mapstructure:"access_token_duration" yaml:"access_token_duration"`

	// RefreshTokenDuration is the lifetime of refresh tokens.
	// Default: 168h (7 days)
	RefreshTokenDuration time.Duration `mapstructure:"refresh_token_duration" yaml:"refresh_token_duration"`
}

// MarshalYAML redacts the JWT signing secret when the config is serialized
// for display. Only the secret is masked; an empty secret stays empty so
// "no secret configured" is distinguishable from a redacted one.
func (c JWTConfig) MarshalYAML() (interface{}, error) {
	type alias JWTConfig // avoid infinite recursion
	out := alias(c)
	if out.Secret != "" {
		out.Secret = redactedSecret
	}
	return out, nil
}

// MarshalJSON redacts the JWT signing secret when the config is serialized
// for display. See MarshalYAML.
func (c JWTConfig) MarshalJSON() ([]byte, error) {
	type alias JWTConfig // avoid infinite recursion
	out := alias(c)
	if out.Secret != "" {
		out.Secret = redactedSecret
	}
	return json.Marshal(out)
}

// ApplyDefaults fills in zero values with sensible defaults. It is the single
// source of truth for API config defaults — both NewServer and the global
// config.ApplyDefaults path call it, so a new field defaulted here cannot drift
// between the two entry points.
func (c *APIConfig) ApplyDefaults() {
	if c.Port == 0 {
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
	// When pprof is on but the sampling rates are left unset, fall back to
	// sensible defaults so /debug/pprof/{mutex,block} return non-empty profiles
	// out of the box. Left at zero (disabled) when pprof is off.
	if c.Pprof {
		if c.PprofMutexRate == 0 {
			c.PprofMutexRate = 100
		}
		if c.PprofBlockRateNs == 0 {
			c.PprofBlockRateNs = 1_000_000
		}
	}
	// JWT defaults
	if c.JWT.AccessTokenDuration == 0 {
		c.JWT.AccessTokenDuration = 15 * time.Minute
	}
	if c.JWT.RefreshTokenDuration == 0 {
		c.JWT.RefreshTokenDuration = 7 * 24 * time.Hour
	}
}

// GetJWTSecret returns the JWT secret, preferring the environment variable.
// Returns empty string if neither env var nor config secret is set.
// Logs a warning if the environment variable overrides a config file value.
func (c *APIConfig) GetJWTSecret() string {
	envSecret := os.Getenv(EnvControlPlaneSecret)
	if envSecret != "" {
		if c.JWT.Secret != "" && c.JWT.Secret != envSecret {
			logger.Warn("JWT secret from environment variable overrides config file value",
				"env_var", EnvControlPlaneSecret)
		}
		return envSecret
	}
	return c.JWT.Secret
}

// HasJWTSecret returns whether a JWT secret is configured.
func (c *APIConfig) HasJWTSecret() bool {
	return c.GetJWTSecret() != ""
}
