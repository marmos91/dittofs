package config

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/content"
	contentFs "github.com/marmos91/dittofs/pkg/content/fs"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/memory"
	"github.com/mitchellh/mapstructure"
)

// CreateContentStore creates a content store based on configuration.
//
// This factory function uses the Type field to determine which store implementation
// to create, then decodes the type-specific configuration from the corresponding
// map and passes it to the store's constructor.
//
// Supported types:
//   - "filesystem": Uses pkg/content/fs (local filesystem storage)
//   - "memory": Uses in-memory storage (future implementation)
//
// Parameters:
//   - ctx: Context for initialization operations
//   - cfg: Content store configuration
//
// Returns:
//   - content.ContentStore: Initialized content store (as WritableContentStore)
//   - error: Configuration or initialization error
func CreateContentStore(ctx context.Context, cfg *ContentConfig) (content.WritableContentStore, error) {
	switch cfg.Type {
	case "filesystem":
		return createFilesystemContentStore(ctx, cfg.Filesystem)
	case "memory":
		return nil, fmt.Errorf("memory content store not yet implemented")
	default:
		return nil, fmt.Errorf("unknown content store type: %q", cfg.Type)
	}
}

// createFilesystemContentStore creates a filesystem-based content store.
func createFilesystemContentStore(ctx context.Context, options map[string]any) (content.WritableContentStore, error) {
	// Define the configuration struct for filesystem content store
	type FilesystemContentStoreConfig struct {
		Path string `mapstructure:"path"`
	}

	// Decode the options into the config struct
	var storeCfg FilesystemContentStoreConfig
	if err := mapstructure.Decode(options, &storeCfg); err != nil {
		return nil, fmt.Errorf("failed to decode filesystem content store config: %w", err)
	}

	// Validate required fields
	if storeCfg.Path == "" {
		return nil, fmt.Errorf("filesystem content store: path is required")
	}

	// Create the store
	store, err := contentFs.NewFSContentStore(ctx, storeCfg.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to create filesystem content store: %w", err)
	}

	return store, nil
}

// CreateMetadataStore creates a metadata store based on configuration.
//
// This factory function uses the Type field to determine which store implementation
// to create, then decodes the type-specific configuration from the corresponding
// map and passes it to the store's constructor.
//
// Supported types:
//   - "memory": Uses pkg/metadata/memory (in-memory storage)
//
// Parameters:
//   - ctx: Context for initialization operations
//   - cfg: Metadata store configuration
//
// Returns:
//   - metadata.MetadataStore: Initialized metadata store
//   - error: Configuration or initialization error
func CreateMetadataStore(ctx context.Context, cfg *MetadataConfig) (metadata.MetadataStore, error) {
	switch cfg.Type {
	case "memory":
		return createMemoryMetadataStore(ctx, cfg.Memory, &cfg.Capabilities)
	default:
		return nil, fmt.Errorf("unknown metadata store type: %q", cfg.Type)
	}
}

// createMemoryMetadataStore creates an in-memory metadata store.
func createMemoryMetadataStore(ctx context.Context, options map[string]any, capabilities *metadata.FilesystemCapabilities) (metadata.MetadataStore, error) {
	// Check context before creating store
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Decode store-specific options
	type MemoryMetadataStoreOptions struct {
		MaxStorageBytes uint64 `mapstructure:"max_storage_bytes"`
		MaxFiles        uint64 `mapstructure:"max_files"`
	}

	var storeOpts MemoryMetadataStoreOptions
	if err := mapstructure.Decode(options, &storeOpts); err != nil {
		return nil, fmt.Errorf("failed to decode memory metadata store options: %w", err)
	}

	// Create store config with capabilities directly (no conversion needed)
	storeConfig := memory.MemoryMetadataStoreConfig{
		Capabilities:    *capabilities,
		MaxStorageBytes: storeOpts.MaxStorageBytes,
		MaxFiles:        storeOpts.MaxFiles,
	}

	store := memory.NewMemoryMetadataStore(storeConfig)
	return store, nil
}

// CreateShares creates and adds shares to the metadata store based on configuration.
//
// This function:
//  1. Creates the root directory attributes for each share
//  2. Adds the share to the metadata store
//  3. Returns the metadata store with all shares configured
//
// Parameters:
//   - ctx: Context for share creation operations
//   - store: Metadata store to add shares to
//   - shares: List of share configurations
//
// Returns:
//   - error: Share creation error
func CreateShares(ctx context.Context, store metadata.MetadataStore, shares []ShareConfig) error {
	for i, shareCfg := range shares {
		// Convert config to metadata.ShareOptions
		options := metadata.ShareOptions{
			ReadOnly:           shareCfg.ReadOnly,
			Async:              shareCfg.Async,
			AllowedClients:     shareCfg.AllowedClients,
			DeniedClients:      shareCfg.DeniedClients,
			RequireAuth:        shareCfg.RequireAuth,
			AllowedAuthMethods: shareCfg.AllowedAuthMethods,
			IdentityMapping: &metadata.IdentityMapping{
				MapAllToAnonymous:         shareCfg.IdentityMapping.MapAllToAnonymous,
				MapPrivilegedToAnonymous:  shareCfg.IdentityMapping.MapPrivilegedToAnonymous,
				AnonymousUID:              &shareCfg.IdentityMapping.AnonymousUID,
				AnonymousGID:              &shareCfg.IdentityMapping.AnonymousGID,
			},
		}

		// Create root directory attributes
		rootAttr := &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: shareCfg.RootAttr.Mode,
			UID:  shareCfg.RootAttr.UID,
			GID:  shareCfg.RootAttr.GID,
			Size: 4096, // Standard directory size
		}

		// Add the share
		if err := store.AddShare(ctx, shareCfg.Name, options, rootAttr); err != nil {
			return fmt.Errorf("failed to add share[%d] %q: %w", i, shareCfg.Name, err)
		}
	}

	return nil
}

// ConfigureMetadataStore applies metadata-specific server configuration.
//
// This sets up server-level metadata settings like DUMP access restrictions.
//
// Parameters:
//   - ctx: Context for configuration operations
//   - store: Metadata store to configure
//   - cfg: Metadata configuration
//
// Returns:
//   - error: Configuration error
func ConfigureMetadataStore(ctx context.Context, store metadata.MetadataStore, cfg *MetadataConfig) error {
	serverConfig := metadata.MetadataServerConfig{
		CustomSettings: make(map[string]any),
	}

	// Configure DUMP restrictions
	if cfg.DumpRestricted {
		serverConfig.CustomSettings["nfs.mount.dump_allowed_clients"] = cfg.DumpAllowedClients
	}

	if err := store.SetServerConfig(ctx, serverConfig); err != nil {
		return fmt.Errorf("failed to set metadata server config: %w", err)
	}

	return nil
}
