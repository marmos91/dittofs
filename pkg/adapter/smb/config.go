package smb

import (
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/smb/session"
	"github.com/marmos91/dittofs/internal/protocol/smb/signing"
)

// DefaultMaxMessageSize is the default maximum allowed size for a single SMB2 message (64MB).
// This provides DoS protection by rejecting oversized messages while allowing
// large file operations. The SMB2 spec doesn't define a maximum, but 64MB is
// generous for typical operations (most SMB2 messages are < 1MB).
const DefaultMaxMessageSize = 64 * 1024 * 1024

// SMBTimeoutsConfig groups all timeout-related configuration.
type SMBTimeoutsConfig struct {
	// Read is the maximum duration for reading a complete SMB2 request.
	// This prevents slow or malicious clients from holding connections indefinitely.
	// 0 means no timeout (not recommended).
	// Recommended: 30s for LAN, 60s for WAN.
	Read time.Duration `mapstructure:"read" validate:"min=0"`

	// Write is the maximum duration for writing an SMB2 response.
	// This prevents slow networks or clients from blocking server resources.
	// 0 means no timeout (not recommended).
	// Recommended: 30s for LAN, 60s for WAN.
	Write time.Duration `mapstructure:"write" validate:"min=0"`

	// Idle is the maximum duration a connection can remain idle
	// between requests before being closed automatically.
	// This frees resources from abandoned connections.
	// 0 means no timeout (connections stay open indefinitely).
	// Recommended: 5m for production.
	Idle time.Duration `mapstructure:"idle" validate:"min=0"`

	// Shutdown is the maximum duration to wait for active connections
	// to complete during graceful shutdown.
	// After this timeout, remaining connections are forcibly closed.
	// Must be > 0 to ensure shutdown completes.
	// Recommended: 30s (balances graceful shutdown with restart time).
	Shutdown time.Duration `mapstructure:"shutdown" validate:"required,gt=0"`
}

// SMBConfig holds configuration parameters for the SMB server.
//
// These values control server behavior including connection limits, timeouts,
// and resource management.
//
// Default values (applied by New if zero):
//   - Port: 12445 (non-privileged port, standard is 445)
//   - MaxConnections: 0 (unlimited)
//   - Timeouts.Read: 5m
//   - Timeouts.Write: 30s
//   - Timeouts.Idle: 5m
//   - Timeouts.Shutdown: 30s
//   - MetricsLogInterval: 5m (0 disables)
//
// Production recommendations:
//   - MaxConnections: Set based on expected load (e.g., 1000 for busy servers)
//   - Use non-standard port (e.g., 12445) for testing without root privileges
type SMBConfig struct {
	// Enabled controls whether the SMB adapter is active.
	// When false, the SMB adapter will not be started.
	Enabled bool `mapstructure:"enabled"`

	// Port is the TCP port to listen on for SMB connections.
	// Standard SMB port is 445, but requires root. Must be > 0.
	// If 0, defaults to 12445.
	Port int `mapstructure:"port" validate:"min=0,max=65535"`

	// MaxConnections limits the number of concurrent client connections.
	// When reached, new connections are rejected until existing ones close.
	// 0 means unlimited (not recommended for production).
	// Recommended: 1000-5000 for production servers.
	MaxConnections int `mapstructure:"max_connections" validate:"min=0"`

	// MaxRequestsPerConnection limits the number of concurrent SMB2 requests
	// that can be processed simultaneously on a single connection.
	// This enables parallel handling of multiple operations.
	// 0 means unlimited (will default to 100).
	// Recommended: 50-200 for high-throughput servers.
	MaxRequestsPerConnection int `mapstructure:"max_requests_per_connection" validate:"min=0"`

	// Timeouts groups all timeout-related configuration
	Timeouts SMBTimeoutsConfig `mapstructure:"timeouts"`

	// MetricsLogInterval is the interval at which to log server metrics
	// (active connections, requests/sec, etc.).
	// 0 disables periodic metrics logging.
	// Recommended: 5m for production monitoring.
	MetricsLogInterval time.Duration `mapstructure:"metrics_log_interval" validate:"min=0"`

	// Credits configures SMB2 credit management behavior.
	// Credits control flow control and parallelism per client.
	Credits SMBCreditsConfig `mapstructure:"credits"`

	// MaxMessageSize is the maximum allowed size for a single SMB2 message.
	// This provides DoS protection by rejecting oversized messages.
	// 0 means use the default (64MB).
	// Recommended: 64MB for most deployments, lower for constrained environments.
	MaxMessageSize int `mapstructure:"max_message_size" validate:"min=0"`

	// Signing configures SMB2 message signing behavior.
	// Signing provides message integrity protection using HMAC-SHA256.
	Signing SMBSigningConfig `mapstructure:"signing"`
}

// SMBSigningConfig configures SMB2 message signing.
//
// Message signing provides integrity protection using HMAC-SHA256, preventing
// man-in-the-middle attacks and message tampering. Per MS-SMB2, signing is:
//   - Advertised during NEGOTIATE based on Enabled flag
//   - Configured per-session during SESSION_SETUP based on Required flag
//
// When Required is true, all authenticated sessions must use signing and
// unsigned messages will be rejected. When Enabled is true but Required is
// false, signing is available but not mandatory.
//
// Default values (applied if not specified):
//   - Enabled: true (signing capability is advertised)
//   - Required: false (signing is optional)
//
// Production recommendations:
//   - Set Required: true for security-sensitive environments
//   - Keep Enabled: true for client compatibility
type SMBSigningConfig struct {
	// Enabled controls whether signing capability is advertised to clients.
	// When true, SMB2_NEGOTIATE_SIGNING_ENABLED is set in NEGOTIATE response.
	// Default: true
	Enabled *bool `mapstructure:"enabled"`

	// Required controls whether signing is mandatory for all sessions.
	// When true, SMB2_NEGOTIATE_SIGNING_REQUIRED is set and unsigned
	// messages from established sessions will be rejected.
	// Default: false
	Required bool `mapstructure:"required"`
}

// applyDefaults fills in nil values with sensible defaults.
func (c *SMBSigningConfig) applyDefaults() {
	if c.Enabled == nil {
		enabled := true
		c.Enabled = &enabled
	}

	// Ensure logical consistency: signing cannot be required if it is disabled.
	// If Required is true, force Enabled to true.
	// Note: c.Enabled is guaranteed non-nil at this point from the above check.
	if c.Required && !*c.Enabled {
		enabled := true
		c.Enabled = &enabled
	}
}

// ToSigningConfig converts to the internal signing.SigningConfig type.
// It assumes applyDefaults has been called to initialize any nil fields.
func (c *SMBSigningConfig) ToSigningConfig() signing.SigningConfig {
	return signing.SigningConfig{
		Enabled:  *c.Enabled,
		Required: c.Required,
	}
}

// SMBCreditsConfig configures SMB2 credit management.
//
// Credits are flow control tokens that limit how many concurrent operations
// a client can have outstanding. Proper configuration balances throughput
// (more credits = more parallelism) with protection (fewer credits = less DoS risk).
//
// Strategy options:
//   - "fixed": Always grant InitialGrant credits. Simple but doesn't adapt.
//   - "echo": Grant what client requests (within bounds). Maintains client pool.
//   - "adaptive": Adjusts based on server load and client behavior. Recommended.
//
// Default values (applied if zero):
//   - Strategy: "adaptive"
//   - MinGrant: 16
//   - MaxGrant: 8192
//   - InitialGrant: 256
//   - MaxSessionCredits: 65535
type SMBCreditsConfig struct {
	// Strategy is the credit grant strategy.
	// Valid values: "fixed", "echo", "adaptive" (default: "adaptive")
	Strategy string `mapstructure:"strategy" validate:"omitempty,oneof=fixed echo adaptive"`

	// MinGrant is the minimum credits to grant per response.
	// Always granting at least some credits prevents client deadlock.
	// Default: 16
	MinGrant uint16 `mapstructure:"min_grant" validate:"min=1"`

	// MaxGrant is the maximum credits to grant per response.
	// Limits memory exposure from a single client.
	// Default: 8192
	MaxGrant uint16 `mapstructure:"max_grant" validate:"min=1"`

	// InitialGrant is credits granted for initial requests (NEGOTIATE, SESSION_SETUP).
	// Higher values allow faster client startup, lower values are more conservative.
	// Default: 256
	InitialGrant uint16 `mapstructure:"initial_grant" validate:"min=1"`

	// MaxSessionCredits limits total outstanding credits per session.
	// Prevents a single client from monopolizing server resources.
	// Default: 65535 (~64K credits)
	MaxSessionCredits uint32 `mapstructure:"max_session_credits" validate:"min=1"`

	// LoadThresholdHigh triggers throttling when active requests exceed this.
	// Only used by "adaptive" strategy.
	// Default: 1000
	LoadThresholdHigh int64 `mapstructure:"load_threshold_high" validate:"min=0"`

	// LoadThresholdLow triggers credit boost when active requests are below this.
	// Only used by "adaptive" strategy.
	// Default: 100
	LoadThresholdLow int64 `mapstructure:"load_threshold_low" validate:"min=0"`

	// AggressiveClientThreshold triggers throttling when a session has this many
	// outstanding requests. Only used by "adaptive" strategy.
	// Default: 256
	AggressiveClientThreshold int64 `mapstructure:"aggressive_client_threshold" validate:"min=0"`
}

// applyDefaults fills in zero values with sensible defaults.
func (c *SMBConfig) applyDefaults() {
	if c.Port <= 0 {
		c.Port = 12445
	}
	if c.MaxRequestsPerConnection == 0 {
		c.MaxRequestsPerConnection = 100
	}
	if c.Timeouts.Read == 0 {
		c.Timeouts.Read = 5 * time.Minute
	}
	if c.Timeouts.Write == 0 {
		c.Timeouts.Write = 30 * time.Second
	}
	if c.Timeouts.Idle == 0 {
		c.Timeouts.Idle = 5 * time.Minute
	}
	if c.Timeouts.Shutdown == 0 {
		c.Timeouts.Shutdown = 30 * time.Second
	}
	if c.MetricsLogInterval == 0 {
		c.MetricsLogInterval = 5 * time.Minute
	}
	if c.MaxMessageSize == 0 {
		c.MaxMessageSize = DefaultMaxMessageSize
	}

	// Apply credit defaults
	c.Credits.applyDefaults()

	// Apply signing defaults
	c.Signing.applyDefaults()
}

// applyDefaults fills in zero values with sensible defaults.
func (c *SMBCreditsConfig) applyDefaults() {
	if c.Strategy == "" {
		c.Strategy = "adaptive"
	}
	if c.MinGrant == 0 {
		c.MinGrant = 16
	}
	if c.MaxGrant == 0 {
		c.MaxGrant = session.MaximumCreditGrant
	}
	if c.InitialGrant == 0 {
		c.InitialGrant = session.DefaultInitialCredits
	}
	if c.MaxSessionCredits == 0 {
		c.MaxSessionCredits = 65535
	}
	if c.LoadThresholdHigh == 0 {
		c.LoadThresholdHigh = 1000
	}
	if c.LoadThresholdLow == 0 {
		c.LoadThresholdLow = 100
	}
	if c.AggressiveClientThreshold == 0 {
		c.AggressiveClientThreshold = 256
	}
}

// ToSessionConfig converts to the internal session.CreditConfig type.
func (c *SMBCreditsConfig) ToSessionConfig() session.CreditConfig {
	return session.CreditConfig{
		MinGrant:                  c.MinGrant,
		MaxGrant:                  c.MaxGrant,
		InitialGrant:              c.InitialGrant,
		MaxSessionCredits:         c.MaxSessionCredits,
		LoadThresholdHigh:         c.LoadThresholdHigh,
		LoadThresholdLow:          c.LoadThresholdLow,
		AggressiveClientThreshold: c.AggressiveClientThreshold,
	}
}

// GetStrategy returns the CreditStrategy enum for the configured strategy.
func (c *SMBCreditsConfig) GetStrategy() session.CreditStrategy {
	switch c.Strategy {
	case "fixed":
		return session.StrategyFixed
	case "echo":
		return session.StrategyEcho
	case "adaptive":
		return session.StrategyAdaptive
	default:
		return session.StrategyAdaptive
	}
}

// validate checks that the configuration is valid for production use.
func (c *SMBConfig) validate() error {
	// Port 0 is valid - it means OS-assigned port (useful for testing)
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("invalid port %d: must be 0-65535", c.Port)
	}
	if c.MaxConnections < 0 {
		return fmt.Errorf("invalid MaxConnections %d: must be >= 0", c.MaxConnections)
	}
	if c.Timeouts.Read < 0 {
		return fmt.Errorf("invalid timeouts.read %v: must be >= 0", c.Timeouts.Read)
	}
	if c.Timeouts.Write < 0 {
		return fmt.Errorf("invalid timeouts.write %v: must be >= 0", c.Timeouts.Write)
	}
	if c.Timeouts.Idle < 0 {
		return fmt.Errorf("invalid timeouts.idle %v: must be >= 0", c.Timeouts.Idle)
	}
	if c.Timeouts.Shutdown <= 0 {
		return fmt.Errorf("invalid timeouts.shutdown %v: must be > 0", c.Timeouts.Shutdown)
	}
	return nil
}
