package api

import (
	"os"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

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

// applyDefaults fills in zero values with sensible defaults.
func (c *APIConfig) applyDefaults() {
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
