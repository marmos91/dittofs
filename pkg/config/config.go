package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marmos91/dittofs/pkg/adapter/nfs"
	"github.com/marmos91/dittofs/pkg/store/metadata"
	"github.com/spf13/viper"
)

// Config represents the complete DittoFS configuration.
//
// This structure captures all configurable aspects of the DittoFS server including:
//   - Logging configuration
//   - Server-wide settings
//   - Content store selection and configuration (store-specific)
//   - Metadata store selection and configuration (store-specific)
//   - Share/export definitions
//   - Protocol adapter configurations
//
// Configuration sources (in order of precedence):
//  1. CLI flags (highest priority)
//  2. Environment variables (DITTOFS_*)
//  3. Configuration file (YAML or TOML)
//  4. Default values (lowest priority)
//
// Store Configuration Pattern:
// Each store implementation defines its own configuration type and factory function.
// The Config struct contains type-specific sections (e.g., content.filesystem, content.memory)
// and only the section matching the selected type is used.
type Config struct {
	// Logging controls log output behavior
	Logging LoggingConfig `mapstructure:"logging"`

	// Server contains server-wide settings
	Server ServerConfig `mapstructure:"server"`

	// Content specifies the content store type and type-specific configuration
	Content ContentConfig `mapstructure:"content"`

	// Metadata specifies the metadata store type and type-specific configuration
	Metadata MetadataConfig `mapstructure:"metadata"`

	// Shares defines the list of shares/exports available to clients
	Shares []ShareConfig `mapstructure:"shares" validate:"dive"`

	// Adapters contains protocol adapter configurations
	Adapters AdaptersConfig `mapstructure:"adapters"`

	// GC contains garbage collection configuration
	GC GCConfig `mapstructure:"gc"`
}

// LoggingConfig controls logging behavior.
type LoggingConfig struct {
	// Level is the minimum log level to output
	// Valid values: DEBUG, INFO, WARN, ERROR (case-insensitive, normalized to uppercase)
	Level string `mapstructure:"level" validate:"required,oneof=DEBUG INFO WARN ERROR debug info warn error"`

	// Format specifies the log output format
	// Valid values: text, json
	Format string `mapstructure:"format" validate:"required,oneof=text json"`

	// Output specifies where logs are written
	// Valid values: stdout, stderr, or a file path
	Output string `mapstructure:"output" validate:"required"`
}

// ServerConfig contains server-wide settings.
type ServerConfig struct {
	// ShutdownTimeout is the maximum time to wait for graceful shutdown
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout" validate:"required,gt=0"`

	// Metrics contains Prometheus metrics server configuration
	Metrics MetricsConfig `mapstructure:"metrics"`
}

// MetricsConfig configures the Prometheus metrics HTTP server.
// When Enabled is false, no metrics are collected (zero overhead).
type MetricsConfig struct {
	// Enabled controls whether metrics collection and HTTP server are enabled
	Enabled bool `mapstructure:"enabled"`

	// Port is the HTTP port for the metrics endpoint
	// Default: 9090
	Port int `mapstructure:"port" validate:"omitempty,min=1,max=65535"`
}

// ContentConfig specifies content store configuration with named stores.
type ContentConfig struct {
	// Global settings that apply to all content stores
	Global ContentGlobalConfig `mapstructure:"global"`

	// Named content store instances
	Stores map[string]ContentStoreConfig `mapstructure:"stores"`
}

// ContentGlobalConfig contains global settings for all content stores.
type ContentGlobalConfig struct {
	// Future: cache settings, compression, encryption
}

// ContentStoreConfig defines a single content store instance.
type ContentStoreConfig struct {
	// Type specifies which content store implementation to use
	// Valid values: filesystem, memory, s3
	Type string `mapstructure:"type" validate:"required,oneof=filesystem memory s3"`

	// Filesystem contains filesystem-specific configuration
	// Only used when Type = "filesystem"
	Filesystem map[string]any `mapstructure:"filesystem"`

	// Memory contains memory-specific configuration
	// Only used when Type = "memory"
	Memory map[string]any `mapstructure:"memory"`

	// S3 contains S3-specific configuration
	// Only used when Type = "s3"
	S3 map[string]any `mapstructure:"s3"`
}

// MetadataConfig specifies metadata store configuration with named stores.
type MetadataConfig struct {
	// Global settings that apply to all metadata stores
	Global MetadataGlobalConfig `mapstructure:"global"`

	// Named metadata store instances
	Stores map[string]MetadataStoreConfig `mapstructure:"stores"`
}

// MetadataGlobalConfig contains global settings for all metadata stores.
type MetadataGlobalConfig struct {
	// FilesystemCapabilities defines filesystem capabilities and limits
	FilesystemCapabilities metadata.FilesystemCapabilities `mapstructure:"filesystem_capabilities"`
}

// MetadataStoreConfig defines a single metadata store instance.
type MetadataStoreConfig struct {
	// Type specifies which metadata store implementation to use
	// Valid values: memory, badger
	Type string `mapstructure:"type" validate:"required,oneof=memory badger"`

	// Memory contains memory-specific configuration
	// Only used when Type = "memory"
	Memory map[string]any `mapstructure:"memory"`

	// Badger contains BadgerDB-specific configuration
	// Only used when Type = "badger"
	Badger map[string]any `mapstructure:"badger"`
}

// ShareConfig defines a single share/export.
type ShareConfig struct {
	// Name is the share path (e.g., "/export")
	Name string `mapstructure:"name" validate:"required,startswith=/"`

	// MetadataStore is the name of the metadata store to use for this share
	MetadataStore string `mapstructure:"metadata_store" validate:"required"`

	// ContentStore is the name of the content store to use for this share
	ContentStore string `mapstructure:"content_store" validate:"required"`

	// ReadOnly makes the share read-only if true
	ReadOnly bool `mapstructure:"read_only"`

	// Async allows asynchronous writes if true
	Async bool `mapstructure:"async"`

	// AllowedClients lists IP addresses or CIDR ranges allowed to access
	// Empty list means all clients are allowed
	AllowedClients []string `mapstructure:"allowed_clients"`

	// DeniedClients lists IP addresses or CIDR ranges explicitly denied
	// Takes precedence over AllowedClients
	DeniedClients []string `mapstructure:"denied_clients"`

	// RequireAuth requires authentication if true
	RequireAuth bool `mapstructure:"require_auth"`

	// AllowedAuthMethods lists allowed authentication methods
	// Valid values: anonymous, unix
	AllowedAuthMethods []string `mapstructure:"allowed_auth_methods" validate:"dive,oneof=anonymous unix"`

	// IdentityMapping configures user/group mapping
	IdentityMapping IdentityMappingConfig `mapstructure:"identity_mapping" validate:"required"`

	// RootDirectoryAttributes specifies attributes for the share root directory
	// These define the permissions and ownership of the export root
	RootDirectoryAttributes RootDirectoryAttributesConfig `mapstructure:"root_directory_attributes" validate:"required"`

	// DumpRestricted restricts DUMP operations to allowed clients only
	// DUMP is a mount protocol operation that lists active mounts for this share
	DumpRestricted bool `mapstructure:"dump_restricted"`

	// DumpAllowedClients lists IP addresses or CIDR ranges allowed to use DUMP
	// Only used if DumpRestricted is true
	// Empty list with DumpRestricted=true means no clients can use DUMP
	DumpAllowedClients []string `mapstructure:"dump_allowed_clients"`
}

// IdentityMappingConfig controls user/group identity mapping.
type IdentityMappingConfig struct {
	// MapAllToAnonymous maps all users to anonymous (all_squash)
	MapAllToAnonymous bool `mapstructure:"map_all_to_anonymous"`

	// MapPrivilegedToAnonymous maps root user to anonymous (root_squash)
	MapPrivilegedToAnonymous bool `mapstructure:"map_privileged_to_anonymous"`

	// AnonymousUID is the UID to use for anonymous users
	AnonymousUID uint32 `mapstructure:"anonymous_uid"`

	// AnonymousGID is the GID to use for anonymous users
	AnonymousGID uint32 `mapstructure:"anonymous_gid"`
}

// RootDirectoryAttributesConfig specifies the attributes for a share's root directory.
// These attributes define the permissions and ownership of the export root.
type RootDirectoryAttributesConfig struct {
	// Mode is the Unix permission mode (e.g., 0755)
	Mode uint32 `mapstructure:"mode" validate:"lte=511"` // 511 = 0777 in decimal

	// UID is the owner user ID
	UID uint32 `mapstructure:"uid"`

	// GID is the owner group ID
	GID uint32 `mapstructure:"gid"`
}

// AdaptersConfig contains all protocol adapter configurations.
type AdaptersConfig struct {
	// NFS contains NFS protocol configuration.
	// Uses the nfs.NFSConfig type directly to avoid duplication.
	NFS nfs.NFSConfig `mapstructure:"nfs"`
}

// GCConfig contains garbage collection configuration.
//
// Garbage collection identifies and removes content that is no longer
// referenced by any metadata (orphaned content). This can occur due to:
//   - Server crashes during delete operations
//   - Failed delete operations
//   - Buffered deletions lost in memory
//
// The collector is generic and works with any MetadataStore and ContentStore
// implementation that supports the required interfaces.
type GCConfig struct {
	// Enabled controls whether garbage collection is active (default: false)
	Enabled bool `mapstructure:"enabled"`

	// Interval is how often to run garbage collection (default: 24h)
	// Format: duration string (e.g., "24h", "12h", "30m")
	Interval time.Duration `mapstructure:"interval"`

	// Timeout is the maximum duration for a single GC run (default: 10m)
	// For large deployments with millions of files, increase this value.
	// Format: duration string (e.g., "10m", "30m", "1h")
	Timeout time.Duration `mapstructure:"timeout"`

	// BatchSize is how many orphaned items to delete per batch (default: 1000)
	// S3 supports up to 1000 objects per DeleteObjects call
	BatchSize int `mapstructure:"batch_size"`

	// DryRun mode logs what would be deleted without actually deleting (default: false)
	// Useful for testing and validation
	DryRun bool `mapstructure:"dry_run"`
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

	// Unmarshal into config struct
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
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
