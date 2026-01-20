package config

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/payload"
	"github.com/marmos91/dittofs/pkg/registry"
)

// InitializeRegistry creates a fully configured Registry from the provided configuration.
//
// This function orchestrates the complete initialization process:
//  1. Creates cache (WAL-backed, mandatory for crash recovery)
//  2. Creates block store from payload configuration
//  3. Creates transfer manager for cache-to-block-store persistence
//  4. Creates and registers all metadata stores from cfg.Metadata.Stores
//  5. Validates and adds all shares from cfg.Shares
//
// The resulting Registry contains all stores and shares ready for use by the DittoServer.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - cfg: Complete configuration loaded from config file
//
// Returns:
//   - *registry.Registry: Fully initialized registry
//   - error: If store creation fails, share validation fails, or configuration is invalid
//
// Validation performed:
//   - At least one metadata store must be configured
//   - At least one share must be configured
//   - All shares must reference existing metadata stores
//
// Example:
//
//	cfg, _ := config.Load("config.yaml")
//	reg, err := config.InitializeRegistry(ctx, cfg)
//	if err != nil {
//	    log.Fatalf("Failed to initialize registry: %v", err)
//	}
func InitializeRegistry(ctx context.Context, cfg *Config) (*registry.Registry, error) {
	logger.Debug("Initializing registry from configuration")

	// Validate configuration has required sections
	if err := validateRegistryConfig(cfg); err != nil {
		return nil, err
	}

	// Create registry
	reg := registry.NewRegistry()

	// Step 1: Create cache from configuration (WAL-backed, mandatory)
	globalCache, err := CreateCache(cfg.Cache)
	if err != nil {
		return nil, fmt.Errorf("failed to create cache: %w", err)
	}
	logger.Info("Created WAL-backed cache", "path", cfg.Cache.Path, "size", cfg.Cache.Size)

	// Step 2: Create block store from first payload store (required)
	if len(cfg.Payload.Stores) == 0 {
		return nil, fmt.Errorf("at least one payload store must be configured")
	}

	// Get first payload store (or the one specified by name)
	var blockStoreName string
	var blockStoreCfg PayloadStoreConfig
	for name, storeCfg := range cfg.Payload.Stores {
		blockStoreName = name
		blockStoreCfg = storeCfg
		break
	}

	blockStore, err := CreateBlockStore(ctx, blockStoreCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create block store %q: %w", blockStoreName, err)
	}
	logger.Info("Created block store", "name", blockStoreName, "type", blockStoreCfg.Type)

	// Step 3: Create transfer manager (required)
	// Create a dedicated ObjectStore for content-addressed deduplication
	// This is shared across all files/shares to enable global deduplication
	objectStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	transferMgr := CreateTransferManager(globalCache, blockStore, objectStore, cfg.Payload.Transfer)
	logger.Info("Created transfer manager for cache-to-block-store persistence",
		"uploads", cfg.Payload.Transfer.Workers.Uploads,
		"downloads", cfg.Payload.Transfer.Workers.Downloads)

	// Step 4: Create PayloadService with cache and transfer manager
	payloadSvc, err := payload.New(globalCache, transferMgr)
	if err != nil {
		return nil, fmt.Errorf("failed to create payload service: %w", err)
	}
	reg.SetPayloadService(payloadSvc)
	logger.Info("Created payload service")

	// Start background uploader for async block store uploads
	transferMgr.Start(ctx)

	// Step 4.5: Recover unflushed cache data from previous run (async)
	// Use a context derived from the parent to allow graceful cancellation during shutdown
	go func(recoveryCtx context.Context) {
		stats := transferMgr.RecoverUnflushedBlocks(recoveryCtx)
		if stats.BlocksFound > 0 {
			logger.Info("Recovery: started background upload of unflushed data",
				"files", stats.FilesScanned,
				"blocks", stats.BlocksFound,
				"bytes", stats.BytesPending)
		} else {
			logger.Info("Recovery: no unflushed data found")
		}
	}(ctx)

	// Step 5: Register all metadata stores
	if err := registerMetadataStores(ctx, reg, cfg); err != nil {
		return nil, fmt.Errorf("failed to register metadata stores: %w", err)
	}
	logger.Info("Registered metadata stores", "count", reg.CountMetadataStores())

	// Step 6: Add all shares
	if err := addShares(ctx, reg, cfg); err != nil {
		return nil, fmt.Errorf("failed to add shares: %w", err)
	}
	logger.Info("Registered shares", "count", reg.CountShares())

	// Step 7: Create and register user store (if users/groups configured)
	if len(cfg.Users) > 0 || len(cfg.Groups) > 0 || cfg.Guest.Enabled {
		userStore, err := cfg.CreateUserStore()
		if err != nil {
			return nil, fmt.Errorf("failed to create user store: %w", err)
		}
		reg.SetUserStore(userStore)
		logger.Info("Registered user store", "users", len(cfg.Users), "groups", len(cfg.Groups), "guest_enabled", cfg.Guest.Enabled)
	}

	return reg, nil
}

// validateRegistryConfig performs basic validation on the configuration.
func validateRegistryConfig(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("configuration is nil")
	}

	if len(cfg.Metadata.Stores) == 0 {
		return fmt.Errorf("no metadata stores configured: at least one metadata store is required")
	}

	if len(cfg.Shares) == 0 {
		return fmt.Errorf("no shares configured: at least one share is required")
	}

	return nil
}

// registerMetadataStores creates and registers all configured metadata stores.
func registerMetadataStores(ctx context.Context, reg *registry.Registry, cfg *Config) error {
	// Get filesystem capabilities that apply to all metadata stores
	capabilities := cfg.Metadata.FilesystemCapabilities

	for name, storeCfg := range cfg.Metadata.Stores {
		logger.Debug("Creating metadata store", "name", name, "type", storeCfg.Type)

		store, err := createMetadataStore(ctx, storeCfg, capabilities)
		if err != nil {
			return fmt.Errorf("failed to create metadata store %q: %w", name, err)
		}

		if err := reg.RegisterMetadataStore(name, store); err != nil {
			return fmt.Errorf("failed to register metadata store %q: %w", name, err)
		}

		logger.Debug("Metadata store registered successfully", "name", name)
	}

	return nil
}

// addShares validates and adds all configured shares to the registry.
// Each share automatically gets a Cache for content storage.
func addShares(ctx context.Context, reg *registry.Registry, cfg *Config) error {
	for i, shareCfg := range cfg.Shares {
		logger.Debug("Adding share", "name", shareCfg.Name, "metadata", shareCfg.Metadata, "read_only", shareCfg.ReadOnly)

		// Validate share configuration
		if shareCfg.Name == "" {
			return fmt.Errorf("share #%d: name cannot be empty", i+1)
		}
		if shareCfg.Metadata == "" {
			return fmt.Errorf("share %q: metadata cannot be empty", shareCfg.Name)
		}

		// Create ShareConfig from configuration
		// Note: ContentStore and Cache fields were removed - Cache is auto-created
		shareConfig := &registry.ShareConfig{
			Name:                     shareCfg.Name,
			MetadataStore:            shareCfg.Metadata,
			ReadOnly:                 shareCfg.ReadOnly,
			DefaultPermission:        shareCfg.DefaultPermission,
			AllowedClients:           shareCfg.AllowedClients,
			DeniedClients:            shareCfg.DeniedClients,
			RequireAuth:              shareCfg.RequireAuth,
			AllowedAuthMethods:       shareCfg.AllowedAuthMethods,
			MapAllToAnonymous:        shareCfg.IdentityMapping.MapAllToAnonymous,
			MapPrivilegedToAnonymous: shareCfg.IdentityMapping.MapPrivilegedToAnonymous,
			AnonymousUID:             shareCfg.IdentityMapping.AnonymousUID,
			AnonymousGID:             shareCfg.IdentityMapping.AnonymousGID,
			RootAttr:                 createRootAttributes(shareCfg.RootDirectoryAttributes),
		}

		// Add share to registry (registry will validate stores exist, create root directory, and auto-create Cache)
		if err := reg.AddShare(ctx, shareConfig); err != nil {
			return fmt.Errorf("failed to add share %q: %w", shareCfg.Name, err)
		}

		logger.Info("Share configured with Cache content storage", "share", shareCfg.Name)
		logger.Debug("Share added successfully", "name", shareCfg.Name)
	}

	return nil
}

// createRootAttributes creates FileAttr for the share's root directory from configuration.
func createRootAttributes(cfg RootDirectoryAttributesConfig) *metadata.FileAttr {
	return &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: cfg.Mode,
		UID:  cfg.UID,
		GID:  cfg.GID,
		// Size, timestamps, and other fields will be filled by CreateRootDirectory
	}
}
