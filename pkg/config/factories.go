package config

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/content"
	contentFs "github.com/marmos91/dittofs/pkg/content/fs"
	contentS3 "github.com/marmos91/dittofs/pkg/content/s3"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/badger"
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
//   - "s3": Uses pkg/content/s3 (Amazon S3 or compatible storage)
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
		return nil, fmt.Errorf("memory content store is not available - use 'filesystem' instead (planned for future release)")
	case "s3":
		return createS3ContentStore(ctx, cfg.S3)
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

// createS3ContentStore creates an S3-based content store.
func createS3ContentStore(ctx context.Context, options map[string]any) (content.WritableContentStore, error) {
	// Define the configuration struct for S3 content store
	type S3ContentStoreConfig struct {
		Region          string `mapstructure:"region"`
		Bucket          string `mapstructure:"bucket"`
		KeyPrefix       string `mapstructure:"key_prefix"`
		Endpoint        string `mapstructure:"endpoint"`
		AccessKeyID     string `mapstructure:"access_key_id"`
		SecretAccessKey string `mapstructure:"secret_access_key"`
		PartSize        int64  `mapstructure:"part_size"`
	}

	// Decode the options into the config struct
	var storeCfg S3ContentStoreConfig
	if err := mapstructure.Decode(options, &storeCfg); err != nil {
		return nil, fmt.Errorf("failed to decode S3 content store config: %w", err)
	}

	// Validate required fields
	if storeCfg.Bucket == "" {
		return nil, fmt.Errorf("S3 content store: bucket is required")
	}

	if storeCfg.Region == "" {
		return nil, fmt.Errorf("S3 content store: region is required")
	}

	// ========================================================================
	// Step 1: Build AWS Config
	// ========================================================================

	var configOptions []func(*awsConfig.LoadOptions) error

	// Set region
	configOptions = append(configOptions, awsConfig.WithRegion(storeCfg.Region))

	// Set custom endpoint if provided (for MinIO, Localstack, etc.)
	if storeCfg.Endpoint != "" {
		//nolint:staticcheck // TODO: migrate to BaseEndpoint when AWS SDK v2 stabilizes the new API
		customResolver := aws.EndpointResolverWithOptionsFunc(
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				//nolint:staticcheck // TODO: migrate to BaseEndpoint when AWS SDK v2 stabilizes the new API
				return aws.Endpoint{
					URL:               storeCfg.Endpoint,
					HostnameImmutable: true,
					Source:            aws.EndpointSourceCustom,
				}, nil
			},
		)
		//nolint:staticcheck // TODO: migrate to BaseEndpoint when AWS SDK v2 stabilizes the new API
		configOptions = append(configOptions, awsConfig.WithEndpointResolverWithOptions(customResolver))
	}

	// Set credentials if provided, otherwise use default credential chain
	if storeCfg.AccessKeyID != "" && storeCfg.SecretAccessKey != "" {
		credProvider := credentials.NewStaticCredentialsProvider(
			storeCfg.AccessKeyID,
			storeCfg.SecretAccessKey,
			"", // session token (empty for static credentials)
		)
		configOptions = append(configOptions, awsConfig.WithCredentialsProvider(credProvider))
	}

	// Load AWS config
	cfg, err := awsConfig.LoadDefaultConfig(ctx, configOptions...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// ========================================================================
	// Step 2: Create S3 Client
	// ========================================================================

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		// Force path-style addressing for compatibility with MinIO/Localstack
		if storeCfg.Endpoint != "" {
			o.UsePathStyle = true
		}
	})

	// ========================================================================
	// Step 3: Create S3 Content Store
	// ========================================================================

	store, err := contentS3.NewS3ContentStore(ctx, contentS3.S3ContentStoreConfig{
		Client:    client,
		Bucket:    storeCfg.Bucket,
		KeyPrefix: storeCfg.KeyPrefix,
		PartSize:  storeCfg.PartSize,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create S3 content store: %w", err)
	}

	logger.Info("S3 content store initialized: bucket=%s, region=%s, prefix=%s",
		storeCfg.Bucket, storeCfg.Region, storeCfg.KeyPrefix)

	return store, nil
}

// CreateMetadataStore creates a metadata store based on configuration.
//
// This factory function uses the Type field to determine which store implementation
// to create, then decodes the type-specific configuration from the corresponding
// map and passes it to the store's constructor.
//
// Supported types:
//   - "memory": Uses pkg/metadata/memory (in-memory storage, ephemeral)
//   - "badger": Uses pkg/metadata/badger (BadgerDB storage, persistent)
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
	case "badger":
		return createBadgerMetadataStore(ctx, cfg.Badger, &cfg.Capabilities)
	default:
		return nil, fmt.Errorf("unknown metadata store type: %q (supported: memory, badger)", cfg.Type)
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

// createBadgerMetadataStore creates a BadgerDB-based persistent metadata store.
func createBadgerMetadataStore(ctx context.Context, options map[string]any, capabilities *metadata.FilesystemCapabilities) (metadata.MetadataStore, error) {
	// Check context before creating store
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Decode store-specific options
	type BadgerMetadataStoreOptions struct {
		DBPath          string `mapstructure:"db_path"`
		MaxStorageBytes uint64 `mapstructure:"max_storage_bytes"`
		MaxFiles        uint64 `mapstructure:"max_files"`
	}

	var storeOpts BadgerMetadataStoreOptions
	if err := mapstructure.Decode(options, &storeOpts); err != nil {
		return nil, fmt.Errorf("failed to decode badger metadata store options: %w", err)
	}

	// Validate required fields
	if storeOpts.DBPath == "" {
		return nil, fmt.Errorf("badger metadata store: db_path is required")
	}

	// Create store config
	storeConfig := badger.BadgerMetadataStoreConfig{
		DBPath:          storeOpts.DBPath,
		Capabilities:    *capabilities,
		MaxStorageBytes: storeOpts.MaxStorageBytes,
		MaxFiles:        storeOpts.MaxFiles,
	}

	store, err := badger.NewBadgerMetadataStore(ctx, storeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create badger metadata store: %w", err)
	}

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
				MapAllToAnonymous:        shareCfg.IdentityMapping.MapAllToAnonymous,
				MapPrivilegedToAnonymous: shareCfg.IdentityMapping.MapPrivilegedToAnonymous,
				AnonymousUID:             &shareCfg.IdentityMapping.AnonymousUID,
				AnonymousGID:             &shareCfg.IdentityMapping.AnonymousGID,
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

		// Add the share (idempotent - skip if already exists)
		if err := store.AddShare(ctx, shareCfg.Name, options, rootAttr); err != nil {
			// Check if the share already exists (expected on server restart with persistent metadata)
			var storeErr *metadata.StoreError
			if errors.As(err, &storeErr) && storeErr.Code == metadata.ErrAlreadyExists {
				logger.Info("Share %q already exists, skipping creation", shareCfg.Name)
				continue
			}
			// Any other error is a real problem
			return fmt.Errorf("failed to add share[%d] %q: %w", i, shareCfg.Name, err)
		}

		logger.Info("Created share: %s", shareCfg.Name)
	}

	// Log actual share configurations from metadata store for verification
	if err := logShareConfigurations(ctx, store, shares); err != nil {
		logger.Warn("Failed to log share configurations: %v", err)
	}

	return nil
}

// logShareConfigurations queries and logs the actual share configurations from the metadata store.
//
// This helps verify that the shares are configured as expected, especially when shares
// already exist and were skipped during creation.
func logShareConfigurations(ctx context.Context, store metadata.MetadataStore, shares []ShareConfig) error {
	logger.Info("=== Active Share Configurations ===")

	for _, shareCfg := range shares {
		// Query the actual share from metadata store
		share, err := store.FindShare(ctx, shareCfg.Name)
		if err != nil {
			logger.Warn("Failed to query share %q: %v", shareCfg.Name, err)
			continue
		}

		// Get the root directory attributes
		rootHandle, err := store.GetShareRoot(ctx, shareCfg.Name)
		if err != nil {
			logger.Warn("Failed to get root handle for share %q: %v", shareCfg.Name, err)
			continue
		}

		rootAttr, err := store.GetFile(ctx, rootHandle)
		if err != nil {
			logger.Warn("Failed to get root attributes for share %q: %v", shareCfg.Name, err)
			continue
		}

		// Log share configuration details
		logger.Info("Share: %s", share.Name)
		logger.Info("  Read-Only: %v", share.Options.ReadOnly)
		logger.Info("  Root Directory: uid=%d gid=%d mode=%04o", rootAttr.UID, rootAttr.GID, rootAttr.Mode)

		if share.Options.IdentityMapping != nil {
			logger.Info("  Identity Mapping:")
			logger.Info("    - All Squash (map_all_to_anonymous): %v", share.Options.IdentityMapping.MapAllToAnonymous)
			logger.Info("    - Root Squash (map_privileged_to_anonymous): %v", share.Options.IdentityMapping.MapPrivilegedToAnonymous)
			if share.Options.IdentityMapping.AnonymousUID != nil {
				logger.Info("    - Anonymous UID: %d", *share.Options.IdentityMapping.AnonymousUID)
			}
			if share.Options.IdentityMapping.AnonymousGID != nil {
				logger.Info("    - Anonymous GID: %d", *share.Options.IdentityMapping.AnonymousGID)
			}
		}
	}

	logger.Info("==================================")
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
