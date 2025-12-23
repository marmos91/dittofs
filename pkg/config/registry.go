package config

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/registry"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// InitializeRegistry creates a fully configured Registry from the provided configuration.
//
// This function orchestrates the complete initialization process:
//  1. Creates and registers all metadata stores from cfg.Metadata.Stores
//  2. Creates and registers all content stores from cfg.Content.Stores
//  3. Validates and adds all shares from cfg.Shares
//  4. Validates that all shares reference existing stores
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
//   - At least one content store must be configured
//   - At least one share must be configured
//   - All shares must reference existing stores
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

	// Create empty registry
	reg := registry.NewRegistry()

	// Step 1: Register all metadata stores
	if err := registerMetadataStores(ctx, reg, cfg); err != nil {
		return nil, fmt.Errorf("failed to register metadata stores: %w", err)
	}
	logger.Info("Registered metadata stores", "count", reg.CountMetadataStores())

	// Step 2: Register all content stores
	if err := registerContentStores(ctx, reg, cfg); err != nil {
		return nil, fmt.Errorf("failed to register content stores: %w", err)
	}
	logger.Info("Registered content stores", "count", reg.CountContentStores())

	// Step 3: Register all caches (optional, may be empty)
	if err := registerCaches(ctx, reg, cfg); err != nil {
		return nil, fmt.Errorf("failed to register caches: %w", err)
	}
	if reg.CountCaches() > 0 {
		logger.Info("Registered caches", "count", reg.CountCaches())
	} else {
		logger.Info("No caches configured (all shares will use sync mode)")
	}

	// Step 4: Add all shares
	if err := addShares(ctx, reg, cfg); err != nil {
		return nil, fmt.Errorf("failed to add shares: %w", err)
	}
	logger.Info("Registered shares", "count", reg.CountShares())

	// Step 5: Create and register user store (if users/groups configured)
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

	if len(cfg.Content.Stores) == 0 {
		return fmt.Errorf("no content stores configured: at least one content store is required")
	}

	if len(cfg.Shares) == 0 {
		return fmt.Errorf("no shares configured: at least one share is required")
	}

	return nil
}

// registerMetadataStores creates and registers all configured metadata stores.
func registerMetadataStores(ctx context.Context, reg *registry.Registry, cfg *Config) error {
	// Get global filesystem capabilities that apply to all metadata stores
	capabilities := cfg.Metadata.Global.FilesystemCapabilities

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

// registerContentStores creates and registers all configured content stores.
func registerContentStores(ctx context.Context, reg *registry.Registry, cfg *Config) error {
	for name, storeCfg := range cfg.Content.Stores {
		logger.Debug("Creating content store", "name", name, "type", storeCfg.Type)

		store, err := createContentStore(ctx, storeCfg)
		if err != nil {
			return fmt.Errorf("failed to create content store %q: %w", name, err)
		}

		if err := reg.RegisterContentStore(name, store); err != nil {
			return fmt.Errorf("failed to register content store %q: %w", name, err)
		}

		logger.Debug("Content store registered successfully", "name", name)
	}

	return nil
}

// registerCaches creates and registers all configured caches.
func registerCaches(ctx context.Context, reg *registry.Registry, cfg *Config) error {
	// Caches are optional - if no caches are configured, that's fine
	if len(cfg.Cache.Stores) == 0 {
		return nil
	}

	for name, cacheCfg := range cfg.Cache.Stores {
		logger.Debug("Creating cache", "name", name, "type", cacheCfg.Type)

		cache, err := createCache(ctx, cacheCfg)
		if err != nil {
			return fmt.Errorf("failed to create cache %q: %w", name, err)
		}

		if err := reg.RegisterCache(name, cache); err != nil {
			return fmt.Errorf("failed to register cache %q: %w", name, err)
		}

		logger.Debug("Cache registered successfully", "name", name)
	}

	return nil
}

// addShares validates and adds all configured shares to the registry.
func addShares(ctx context.Context, reg *registry.Registry, cfg *Config) error {
	for i, shareCfg := range cfg.Shares {
		if shareCfg.Cache != "" {
			logger.Debug("Adding share", "name", shareCfg.Name, "metadata", shareCfg.MetadataStore, "content", shareCfg.ContentStore, "read_only", shareCfg.ReadOnly, "cache", shareCfg.Cache)
		} else {
			logger.Debug("Adding share", "name", shareCfg.Name, "metadata", shareCfg.MetadataStore, "content", shareCfg.ContentStore, "read_only", shareCfg.ReadOnly)
		}

		// Validate share configuration
		if shareCfg.Name == "" {
			return fmt.Errorf("share #%d: name cannot be empty", i+1)
		}
		if shareCfg.MetadataStore == "" {
			return fmt.Errorf("share %q: metadata_store cannot be empty", shareCfg.Name)
		}
		if shareCfg.ContentStore == "" {
			return fmt.Errorf("share %q: content_store cannot be empty", shareCfg.Name)
		}

		// Create ShareConfig from configuration
		shareConfig := &registry.ShareConfig{
			Name:                     shareCfg.Name,
			MetadataStore:            shareCfg.MetadataStore,
			ContentStore:             shareCfg.ContentStore,
			Cache:                    shareCfg.Cache, // Unified cache
			ReadOnly:                 shareCfg.ReadOnly,
			AllowGuest:               shareCfg.AllowGuest,
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

		// If a cache is configured, copy cache behavior settings to share config
		if shareCfg.Cache != "" {
			if cacheCfg, ok := cfg.Cache.Stores[shareCfg.Cache]; ok {
				// Copy prefetch config
				if cacheCfg.Prefetch.Enabled != nil {
					shareConfig.PrefetchConfig.Enabled = *cacheCfg.Prefetch.Enabled
				} else {
					shareConfig.PrefetchConfig.Enabled = true // default
				}
				shareConfig.PrefetchConfig.MaxFileSize = cacheCfg.Prefetch.MaxFileSize.Int64()
				shareConfig.PrefetchConfig.ChunkSize = cacheCfg.Prefetch.ChunkSize.Int64()

				// Copy flusher config
				shareConfig.FlusherConfig.SweepInterval = cacheCfg.Flusher.SweepInterval
				shareConfig.FlusherConfig.FlushTimeout = cacheCfg.Flusher.FlushTimeout
				shareConfig.FlusherConfig.FlushPoolSize = cacheCfg.Flusher.FlushPoolSize

				// Copy write gathering config
				if cacheCfg.WriteGathering.Enabled != nil {
					shareConfig.WriteGatheringConfig.Enabled = *cacheCfg.WriteGathering.Enabled
				} else {
					shareConfig.WriteGatheringConfig.Enabled = true // default: enabled
				}
				shareConfig.WriteGatheringConfig.GatherDelay = cacheCfg.WriteGathering.GatherDelay
				shareConfig.WriteGatheringConfig.ActiveThreshold = cacheCfg.WriteGathering.ActiveThreshold
			}
		}

		// Add share to registry (registry will validate stores exist and create root directory)
		if err := reg.AddShare(ctx, shareConfig); err != nil {
			return fmt.Errorf("failed to add share %q: %w", shareCfg.Name, err)
		}

		// Log cache status at INFO level for visibility
		if shareCfg.Cache != "" {
			logger.Info("Share configured with cache (async writes, read caching enabled)", "share", shareCfg.Name, "cache", shareCfg.Cache)
		} else {
			logger.Info("Share configured without cache (sync writes, no read caching)", "share", shareCfg.Name)
		}

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
