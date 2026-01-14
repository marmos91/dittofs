package config

import (
	"os"
	"strings"
	"time"

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
	applyMetadataDefaults(&cfg.Metadata)
	applyContentDefaults(&cfg.Content)
	applyCacheDefaults(&cfg.Cache)
	applyBlockStoreDefaults(&cfg.BlockStore)
	applyFlusherDefaults(&cfg.Flusher)
	applyShareDefaults(cfg.Shares)
	applyAdaptersDefaults(&cfg.Adapters)

	// Note: No defaults for stores, shares, or adapters themselves
	// User must configure at least:
	// - One metadata store
	// - One content store
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
func applyCacheDefaults(cfg *CacheConfig) {
	// Default to in-memory cache
	if cfg.Type == "" {
		cfg.Type = "memory"
	}
	// MaxSize defaults to 0 (unlimited)
}

// applyPrefetchDefaults sets prefetch configuration defaults.
func applyPrefetchDefaults(cfg *PrefetchConfig) {
	// Enabled defaults to true
	if cfg.Enabled == nil {
		enabled := true
		cfg.Enabled = &enabled
	}

	// MaxFileSize defaults to 100MB
	if cfg.MaxFileSize == 0 {
		cfg.MaxFileSize = 100 * 1024 * 1024 // 100MB
	}

	// ChunkSize defaults to 512KB
	if cfg.ChunkSize == 0 {
		cfg.ChunkSize = 512 * 1024 // 512KB
	}
}

// applyFlusherDefaults sets background flusher configuration defaults.
func applyFlusherDefaults(cfg *FlusherConfig) {
	// SweepInterval defaults to 10 seconds
	if cfg.SweepInterval == 0 {
		cfg.SweepInterval = 10 * time.Second
	}

	// FlushTimeout defaults to 30 seconds
	if cfg.FlushTimeout == 0 {
		cfg.FlushTimeout = 30 * time.Second
	}

	// ParallelUploads defaults to 4
	if cfg.ParallelUploads == 0 {
		cfg.ParallelUploads = 4
	}

	// ParallelDownloads defaults to 4
	if cfg.ParallelDownloads == 0 {
		cfg.ParallelDownloads = 4
	}
}

// applyBlockStoreDefaults sets block store configuration defaults.
func applyBlockStoreDefaults(cfg *BlockStoreConfig) {
	// Type defaults to "" (disabled - cache-only mode)
	// No default type - user must explicitly enable block store

	// Apply S3-specific defaults if S3 is configured
	if cfg.Type == "s3" {
		// KeyPrefix defaults to "blocks/"
		if cfg.S3.KeyPrefix == "" {
			cfg.S3.KeyPrefix = "blocks/"
		}

		// MaxRetries defaults to 3
		if cfg.S3.MaxRetries == 0 {
			cfg.S3.MaxRetries = 3
		}
	}

	// Apply filesystem-specific defaults if filesystem is configured
	if cfg.Type == "filesystem" {
		// CreateDir defaults to true (create base directory if it doesn't exist)
		// Note: Go bool defaults to false, so we set it to true here
		// We can't distinguish "explicitly false" from "not set", so we always
		// set to true unless basePath is not configured (validation will catch that)
		if cfg.Filesystem.BasePath != "" && !cfg.Filesystem.CreateDir {
			cfg.Filesystem.CreateDir = true
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

// applyWriteGatheringDefaults sets write gathering optimization defaults.
//
// Write gathering is based on the Linux kernel's "wdelay" optimization (fs/nfsd/vfs.c).
// The 10ms delay matches the Linux kernel's wait_for_concurrent_writes() behavior.
func applyWriteGatheringDefaults(cfg *WriteGatheringConfig) {
	// Enabled defaults to true (optimization on by default)
	if cfg.Enabled == nil {
		enabled := true
		cfg.Enabled = &enabled
	}

	// GatherDelay defaults to 10ms (matches Linux kernel's 10ms delay)
	if cfg.GatherDelay == 0 {
		cfg.GatherDelay = 10 * time.Millisecond
	}

	// ActiveThreshold defaults to 10ms (same as GatherDelay for symmetry)
	if cfg.ActiveThreshold == 0 {
		cfg.ActiveThreshold = 10 * time.Millisecond
	}
}

// applyContentDefaults sets content store defaults.
func applyContentDefaults(cfg *ContentConfig) {
	// Initialize stores map if nil
	if cfg.Stores == nil {
		cfg.Stores = make(map[string]ContentStoreConfig)
	}

	// Apply defaults to each store
	for name, store := range cfg.Stores {
		// Initialize maps if nil
		if store.Filesystem == nil {
			store.Filesystem = make(map[string]any)
		}
		if store.Memory == nil {
			store.Memory = make(map[string]any)
		}
		if store.S3 == nil {
			store.S3 = make(map[string]any)
		}

		// Apply type-specific defaults
		switch store.Type {
		case "filesystem":
			if _, ok := store.Filesystem["path"]; !ok {
				store.Filesystem["path"] = "/tmp/dittofs-content"
			}
		case "memory":
			if _, ok := store.Memory["max_size_bytes"]; !ok {
				store.Memory["max_size_bytes"] = uint64(1073741824) // 1GB
			}
		}

		cfg.Stores[name] = store
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

	// Apply global settings defaults
	applyCapabilitiesDefaults(&cfg.Global.FilesystemCapabilities)
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
		cfg.Port = 445
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
	cfg := &Config{
		Logging: LoggingConfig{},
		Server:  ServerConfig{},
		Content: ContentConfig{
			Stores: map[string]ContentStoreConfig{
				"default": {
					Type:       "filesystem",
					Filesystem: map[string]any{"path": "/tmp/dittofs-content"},
				},
			},
		},
		Metadata: MetadataConfig{
			Global: MetadataGlobalConfig{
				FilesystemCapabilities: metadata.FilesystemCapabilities{
					SupportsHardLinks: true,
					SupportsSymlinks:  true,
					CaseSensitive:     true,
					CasePreserving:    true,
				},
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
				Name:          "/export",
				MetadataStore: "default",
				ContentStore:  "default",
				ReadOnly:      false,
				// Cache is empty by default (sync mode, no caching)
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
		},
	}

	ApplyDefaults(cfg)
	return cfg
}
