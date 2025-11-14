package config

import (
	"time"

	"github.com/marmos91/dittofs/pkg/adapter/nfs"
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
	applyServerDefaults(&cfg.Server)
	applyContentDefaults(&cfg.Content)
	applyMetadataDefaults(&cfg.Metadata)
	applyShareDefaults(cfg.Shares)
	applyAdaptersDefaults(&cfg.Adapters)
}

// applyLoggingDefaults sets logging defaults.
func applyLoggingDefaults(cfg *LoggingConfig) {
	if cfg.Level == "" {
		cfg.Level = "INFO"
	}
	if cfg.Format == "" {
		cfg.Format = "text"
	}
	if cfg.Output == "" {
		cfg.Output = "stdout"
	}
}

// applyServerDefaults sets server defaults.
func applyServerDefaults(cfg *ServerConfig) {
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 30 * time.Second
	}
}

// applyContentDefaults sets content store defaults.
func applyContentDefaults(cfg *ContentConfig) {
	if cfg.Type == "" {
		cfg.Type = "filesystem"
	}

	// Initialize maps if nil
	if cfg.Filesystem == nil {
		cfg.Filesystem = make(map[string]any)
	}
	if cfg.Memory == nil {
		cfg.Memory = make(map[string]any)
	}

	// Apply defaults for all store types (for config file generation)
	if _, ok := cfg.Filesystem["path"]; !ok {
		cfg.Filesystem["path"] = "/tmp/dittofs-content"
	}
	if _, ok := cfg.Memory["max_size_bytes"]; !ok {
		cfg.Memory["max_size_bytes"] = uint64(1073741824) // 1GB
	}
}

// applyMetadataDefaults sets metadata store defaults.
func applyMetadataDefaults(cfg *MetadataConfig) {
	if cfg.Type == "" {
		cfg.Type = "memory"
	}

	// Initialize maps if nil
	if cfg.Memory == nil {
		cfg.Memory = make(map[string]any)
	}

	// Apply filesystem capabilities defaults
	applyCapabilitiesDefaults(&cfg.Capabilities)

	// DumpRestricted defaults to false
	// DumpAllowedClients defaults to empty list
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

	// Boolean defaults - only set if they're false (zero value)
	// These default to true
	if !cfg.SupportsHardLinks {
		cfg.SupportsHardLinks = true
	}
	if !cfg.SupportsSymlinks {
		cfg.SupportsSymlinks = true
	}
	if !cfg.CaseSensitive {
		cfg.CaseSensitive = true
	}
	if !cfg.CasePreserving {
		cfg.CasePreserving = true
	}
}

// applyShareDefaults sets share defaults.
func applyShareDefaults(shares []ShareConfig) {
	for i := range shares {
		share := &shares[i]

		// ReadOnly defaults to false
		// Async defaults to false (sync writes by default)

		// If AllowedClients is nil, initialize to empty (all allowed)
		if share.AllowedClients == nil {
			share.AllowedClients = []string{}
		}

		// If DeniedClients is nil, initialize to empty (none denied)
		if share.DeniedClients == nil {
			share.DeniedClients = []string{}
		}

		// RequireAuth defaults to false

		// If AllowedAuthMethods is nil or empty, default to both methods
		if share.AllowedAuthMethods == nil || len(share.AllowedAuthMethods) == 0 {
			share.AllowedAuthMethods = []string{"anonymous", "unix"}
		}

		// Apply identity mapping defaults
		applyIdentityMappingDefaults(&share.IdentityMapping)

		// Apply root attr defaults
		applyRootAttrDefaults(&share.RootAttr)
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

// applyRootAttrDefaults sets root directory attribute defaults.
func applyRootAttrDefaults(cfg *RootAttrConfig) {
	if cfg.Mode == 0 {
		cfg.Mode = 0755
	}
	// UID and GID default to 0 (root) if not specified
	// This is acceptable since these are the root directory attributes
}

// applyAdaptersDefaults sets adapter defaults.
func applyAdaptersDefaults(cfg *AdaptersConfig) {
	applyNFSDefaults(&cfg.NFS)
}

// applyNFSDefaults sets NFS adapter defaults.
func applyNFSDefaults(cfg *nfs.NFSConfig) {
	// Enabled defaults to true
	if !cfg.Enabled {
		cfg.Enabled = true
	}

	if cfg.Port == 0 {
		cfg.Port = 2049
	}

	// MaxConnections defaults to 0 (unlimited)

	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 5 * time.Minute
	}

	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 30 * time.Second
	}

	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 5 * time.Minute
	}

	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 30 * time.Second
	}

	if cfg.MetricsLogInterval == 0 {
		cfg.MetricsLogInterval = 5 * time.Minute
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
			Filesystem: make(map[string]any),
			Memory:     make(map[string]any),
		},
		Metadata: MetadataConfig{
			Memory: make(map[string]any),
		},
		Shares: []ShareConfig{
			{
				Name:     "/export",
				ReadOnly: false,
				Async:    true,
				IdentityMapping: IdentityMappingConfig{
					MapAllToAnonymous: true,
				},
			},
		},
		Adapters: AdaptersConfig{},
	}

	ApplyDefaults(cfg)
	return cfg
}
