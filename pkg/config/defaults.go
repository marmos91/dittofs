package config

import (
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/pkg/controlplane/api"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// ApplyDefaults sets default values for any unspecified configuration fields.
//
// This function is called after loading configuration from file and environment
// variables to fill in any missing values with sensible defaults.
//
// Default Strategy:
//   - Zero values (0, "", false, nil) are replaced with defaults
//   - Explicit values are preserved
func ApplyDefaults(cfg *Config) {
	applyLoggingDefaults(&cfg.Logging)
	applyTelemetryDefaults(&cfg.Telemetry)
	applyShutdownTimeoutDefaults(cfg)
	applyDatabaseDefaults(&cfg.Database)
	applyMetricsDefaults(&cfg.Metrics)
	applyControlPlaneDefaults(&cfg.ControlPlane)
	applyCacheDefaults(&cfg.Cache)
	applyAdminDefaults(&cfg.Admin)
	applyLockDefaults(&cfg.Lock)
	applyKerberosDefaults(&cfg.Kerberos)
}

// applyLoggingDefaults sets logging defaults and normalizes values.
func applyLoggingDefaults(cfg *LoggingConfig) {
	if cfg.Level == "" {
		cfg.Level = "INFO"
	}
	// Normalize log level to uppercase for consistent internal representation
	cfg.Level = strings.ToUpper(cfg.Level)

	if cfg.Format == "" {
		cfg.Format = "text"
	}
	if cfg.Output == "" {
		cfg.Output = "stdout"
	}
}

// applyTelemetryDefaults sets OpenTelemetry defaults.
func applyTelemetryDefaults(cfg *TelemetryConfig) {
	// Enabled defaults to false (opt-in for telemetry)
	// No need to set, zero value is false

	// Default endpoint is localhost:4317 (standard OTLP gRPC port)
	if cfg.Endpoint == "" {
		cfg.Endpoint = "localhost:4317"
	}

	// Default sample rate is 1.0 (sample all traces)
	if cfg.SampleRate == 0 {
		cfg.SampleRate = 1.0
	}

	// Apply profiling defaults
	applyProfilingDefaults(&cfg.Profiling)
}

// applyProfilingDefaults sets Pyroscope profiling defaults.
func applyProfilingDefaults(cfg *ProfilingConfig) {
	// Enabled defaults to false (opt-in for profiling)
	// No need to set, zero value is false

	// Default endpoint is localhost:4040 (standard Pyroscope port)
	if cfg.Endpoint == "" {
		cfg.Endpoint = "http://localhost:4040"
	}

	// Default profile types include CPU, memory allocation, and goroutines
	if len(cfg.ProfileTypes) == 0 {
		cfg.ProfileTypes = []string{
			"cpu",
			"alloc_objects",
			"alloc_space",
			"inuse_objects",
			"inuse_space",
			"goroutines",
		}
	}
}

// applyShutdownTimeoutDefaults sets shutdown timeout defaults.
func applyShutdownTimeoutDefaults(cfg *Config) {
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 30 * time.Second
	}
}

// applyDatabaseDefaults sets control plane database defaults.
func applyDatabaseDefaults(cfg *store.Config) {
	cfg.ApplyDefaults()
}

// applyMetricsDefaults sets metrics defaults.
func applyMetricsDefaults(cfg *MetricsConfig) {
	// Enabled defaults to false (opt-in for metrics)
	// Port defaults to 9090 if metrics are enabled
	if cfg.Enabled && cfg.Port == 0 {
		cfg.Port = 9090
	}
}

// applyControlPlaneDefaults sets control plane API server defaults.
// API is always enabled (mandatory for managing shares and users).
func applyControlPlaneDefaults(cfg *api.APIConfig) {
	if cfg.Port == 0 {
		cfg.Port = 8080
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 10 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 10 * time.Second
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
}

// applyCacheDefaults sets cache defaults.
// Cache path is required (WAL is mandatory for crash recovery).
func applyCacheDefaults(cfg *CacheConfig) {
	// Default size to 1GB
	if cfg.Size == 0 {
		cfg.Size = bytesize.ByteSize(bytesize.GiB) // 1 GiB
	}
	// Path has no default - it's required and must be configured by user
}

// applyAdminDefaults sets admin user defaults.
func applyAdminDefaults(cfg *AdminConfig) {
	// Default username is "admin"
	if cfg.Username == "" {
		cfg.Username = "admin"
	}
	// Email and PasswordHash have no defaults - they're optional or set during init
}

// applyLockDefaults sets lock manager defaults.
func applyLockDefaults(cfg *LockConfig) {
	// LeaseBreakTimeout defaults to 35 seconds (SMB2 spec maximum, MS-SMB2 2.2.23)
	// This is the Windows default and provides maximum time for SMB clients
	// to acknowledge lease breaks and flush cached data.
	// For CI tests, set DITTOFS_LOCK_LEASE_BREAK_TIMEOUT=5s for faster execution.
	if cfg.LeaseBreakTimeout == 0 {
		cfg.LeaseBreakTimeout = 35 * time.Second
	}
}

// applyKerberosDefaults sets Kerberos authentication defaults.
//
// When Kerberos is enabled, the keytab path and service principal must
// be configured either in the config file or via environment variables:
//   - DITTOFS_KERBEROS_KEYTAB overrides KeytabPath (DITTOFS_KERBEROS_KEYTAB_PATH for compat)
//   - DITTOFS_KERBEROS_PRINCIPAL overrides ServicePrincipal (DITTOFS_KERBEROS_SERVICE_PRINCIPAL for compat)
func applyKerberosDefaults(cfg *KerberosConfig) {
	// Enabled defaults to false (opt-in for Kerberos)
	// No need to set, zero value is false

	// Default krb5.conf path
	if cfg.Krb5Conf == "" {
		cfg.Krb5Conf = "/etc/krb5.conf"
	}

	// Default max clock skew: 5 minutes (standard Kerberos default)
	if cfg.MaxClockSkew == 0 {
		cfg.MaxClockSkew = 5 * time.Minute
	}

	// Default context TTL: 8 hours (typical workday)
	if cfg.ContextTTL == 0 {
		cfg.ContextTTL = 8 * time.Hour
	}

	// Default max concurrent contexts
	if cfg.MaxContexts == 0 {
		cfg.MaxContexts = 10000
	}

	// Identity mapping defaults
	if cfg.IdentityMapping.Strategy == "" {
		cfg.IdentityMapping.Strategy = "static"
	}
	if cfg.IdentityMapping.DefaultUID == 0 {
		cfg.IdentityMapping.DefaultUID = 65534 // nobody
	}
	if cfg.IdentityMapping.DefaultGID == 0 {
		cfg.IdentityMapping.DefaultGID = 65534 // nogroup
	}
}

// GetDefaultConfig returns a Config struct with all default values applied.
//
// This is useful for:
//   - Generating sample configuration files
//   - Testing
//   - Documentation
func GetDefaultConfig() *Config {
	cfg := &Config{
		Logging: LoggingConfig{},
		Database: store.Config{
			Type: store.DatabaseTypeSQLite, // Default to SQLite for single-node
		},
		Cache: CacheConfig{
			Path: "/tmp/dittofs-cache",
			Size: bytesize.ByteSize(bytesize.GiB), // 1 GiB
		},
		Admin: AdminConfig{
			Username: "admin",
		},
	}

	ApplyDefaults(cfg)
	return cfg
}
