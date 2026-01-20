package config

import (
	"os"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/pkg/adapter/nfs"
	"github.com/marmos91/dittofs/pkg/adapter/smb"
	"github.com/marmos91/dittofs/pkg/api"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ApplyDefaults sets default values for any unspecified configuration fields.
//
// This function is called after loading configuration from file and environment
// variables to fill in any missing values with sensible defaults.
//
// Default Strategy:
//   - Zero values (0, "", false, nil) are replaced with defaults
//   - Explicit values are preserved
//   - Store-specific defaults are handled by store implementations
func ApplyDefaults(cfg *Config) {
	applyLoggingDefaults(&cfg.Logging)
	applyTelemetryDefaults(&cfg.Telemetry)
	applyServerDefaults(&cfg.Server)
	applyIdentityDefaults(&cfg.Identity)
	applyMetadataDefaults(&cfg.Metadata)
	applyCacheDefaults(&cfg.Cache)
	applyPayloadDefaults(&cfg.Payload)
	applyShareDefaults(cfg.Shares)
	applyAdaptersDefaults(&cfg.Adapters)

	// Note: No defaults for stores, shares, or adapters themselves
	// User must configure at least:
	// - One metadata store
	// - One payload store (unless using memory only)
	// - One share
	// - One adapter
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

	// Default to insecure for local development
	// Note: Insecure defaults to true only if telemetry is enabled and not explicitly set
	// Since bool zero value is false, we need to handle this differently if we want true default
	// For safety, we leave Insecure as false by default (require TLS)
	// Users must explicitly set insecure: true for non-TLS connections

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

// applyServerDefaults sets server defaults.
func applyServerDefaults(cfg *ServerConfig) {
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 30 * time.Second
	}

	// Apply metrics defaults
	applyMetricsDefaults(&cfg.Metrics)

	// Apply API defaults
	applyAPIDefaults(&cfg.API)
}

// applyIdentityDefaults sets identity store defaults.
func applyIdentityDefaults(cfg *IdentityStoreConfig) {
	// Default to memory store (for testing/development)
	// Production deployments should use sqlite, badger, or postgres
	if cfg.Type == "" {
		cfg.Type = "memory"
	}
}

// applyMetricsDefaults sets metrics defaults.
func applyMetricsDefaults(cfg *MetricsConfig) {
	// Enabled defaults to false (opt-in for metrics)
	// Port defaults to 9090 if metrics are enabled
	if cfg.Enabled && cfg.Port == 0 {
		cfg.Port = 9090
	}
}

// applyAPIDefaults sets API server defaults.
func applyAPIDefaults(cfg *api.APIConfig) {
	// API server is enabled by default
	// nil means "not set" -> default to true
	if cfg.Enabled == nil {
		enabled := true
		cfg.Enabled = &enabled
	}

	// Port defaults to 8080
	if cfg.Port == 0 {
		cfg.Port = 8080
	}

	// Timeout defaults
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

// applyPayloadDefaults sets payload configuration defaults.
func applyPayloadDefaults(cfg *PayloadConfig) {
	// Initialize stores map if nil
	if cfg.Stores == nil {
		cfg.Stores = make(map[string]PayloadStoreConfig)
	}

	// Apply defaults to each store
	for name, storeCfg := range cfg.Stores {
		applyPayloadStoreDefaults(&storeCfg)
		cfg.Stores[name] = storeCfg
	}

	// Apply transfer defaults
	applyTransferDefaults(&cfg.Transfer)
}

// applyPayloadStoreDefaults sets defaults for individual payload stores.
func applyPayloadStoreDefaults(cfg *PayloadStoreConfig) {
	// Apply S3-specific defaults if S3 is configured
	if cfg.Type == "s3" && cfg.S3 != nil {
		// Prefix defaults to "blocks/"
		if cfg.S3.Prefix == "" {
			cfg.S3.Prefix = "blocks/"
		}
		// MaxRetries defaults to 3
		if cfg.S3.MaxRetries == 0 {
			cfg.S3.MaxRetries = 3
		}
	}

	// Apply filesystem-specific defaults if filesystem is configured
	if cfg.Type == "filesystem" && cfg.Filesystem != nil {
		// CreateDir defaults to true
		if cfg.Filesystem.CreateDir == nil {
			createDir := true
			cfg.Filesystem.CreateDir = &createDir
		}
		// DirMode defaults to 0755
		if cfg.Filesystem.DirMode == 0 {
			cfg.Filesystem.DirMode = 0755
		}
		// FileMode defaults to 0644
		if cfg.Filesystem.FileMode == 0 {
			cfg.Filesystem.FileMode = 0644
		}
	}
}

// applyTransferDefaults sets transfer manager defaults.
func applyTransferDefaults(cfg *TransferConfig) {
	// Uploads defaults to 16 (reasonable for S3 concurrent operations)
	if cfg.Workers.Uploads == 0 {
		cfg.Workers.Uploads = 16
	}
	// Downloads defaults to 16 (reasonable for S3 concurrent operations)
	if cfg.Workers.Downloads == 0 {
		cfg.Workers.Downloads = 16
	}
}

// applyMetadataDefaults sets metadata store defaults.
func applyMetadataDefaults(cfg *MetadataConfig) {
	// Initialize stores map if nil
	if cfg.Stores == nil {
		cfg.Stores = make(map[string]MetadataStoreConfig)
	}

	// Apply defaults to each store
	for name, store := range cfg.Stores {
		// Initialize maps if nil
		if store.Memory == nil {
			store.Memory = make(map[string]any)
		}
		if store.Badger == nil {
			store.Badger = make(map[string]any)
		}

		// Apply type-specific defaults
		switch store.Type {
		case "badger":
			if _, ok := store.Badger["db_path"]; !ok {
				store.Badger["db_path"] = "/tmp/dittofs-metadata"
			}
		}

		cfg.Stores[name] = store
	}

	// Apply filesystem capabilities defaults
	applyCapabilitiesDefaults(&cfg.FilesystemCapabilities)
}

// applyCapabilitiesDefaults sets filesystem capabilities defaults.
func applyCapabilitiesDefaults(cfg *metadata.FilesystemCapabilities) {
	if cfg.MaxReadSize == 0 {
		cfg.MaxReadSize = 1048576 // 1MB
	}
	if cfg.PreferredReadSize == 0 {
		cfg.PreferredReadSize = 65536 // 64KB
	}
	if cfg.MaxWriteSize == 0 {
		cfg.MaxWriteSize = 1048576 // 1MB
	}
	if cfg.PreferredWriteSize == 0 {
		cfg.PreferredWriteSize = 65536 // 64KB
	}
	if cfg.MaxFileSize == 0 {
		cfg.MaxFileSize = 9223372036854775807 // 2^63-1
	}
	if cfg.MaxFilenameLen == 0 {
		cfg.MaxFilenameLen = 255
	}
	if cfg.MaxPathLen == 0 {
		cfg.MaxPathLen = 4096
	}
	if cfg.MaxHardLinkCount == 0 {
		cfg.MaxHardLinkCount = 32767
	}
	if cfg.TimestampResolution == 0 {
		cfg.TimestampResolution = 1 // 1 nanosecond (Go time.Time precision)
	}

	// Note: Boolean capability fields (SupportsHardLinks, SupportsSymlinks, etc.)
	// default to false (zero value). This allows users to explicitly disable features.
	// GetDefaultConfig() sets these to true for the default configuration.
}

// applyShareDefaults sets share defaults.
func applyShareDefaults(shares []ShareConfig) {
	for i := range shares {
		share := &shares[i]

		// ReadOnly defaults to false
		// Cache defaults to empty string (sync mode, no caching)

		// If AllowedClients is nil, initialize to empty (all allowed)
		if share.AllowedClients == nil {
			share.AllowedClients = []string{}
		}

		// If DeniedClients is nil, initialize to empty (none denied)
		if share.DeniedClients == nil {
			share.DeniedClients = []string{}
		}

		// RequireAuth defaults to false

		// If AllowedAuthMethods is empty, default to both methods
		if len(share.AllowedAuthMethods) == 0 {
			share.AllowedAuthMethods = []string{"anonymous", "unix"}
		}

		// Apply identity mapping defaults
		applyIdentityMappingDefaults(&share.IdentityMapping)

		// Apply root directory attribute defaults
		applyRootDirectoryAttributesDefaults(&share.RootDirectoryAttributes)

		// DumpRestricted defaults to false
		// DumpAllowedClients defaults to empty list (no restrictions)
		if share.DumpAllowedClients == nil {
			share.DumpAllowedClients = []string{}
		}
	}
}

// applyIdentityMappingDefaults sets identity mapping defaults.
func applyIdentityMappingDefaults(cfg *IdentityMappingConfig) {
	// MapAllToAnonymous defaults to false
	// MapPrivilegedToAnonymous defaults to false

	// Anonymous user defaults (nobody/nogroup)
	if cfg.AnonymousUID == 0 {
		cfg.AnonymousUID = 65534
	}
	if cfg.AnonymousGID == 0 {
		cfg.AnonymousGID = 65534
	}
}

// applyRootDirectoryAttributesDefaults sets root directory attribute defaults.
func applyRootDirectoryAttributesDefaults(cfg *RootDirectoryAttributesConfig) {
	if cfg.Mode == 0 {
		cfg.Mode = 0755
	}

	// UID and GID default to current user if not specified
	// This makes the default config more user-friendly and avoids permission issues
	if cfg.UID == 0 && cfg.GID == 0 {
		// Get current user's UID
		uid := uint32(os.Getuid())
		gid := uint32(os.Getgid())

		// Only use current user if not running as root
		// If running as root, keep 0:0 as that's probably intentional
		if uid != 0 {
			cfg.UID = uid
			cfg.GID = gid
		}
	}
}

// applyAdaptersDefaults sets adapter defaults.
func applyAdaptersDefaults(cfg *AdaptersConfig) {
	// Enable NFS adapter by default if no adapters are configured
	// This ensures that a freshly loaded config (with no config file) will have
	// at least one adapter enabled and pass validation.
	// Users can explicitly set enabled: false in their config to disable it.
	if !cfg.NFS.Enabled {
		// Check if this looks like a default/unconfigured state
		// (Port is 0, meaning no explicit configuration was provided)
		if cfg.NFS.Port == 0 {
			cfg.NFS.Enabled = true
		}
	}

	applyNFSDefaults(&cfg.NFS)
	applySMBDefaults(&cfg.SMB)
}

// applyNFSDefaults sets NFS adapter defaults.
func applyNFSDefaults(cfg *nfs.NFSConfig) {
	// Note: Port and timeout defaults are always applied.
	// Enabled is set to true in applyAdaptersDefaults if not explicitly configured.

	if cfg.Port == 0 {
		cfg.Port = 2049
	}

	// MaxConnections defaults to 0 (unlimited)

	// Apply timeout defaults
	if cfg.Timeouts.Read == 0 {
		cfg.Timeouts.Read = 5 * time.Minute
	}

	if cfg.Timeouts.Write == 0 {
		cfg.Timeouts.Write = 30 * time.Second
	}

	if cfg.Timeouts.Idle == 0 {
		cfg.Timeouts.Idle = 5 * time.Minute
	}

	if cfg.Timeouts.Shutdown == 0 {
		cfg.Timeouts.Shutdown = 30 * time.Second
	}

	if cfg.MetricsLogInterval == 0 {
		cfg.MetricsLogInterval = 5 * time.Minute
	}
}

// applySMBDefaults sets SMB adapter defaults.
func applySMBDefaults(cfg *smb.SMBConfig) {
	// Note: SMB adapter is NOT enabled by default.
	// Users must explicitly enable it in their config.

	if cfg.Port == 0 {
		cfg.Port = 12445
	}

	// MaxConnections defaults to 0 (unlimited)

	// Apply timeout defaults
	if cfg.Timeouts.Read == 0 {
		cfg.Timeouts.Read = 5 * time.Minute
	}

	if cfg.Timeouts.Write == 0 {
		cfg.Timeouts.Write = 30 * time.Second
	}

	if cfg.Timeouts.Idle == 0 {
		cfg.Timeouts.Idle = 5 * time.Minute
	}

	if cfg.Timeouts.Shutdown == 0 {
		cfg.Timeouts.Shutdown = 30 * time.Second
	}

	if cfg.MetricsLogInterval == 0 {
		cfg.MetricsLogInterval = 5 * time.Minute
	}

	// Apply credit configuration defaults
	applySMBCreditsDefaults(&cfg.Credits)
}

// applySMBCreditsDefaults sets SMB credit configuration defaults.
func applySMBCreditsDefaults(cfg *smb.SMBCreditsConfig) {
	if cfg.Strategy == "" {
		cfg.Strategy = "adaptive"
	}
	if cfg.MinGrant == 0 {
		cfg.MinGrant = 16
	}
	if cfg.MaxGrant == 0 {
		cfg.MaxGrant = 8192
	}
	if cfg.InitialGrant == 0 {
		cfg.InitialGrant = 256
	}
	if cfg.MaxSessionCredits == 0 {
		cfg.MaxSessionCredits = 65535
	}
	if cfg.LoadThresholdHigh == 0 {
		cfg.LoadThresholdHigh = 1000
	}
	if cfg.LoadThresholdLow == 0 {
		cfg.LoadThresholdLow = 100
	}
	if cfg.AggressiveClientThreshold == 0 {
		cfg.AggressiveClientThreshold = 256
	}
}

// GetDefaultConfig returns a Config struct with all default values applied.
//
// This is useful for:
//   - Generating sample configuration files
//   - Testing
//   - Documentation
func GetDefaultConfig() *Config {
	createDir := true
	cfg := &Config{
		Logging: LoggingConfig{},
		Server:  ServerConfig{},
		Identity: IdentityStoreConfig{
			Type: "memory", // Default to memory for testing; production should use sqlite/postgres
		},
		Cache: CacheConfig{
			Path: "/tmp/dittofs-cache",
			Size: bytesize.ByteSize(bytesize.GiB), // 1 GiB
		},
		Payload: PayloadConfig{
			Stores: map[string]PayloadStoreConfig{
				"default": {
					Type: "filesystem",
					Filesystem: &PayloadFSConfig{
						BasePath:  "/tmp/dittofs-blocks",
						CreateDir: &createDir,
						DirMode:   0755,
						FileMode:  0644,
					},
				},
			},
			Transfer: TransferConfig{
				Workers: TransferWorkersConfig{
					Uploads:   16,
					Downloads: 16,
				},
			},
		},
		Metadata: MetadataConfig{
			FilesystemCapabilities: metadata.FilesystemCapabilities{
				SupportsHardLinks: true,
				SupportsSymlinks:  true,
				CaseSensitive:     true,
				CasePreserving:    true,
			},
			Stores: map[string]MetadataStoreConfig{
				"default": {
					Type:   "badger",
					Badger: map[string]any{"db_path": "/tmp/dittofs-metadata"},
				},
			},
		},
		Shares: []ShareConfig{
			{
				Name:     "/export",
				Metadata: "default",
				Payload:  "default",
				ReadOnly: false,
				IdentityMapping: IdentityMappingConfig{
					MapAllToAnonymous:        false, // Don't squash by default
					MapPrivilegedToAnonymous: false, // root_squash disabled by default
					AnonymousUID:             65534, // nobody
					AnonymousGID:             65534, // nogroup
				},
			},
		},
		Adapters: AdaptersConfig{
			NFS: nfs.NFSConfig{
				Enabled: true, // NFS adapter enabled by default
			},
			SMB: smb.SMBConfig{
				Enabled: true, // SMB adapter enabled by default
			},
		},
	}

	ApplyDefaults(cfg)
	return cfg
}
