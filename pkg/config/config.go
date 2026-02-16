package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/pkg/controlplane/api"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Config represents the DittoFS configuration.
//
// This structure captures static configuration aspects of the DittoFS server:
//   - Logging configuration
//   - Telemetry/tracing configuration
//   - Server settings (shutdown timeout, metrics, API)
//   - Database connection (control plane persistence)
//   - Cache configuration (WAL-backed, mandatory for crash recovery)
//   - Admin user setup (for initial bootstrap)
//
// Dynamic configuration (users, groups, shares, stores, adapters) is managed
// through the REST API and stored in the control plane database.
//
// Configuration sources (in order of precedence):
//  1. CLI flags (highest priority)
//  2. Environment variables (DITTOFS_*)
//  3. Configuration file (YAML or TOML)
//  4. Default values (lowest priority)
type Config struct {
	// Logging controls log output behavior
	Logging LoggingConfig `mapstructure:"logging" yaml:"logging"`

	// Telemetry controls OpenTelemetry distributed tracing
	Telemetry TelemetryConfig `mapstructure:"telemetry" yaml:"telemetry"`

	// ShutdownTimeout is the maximum time to wait for graceful shutdown
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout" validate:"required,gt=0" yaml:"shutdown_timeout"`

	// Database configures the control plane database (SQLite or PostgreSQL).
	// This is the persistent store for users, groups, shares, and configuration.
	Database store.Config `mapstructure:"database" yaml:"database"`

	// Metrics contains Prometheus metrics server configuration
	Metrics MetricsConfig `mapstructure:"metrics" yaml:"metrics"`

	// ControlPlane contains control plane API server configuration
	ControlPlane api.APIConfig `mapstructure:"controlplane" yaml:"controlplane"`

	// Cache specifies the WAL-backed cache configuration
	// Cache is mandatory for crash recovery - all writes go through cache
	Cache CacheConfig `mapstructure:"cache" yaml:"cache"`

	// Admin contains initial admin user configuration for bootstrap
	// This is used by 'dittofs init' to set up the first admin user
	Admin AdminConfig `mapstructure:"admin" yaml:"admin"`

	// Lock contains lock manager configuration
	// Controls lock limits, timeouts, and behavior
	Lock LockConfig `mapstructure:"lock" yaml:"lock"`

	// Kerberos contains Kerberos/RPCSEC_GSS authentication configuration.
	// When enabled, NFS clients can authenticate using Kerberos tickets
	// via the RPCSEC_GSS protocol (RFC 2203).
	// Environment variable overrides:
	//   DITTOFS_KERBEROS_KEYTAB overrides KeytabPath (DITTOFS_KERBEROS_KEYTAB_PATH for compat)
	//   DITTOFS_KERBEROS_PRINCIPAL overrides ServicePrincipal (DITTOFS_KERBEROS_SERVICE_PRINCIPAL for compat)
	Kerberos KerberosConfig `mapstructure:"kerberos" yaml:"kerberos"`
}

// LockConfig contains lock manager configuration.
// These settings control lock limits, timeouts, and behavior across
// all protocols (NLM, SMB, NFSv4).
type LockConfig struct {
	// MaxLocksPerFile is the maximum number of locks allowed on a single file.
	// Default: 1000
	MaxLocksPerFile int `mapstructure:"max_locks_per_file" yaml:"max_locks_per_file"`

	// MaxLocksPerClient is the maximum number of locks a single client can hold.
	// Default: 10000
	MaxLocksPerClient int `mapstructure:"max_locks_per_client" yaml:"max_locks_per_client"`

	// MaxTotalLocks is the maximum total locks across all files and clients.
	// Default: 100000
	MaxTotalLocks int `mapstructure:"max_total_locks" yaml:"max_total_locks"`

	// BlockingTimeout is the server-side timeout for blocking lock requests.
	// Default: 60s
	BlockingTimeout time.Duration `mapstructure:"blocking_timeout" yaml:"blocking_timeout"`

	// GracePeriodDuration is the duration of the grace period after server restart.
	// Default: 90s
	GracePeriodDuration time.Duration `mapstructure:"grace_period" yaml:"grace_period"`

	// MandatoryLocking controls whether locks are mandatory or advisory.
	// Default: false (advisory)
	MandatoryLocking bool `mapstructure:"mandatory_locking" yaml:"mandatory_locking"`

	// LeaseBreakTimeout is how long to wait for SMB lease breaks before proceeding.
	// This is the maximum time NFS/NLM operations will wait for an SMB client to
	// acknowledge a lease break and flush cached data.
	// Default: 35s (SMB2 spec maximum, MS-SMB2 2.2.23)
	// Set to 5s for faster CI tests via: DITTOFS_LOCK_LEASE_BREAK_TIMEOUT=5s
	LeaseBreakTimeout time.Duration `mapstructure:"lease_break_timeout" yaml:"lease_break_timeout"`
}

// LoggingConfig controls logging behavior.
type LoggingConfig struct {
	// Level is the minimum log level to output
	// Valid values: DEBUG, INFO, WARN, ERROR (case-insensitive, normalized to uppercase)
	Level string `mapstructure:"level" validate:"required,oneof=DEBUG INFO WARN ERROR debug info warn error" yaml:"level"`

	// Format specifies the log output format
	// Valid values: text, json
	Format string `mapstructure:"format" validate:"required,oneof=text json" yaml:"format"`

	// Output specifies where logs are written
	// Valid values: stdout, stderr, or a file path
	Output string `mapstructure:"output" validate:"required" yaml:"output"`
}

// TelemetryConfig controls OpenTelemetry distributed tracing.
// When enabled, trace data is exported to an OTLP-compatible collector
// (e.g., Jaeger, Tempo, or any OTLP receiver).
type TelemetryConfig struct {
	// Enabled controls whether distributed tracing is enabled
	// Default: false (opt-in for telemetry)
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`

	// Endpoint is the OTLP collector endpoint (host:port)
	// Default: "localhost:4317" (standard OTLP gRPC port)
	Endpoint string `mapstructure:"endpoint" yaml:"endpoint"`

	// Insecure controls whether to use insecure (non-TLS) connection
	// Default: true (for local development)
	// Set to false in production with a TLS-enabled collector
	Insecure bool `mapstructure:"insecure" yaml:"insecure"`

	// SampleRate controls the trace sampling rate (0.0 to 1.0)
	// 1.0 = sample all traces, 0.5 = sample 50%, 0.0 = no sampling
	// Default: 1.0 (sample all)
	SampleRate float64 `mapstructure:"sample_rate" validate:"omitempty,gte=0,lte=1" yaml:"sample_rate"`

	// Profiling contains Pyroscope continuous profiling configuration
	Profiling ProfilingConfig `mapstructure:"profiling" yaml:"profiling"`
}

// ProfilingConfig controls Pyroscope continuous profiling.
// When enabled, CPU and memory profiles are continuously sent to a Pyroscope server
// for flame graph visualization and performance analysis.
type ProfilingConfig struct {
	// Enabled controls whether continuous profiling is enabled
	// Default: false (opt-in for profiling)
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`

	// Endpoint is the Pyroscope server endpoint (URL)
	// Default: "http://localhost:4040" (standard Pyroscope port)
	Endpoint string `mapstructure:"endpoint" yaml:"endpoint"`

	// ProfileTypes specifies which profile types to collect
	// Valid values: cpu, alloc_objects, alloc_space, inuse_objects, inuse_space,
	//               goroutines, mutex_count, mutex_duration, block_count, block_duration
	// Default: ["cpu", "alloc_objects", "alloc_space", "inuse_objects", "inuse_space", "goroutines"]
	ProfileTypes []string `mapstructure:"profile_types" yaml:"profile_types"`
}

// MetricsConfig configures the Prometheus metrics HTTP server.
// When Enabled is false, no metrics are collected (zero overhead).
type MetricsConfig struct {
	// Enabled controls whether metrics collection and HTTP server are enabled
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`

	// Port is the HTTP port for the metrics endpoint
	// Default: 9090
	Port int `mapstructure:"port" validate:"omitempty,min=1,max=65535" yaml:"port"`
}

// CacheConfig specifies the WAL-backed cache configuration.
// Cache is mandatory for crash recovery - all writes go through the WAL cache.
// The WAL (Write-Ahead Log) ensures data durability via mmap.
type CacheConfig struct {
	// Path is the directory for the cache WAL file (required)
	// The cache will create a cache.dat file in this directory
	// Example: /var/lib/dittofs/cache or /tmp/dittofs-cache
	Path string `mapstructure:"path" validate:"required" yaml:"path"`

	// Size is the maximum cache size
	// Supports human-readable formats: "1GB", "512MB", "10Gi"
	// Default: 1GB
	Size bytesize.ByteSize `mapstructure:"size" yaml:"size,omitempty"`
}

// AdminConfig contains initial admin user configuration for bootstrap.
// This is used by 'dittofs init' to pre-configure the first admin user.
type AdminConfig struct {
	// Username is the admin username
	// Default: "admin"
	Username string `mapstructure:"username" yaml:"username"`

	// Email is the admin user's email address (optional)
	Email string `mapstructure:"email" yaml:"email,omitempty"`

	// PasswordHash is the bcrypt hash of the admin password
	// Generated during 'dittofs init' or can be set manually
	// Use: htpasswd -nbB "" "password" | cut -d: -f2
	PasswordHash string `mapstructure:"password_hash" yaml:"password_hash,omitempty"`
}

// KerberosConfig contains Kerberos/RPCSEC_GSS authentication configuration.
//
// When Enabled is true, the NFS server supports Kerberos authentication
// via RPCSEC_GSS (RFC 2203). Clients can authenticate using krb5, krb5i
// (integrity), or krb5p (privacy) security flavors.
//
// The server needs a keytab file containing the service principal's key
// and a valid krb5.conf for realm/KDC resolution.
type KerberosConfig struct {
	// Enabled controls whether Kerberos authentication is active.
	// Default: false (AUTH_UNIX only)
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`

	// KeytabPath is the path to the Kerberos keytab file.
	// The keytab must contain the service principal's key.
	// Override: DITTOFS_KERBEROS_KEYTAB (primary), DITTOFS_KERBEROS_KEYTAB_PATH (compat)
	// Example: /etc/dittofs/dittofs.keytab
	KeytabPath string `mapstructure:"keytab_path" yaml:"keytab_path"`

	// ServicePrincipal is the Kerberos service principal name (SPN).
	// Format: service/hostname@REALM (e.g., nfs/server.example.com@EXAMPLE.COM)
	// Override: DITTOFS_KERBEROS_PRINCIPAL (primary), DITTOFS_KERBEROS_SERVICE_PRINCIPAL (compat)
	ServicePrincipal string `mapstructure:"service_principal" yaml:"service_principal"`

	// Krb5Conf is the path to the Kerberos configuration file.
	// Default: /etc/krb5.conf
	Krb5Conf string `mapstructure:"krb5_conf" yaml:"krb5_conf"`

	// MaxClockSkew is the maximum allowed clock difference between client and server.
	// Kerberos requires synchronized clocks; this tolerance handles minor drift.
	// Default: 5m
	MaxClockSkew time.Duration `mapstructure:"max_clock_skew" yaml:"max_clock_skew"`

	// ContextTTL is the maximum lifetime of an RPCSEC_GSS security context.
	// After this duration, clients must re-authenticate.
	// Default: 8h
	ContextTTL time.Duration `mapstructure:"context_ttl" yaml:"context_ttl"`

	// MaxContexts is the maximum number of concurrent RPCSEC_GSS contexts.
	// Prevents memory exhaustion from excessive context creation.
	// Default: 10000
	MaxContexts int `mapstructure:"max_contexts" yaml:"max_contexts"`

	// IdentityMapping configures how Kerberos principals are mapped to Unix identities.
	IdentityMapping IdentityMappingConfig `mapstructure:"identity_mapping" yaml:"identity_mapping"`
}

// IdentityMappingConfig controls how Kerberos principals are mapped to Unix UID/GID.
//
// The mapping strategy determines how authenticated Kerberos principals
// (e.g., "alice@EXAMPLE.COM") are converted to Unix identities for
// NFS file permission checks.
type IdentityMappingConfig struct {
	// Strategy selects the identity mapping approach.
	// Currently supported: "static" (map from config file)
	// Future: "ldap", "nsswitch", "regex"
	// Default: "static"
	Strategy string `mapstructure:"strategy" yaml:"strategy"`

	// StaticMap maps "principal@REALM" strings to Unix identities.
	// Only used when Strategy is "static".
	// Example: {"alice@EXAMPLE.COM": {UID: 1000, GID: 1000}}
	StaticMap map[string]StaticIdentity `mapstructure:"static_map" yaml:"static_map"`

	// DefaultUID is the Unix UID assigned to principals not found in StaticMap.
	// Default: 65534 (nobody)
	DefaultUID uint32 `mapstructure:"default_uid" yaml:"default_uid"`

	// DefaultGID is the Unix GID assigned to principals not found in StaticMap.
	// Default: 65534 (nogroup)
	DefaultGID uint32 `mapstructure:"default_gid" yaml:"default_gid"`
}

// StaticIdentity represents a Unix identity for a specific Kerberos principal.
type StaticIdentity struct {
	// UID is the Unix user ID
	UID uint32 `mapstructure:"uid" yaml:"uid"`

	// GID is the Unix primary group ID
	GID uint32 `mapstructure:"gid" yaml:"gid"`

	// GIDs is a list of supplementary group IDs
	GIDs []uint32 `mapstructure:"gids" yaml:"gids,omitempty"`
}

// Load loads configuration from file, environment, and defaults.
//
// Configuration precedence (highest to lowest):
//  1. Environment variables (DITTOFS_*)
//  2. Configuration file
//  3. Default values
//
// Parameters:
//   - configPath: Path to config file (empty string uses default location)
//
// Returns:
//   - *Config: Loaded and validated configuration
//   - error: Configuration loading or validation error
func Load(configPath string) (*Config, error) {
	v := viper.New()

	// Configure viper
	setupViper(v, configPath)

	// Read configuration file if it exists
	configFileFound, err := readConfigFile(v)
	if err != nil {
		return nil, err
	}

	// If no config file was found, use defaults
	if !configFileFound {
		cfg := GetDefaultConfig()
		return cfg, nil
	}

	// Unmarshal into config struct with custom decode hooks
	var cfg Config
	if err := v.Unmarshal(&cfg, viper.DecodeHook(configDecodeHooks())); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Apply defaults for any missing values
	ApplyDefaults(&cfg)

	// Validate configuration
	if err := Validate(&cfg); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	return &cfg, nil
}

// MustLoad loads configuration with helpful error messages.
// It checks if the config file exists and provides user-friendly instructions if not.
//
// Parameters:
//   - configPath: Path to config file (empty string uses default location)
//
// Returns:
//   - *Config: Loaded and validated configuration
//   - error: User-friendly error with instructions if config not found
func MustLoad(configPath string) (*Config, error) {
	// Determine config path
	if configPath == "" {
		if !DefaultConfigExists() {
			return nil, fmt.Errorf("no configuration file found at default location: %s\n\n"+
				"Please initialize a configuration file first:\n"+
				"  dittofs init\n\n"+
				"Or specify a custom config file:\n"+
				"  dittofs <command> --config /path/to/config.yaml",
				GetDefaultConfigPath())
		}
		configPath = GetDefaultConfigPath()
	} else {
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			return nil, fmt.Errorf("configuration file not found: %s\n\n"+
				"Please create the configuration file:\n"+
				"  dittofs init --config %s",
				configPath, configPath)
		}
	}

	// Load configuration
	cfg, err := Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration: %w", err)
	}

	return cfg, nil
}

// SaveConfig saves the configuration to the specified file path.
// The configuration is saved in YAML format using proper yaml tags.
func SaveConfig(cfg *Config, path string) error {
	// Create parent directory if it doesn't exist
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Use yaml.Marshal directly to respect yaml tags
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Write to file with restricted permissions (0600 = owner read/write only).
	// This is important because config files may contain sensitive data like password hashes.
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// setupViper configures viper with environment variables and config file settings.
func setupViper(v *viper.Viper, configPath string) {
	// Set up environment variable support
	// Environment variables use DITTOFS_ prefix and underscores
	// Example: DITTOFS_LOGGING_LEVEL=DEBUG
	v.SetEnvPrefix("DITTOFS")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Configure config file search
	if configPath != "" {
		// Use explicitly specified config file
		v.SetConfigFile(configPath)
	} else {
		// Use default location: $XDG_CONFIG_HOME/dittofs/config.{yaml,toml}
		configDir := getConfigDir()
		v.AddConfigPath(configDir)
		v.SetConfigName("config")
		v.SetConfigType("yaml") // Primary format
	}
}

// readConfigFile reads the configuration file if it exists.
// Returns (fileFound, error) where fileFound indicates if a config file was found.
func readConfigFile(v *viper.Viper) (bool, error) {
	if err := v.ReadInConfig(); err != nil {
		// Check if error is "config file not found"
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// Config file not found is acceptable - use defaults
			return false, nil
		}
		// Also check for os.PathError when explicit config file doesn't exist
		if os.IsNotExist(err) {
			// Config file not found is acceptable - use defaults
			return false, nil
		}
		// Other errors are problems
		return false, fmt.Errorf("failed to read config file: %w", err)
	}

	return true, nil
}

// configDecodeHooks returns a combined decode hook for all custom types.
// This includes ByteSize and time.Duration parsing.
func configDecodeHooks() mapstructure.DecodeHookFunc {
	return mapstructure.ComposeDecodeHookFunc(
		byteSizeDecodeHook(),
		durationDecodeHook(),
	)
}

// byteSizeDecodeHook returns a mapstructure decode hook that converts strings
// and integers to bytesize.ByteSize. This enables config files to use human-readable
// sizes like "1Gi", "500Mi", "100MB", or plain numbers.
func byteSizeDecodeHook() mapstructure.DecodeHookFunc {
	return func(from reflect.Type, to reflect.Type, data interface{}) (interface{}, error) {
		// Only handle conversion to ByteSize
		if to != reflect.TypeOf(bytesize.ByteSize(0)) {
			return data, nil
		}

		switch v := data.(type) {
		case string:
			// Parse human-readable string like "1Gi", "500Mi", "100MB"
			return bytesize.ParseByteSize(v)
		case int:
			return bytesize.ByteSize(v), nil
		case int64:
			return bytesize.ByteSize(v), nil
		case uint64:
			return bytesize.ByteSize(v), nil
		case float64:
			// YAML often deserializes numbers as float64
			return bytesize.ByteSize(v), nil
		default:
			return data, nil
		}
	}
}

// durationDecodeHook returns a mapstructure decode hook that converts strings
// to time.Duration. This enables config files to use human-readable durations
// like "30s", "5m", "1h".
func durationDecodeHook() mapstructure.DecodeHookFunc {
	return func(from reflect.Type, to reflect.Type, data interface{}) (interface{}, error) {
		// Only handle conversion to time.Duration
		if to != reflect.TypeOf(time.Duration(0)) {
			return data, nil
		}

		switch v := data.(type) {
		case string:
			// Parse duration string like "30s", "5m", "1h"
			return time.ParseDuration(v)
		case int:
			// Assume nanoseconds for raw integers
			return time.Duration(v), nil
		case int64:
			return time.Duration(v), nil
		case float64:
			// YAML often deserializes numbers as float64
			return time.Duration(v), nil
		default:
			return data, nil
		}
	}
}

// getConfigDir returns the configuration directory path.
//
// Uses XDG_CONFIG_HOME if set, otherwise ~/.config, or falls back to current
// directory (.) if home directory cannot be determined.
func getConfigDir() string {
	// Check XDG_CONFIG_HOME
	if xdgConfig := os.Getenv("XDG_CONFIG_HOME"); xdgConfig != "" {
		return filepath.Join(xdgConfig, "dittofs")
	}

	// Fall back to ~/.config
	home, err := os.UserHomeDir()
	if err != nil {
		// If we can't get home dir, use current directory as last resort
		return "."
	}

	return filepath.Join(home, ".config", "dittofs")
}

// GetDefaultConfigPath returns the default configuration file path.
func GetDefaultConfigPath() string {
	return filepath.Join(getConfigDir(), "config.yaml")
}

// DefaultConfigExists checks if a config file exists at the default location.
func DefaultConfigExists() bool {
	path := GetDefaultConfigPath()
	_, err := os.Stat(path)
	return err == nil
}

// GetConfigDir returns the configuration directory path (exposed for init command).
func GetConfigDir() string {
	return getConfigDir()
}
