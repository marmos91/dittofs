package config

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/registry"
)

// InitializeRegistry creates a fully configured Registry from the provided configuration.
//
// This function orchestrates the complete initialization process:
//  1. Creates and registers all metadata stores from cfg.Metadata.Stores
//  2. Validates and adds all shares from cfg.Shares
//  3. Each share automatically gets a Cache for content storage
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

	// Create empty registry
	reg := registry.NewRegistry()

	// Step 1: Register all metadata stores
	if err := registerMetadataStores(ctx, reg, cfg); err != nil {
		return nil, fmt.Errorf("failed to register metadata stores: %w", err)
	}
	logger.Info("Registered metadata stores", "count", reg.CountMetadataStores())

	// Step 2: Add all shares (each share gets its own Cache automatically)
	if err := addShares(ctx, reg, cfg); err != nil {
		return nil, fmt.Errorf("failed to add shares: %w", err)
	}
	logger.Info("Registered shares", "count", reg.CountShares())

	// Step 3: Create and register user store (if users/groups configured)
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

// addShares validates and adds all configured shares to the registry.
// Each share automatically gets a Cache for content storage.
func addShares(ctx context.Context, reg *registry.Registry, cfg *Config) error {
	for i, shareCfg := range cfg.Shares {
		logger.Debug("Adding share", "name", shareCfg.Name, "metadata", shareCfg.MetadataStore, "read_only", shareCfg.ReadOnly)

		// Validate share configuration
		if shareCfg.Name == "" {
			return fmt.Errorf("share #%d: name cannot be empty", i+1)
		}
		if shareCfg.MetadataStore == "" {
			return fmt.Errorf("share %q: metadata_store cannot be empty", shareCfg.Name)
		}

		// Create ShareConfig from configuration
		// Note: ContentStore and Cache fields were removed - Cache is auto-created
		shareConfig := &registry.ShareConfig{
			Name:                     shareCfg.Name,
			MetadataStore:            shareCfg.MetadataStore,
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
