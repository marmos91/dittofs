package config

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/store/content"
	contentfs "github.com/marmos91/dittofs/pkg/store/content/fs"
	contentmemory "github.com/marmos91/dittofs/pkg/store/content/memory"
	"github.com/marmos91/dittofs/pkg/store/content/s3"
	"github.com/marmos91/dittofs/pkg/store/metadata"
	"github.com/marmos91/dittofs/pkg/store/metadata/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/store/metadata/memory"
	"github.com/mitchellh/mapstructure"
)

// s3YAMLConfig represents S3 configuration loaded from YAML files.
type s3YAMLConfig struct {
	Endpoint        string `mapstructure:"endpoint"`
	Region          string `mapstructure:"region"`
	Bucket          string `mapstructure:"bucket"`
	AccessKeyID     string `mapstructure:"access_key_id"`
	SecretAccessKey string `mapstructure:"secret_access_key"`
	KeyPrefix       string `mapstructure:"key_prefix"`
	ForcePathStyle  bool   `mapstructure:"force_path_style"`
	PartSize        int64  `mapstructure:"part_size"`
}

// createMetadataStore creates a single metadata store instance.
// The capabilities parameter contains global filesystem settings that apply to all metadata stores.
func createMetadataStore(
	ctx context.Context,
	cfg MetadataStoreConfig,
	capabilities metadata.FilesystemCapabilities,
) (metadata.MetadataStore, error) {
	switch cfg.Type {
	case "memory":
		return createMemoryMetadataStore(ctx, cfg, capabilities)
	case "badger":
		return createBadgerMetadataStore(ctx, cfg, capabilities)
	default:
		return nil, fmt.Errorf("unknown metadata store type: %q", cfg.Type)
	}
}

// createMemoryMetadataStore creates an in-memory metadata store.
func createMemoryMetadataStore(
	ctx context.Context,
	cfg MetadataStoreConfig,
	capabilities metadata.FilesystemCapabilities,
) (metadata.MetadataStore, error) {
	// Decode memory-specific configuration
	var memoryCfg metadatamemory.MemoryMetadataStoreConfig
	if err := mapstructure.Decode(cfg.Memory, &memoryCfg); err != nil {
		return nil, fmt.Errorf("invalid memory config: %w", err)
	}

	// Create memory store with configuration
	store := metadatamemory.NewMemoryMetadataStore(memoryCfg)
	store.SetFilesystemCapabilities(capabilities)

	return store, nil
}

// createBadgerMetadataStore creates a BadgerDB metadata store.
func createBadgerMetadataStore(
	ctx context.Context,
	cfg MetadataStoreConfig,
	capabilities metadata.FilesystemCapabilities,
) (metadata.MetadataStore, error) {
	// Decode BadgerDB-specific configuration
	var badgerCfg badger.BadgerMetadataStoreConfig
	if err := mapstructure.Decode(cfg.Badger, &badgerCfg); err != nil {
		return nil, fmt.Errorf("invalid badger config: %w", err)
	}

	// Create BadgerDB store
	store, err := badger.NewBadgerMetadataStore(ctx, badgerCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to open badger database: %w", err)
	}

	store.SetFilesystemCapabilities(capabilities)

	return store, nil
}

// createContentStore creates a single content store instance.
func createContentStore(
	ctx context.Context,
	cfg ContentStoreConfig,
) (content.ContentStore, error) {
	switch cfg.Type {
	case "filesystem":
		return createFilesystemContentStore(ctx, cfg)
	case "memory":
		return createMemoryContentStore(ctx, cfg)
	case "s3":
		return createS3ContentStore(ctx, cfg)
	default:
		return nil, fmt.Errorf("unknown content store type: %q", cfg.Type)
	}
}

// createFilesystemContentStore creates a filesystem-backed content store.
func createFilesystemContentStore(
	ctx context.Context,
	cfg ContentStoreConfig,
) (content.ContentStore, error) {
	// Decode filesystem-specific configuration to get the path
	var fsCfg struct {
		Path string `mapstructure:"path"`
	}
	if err := mapstructure.Decode(cfg.Filesystem, &fsCfg); err != nil {
		return nil, fmt.Errorf("invalid filesystem config: %w", err)
	}

	if fsCfg.Path == "" {
		return nil, fmt.Errorf("filesystem path is required")
	}

	// Create filesystem store
	store, err := contentfs.NewFSContentStore(ctx, fsCfg.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize filesystem store: %w", err)
	}

	return store, nil
}

// createMemoryContentStore creates an in-memory content store.
func createMemoryContentStore(
	ctx context.Context,
	cfg ContentStoreConfig,
) (content.ContentStore, error) {
	// Decode memory-specific configuration
	var memCfg struct {
		MaxSizeBytes uint64 `mapstructure:"max_size_bytes"`
	}
	if err := mapstructure.Decode(cfg.Memory, &memCfg); err != nil {
		return nil, fmt.Errorf("invalid memory config: %w", err)
	}

	// Create memory store (currently doesn't support size limit configuration)
	store, err := contentmemory.NewMemoryContentStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create memory content store: %w", err)
	}

	return store, nil
}

// createS3ContentStore creates an S3-backed content store.
func createS3ContentStore(
	ctx context.Context,
	cfg ContentStoreConfig,
) (content.ContentStore, error) {
	// Decode S3 configuration from YAML
	var yamlCfg s3YAMLConfig
	if err := mapstructure.Decode(cfg.S3, &yamlCfg); err != nil {
		return nil, fmt.Errorf("invalid S3 config: %w", err)
	}

	// Validate required fields
	if yamlCfg.Bucket == "" {
		return nil, fmt.Errorf("S3 bucket is required")
	}
	if yamlCfg.Region == "" {
		return nil, fmt.Errorf("S3 region is required")
	}

	// Create S3 client using helper function
	client, err := s3.NewS3ClientFromConfig(
		ctx,
		yamlCfg.Endpoint,
		yamlCfg.Region,
		yamlCfg.AccessKeyID,
		yamlCfg.SecretAccessKey,
		yamlCfg.ForcePathStyle,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create S3 client: %w", err)
	}

	// Build S3ContentStoreConfig
	s3Cfg := s3.S3ContentStoreConfig{
		Client:    client,
		Bucket:    yamlCfg.Bucket,
		KeyPrefix: yamlCfg.KeyPrefix,
		PartSize:  yamlCfg.PartSize,
	}

	// Create S3 store
	store, err := s3.NewS3ContentStore(ctx, s3Cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize S3 store: %w", err)
	}

	return store, nil
}
