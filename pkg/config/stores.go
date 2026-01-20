package config

import (
	"context"
	"fmt"
	"os"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/cache/wal"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
	"github.com/marmos91/dittofs/pkg/payload/store"
	blockfs "github.com/marmos91/dittofs/pkg/payload/store/fs"
	blockmemory "github.com/marmos91/dittofs/pkg/payload/store/memory"
	blocks3 "github.com/marmos91/dittofs/pkg/payload/store/s3"
	"github.com/marmos91/dittofs/pkg/payload/transfer"
	"github.com/mitchellh/mapstructure"
)

// CreateCache creates a WAL-backed cache instance from configuration.
// WAL is mandatory for crash recovery - all writes go through the WAL cache.
func CreateCache(cfg CacheConfig) (*cache.Cache, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("cache path is required (cache.path)")
	}
	persister, err := wal.NewMmapPersister(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to create WAL persister: %w", err)
	}
	return cache.NewWithWal(uint64(cfg.Size), persister)
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
	case "postgres":
		return createPostgresMetadataStore(ctx, cfg, capabilities)
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

// createPostgresMetadataStore creates a PostgreSQL metadata store.
func createPostgresMetadataStore(
	ctx context.Context,
	cfg MetadataStoreConfig,
	capabilities metadata.FilesystemCapabilities,
) (metadata.MetadataStore, error) {
	// Decode PostgreSQL-specific configuration
	var pgCfg postgres.PostgresMetadataStoreConfig
	if err := mapstructure.Decode(cfg.Postgres, &pgCfg); err != nil {
		return nil, fmt.Errorf("invalid postgres config: %w", err)
	}

	// Apply defaults
	pgCfg.ApplyDefaults()

	// Handle PrepareStatements default (we want true by default)
	// mapstructure doesn't have a way to set bool defaults, so we check if it's explicitly set
	if cfg.Postgres != nil {
		if _, exists := cfg.Postgres["prepare_statements"]; !exists {
			pgCfg.PrepareStatements = true // Default to true
		}
	} else {
		pgCfg.PrepareStatements = true
	}

	// Create PostgreSQL store
	store, err := postgres.NewPostgresMetadataStore(ctx, &pgCfg, capabilities)
	if err != nil {
		return nil, fmt.Errorf("failed to create postgres metadata store: %w", err)
	}

	return store, nil
}

// CreateBlockStore creates a block store instance from configuration.
func CreateBlockStore(ctx context.Context, cfg PayloadStoreConfig) (store.BlockStore, error) {
	switch cfg.Type {
	case "memory":
		return blockmemory.New(), nil
	case "s3":
		if cfg.S3 == nil {
			return nil, fmt.Errorf("S3 block store requires s3 configuration")
		}
		return createS3BlockStore(ctx, cfg.S3)
	case "filesystem":
		if cfg.Filesystem == nil {
			return nil, fmt.Errorf("filesystem block store requires filesystem configuration")
		}
		return createFSBlockStore(ctx, cfg.Filesystem)
	default:
		return nil, fmt.Errorf("unknown block store type: %q", cfg.Type)
	}
}

// createS3BlockStore creates an S3-backed block store.
func createS3BlockStore(ctx context.Context, cfg *PayloadS3Config) (store.BlockStore, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("S3 block store requires bucket to be set")
	}

	s3Cfg := blocks3.Config{
		Bucket:         cfg.Bucket,
		Region:         cfg.Region,
		Endpoint:       cfg.Endpoint,
		AccessKey:      cfg.AccessKeyID,
		SecretKey:      cfg.SecretAccessKey,
		KeyPrefix:      cfg.Prefix,
		MaxRetries:     cfg.MaxRetries,
		ForcePathStyle: cfg.ForcePathStyle,
	}

	return blocks3.NewFromConfig(ctx, s3Cfg)
}

// createFSBlockStore creates a filesystem-backed block store.
func createFSBlockStore(_ context.Context, cfg *PayloadFSConfig) (store.BlockStore, error) {
	if cfg.BasePath == "" {
		return nil, fmt.Errorf("filesystem block store requires base_path to be set")
	}

	// Build config - fs.New() applies defaults for zero values
	createDir := true
	if cfg.CreateDir != nil {
		createDir = *cfg.CreateDir
	}
	fsCfg := blockfs.Config{
		BasePath:  cfg.BasePath,
		CreateDir: createDir,
		DirMode:   os.FileMode(cfg.DirMode),
		FileMode:  os.FileMode(cfg.FileMode),
	}

	return blockfs.New(fsCfg)
}

// CreateTransferManager creates a transfer manager instance from configuration.
// objectStore is required for content-addressed deduplication.
func CreateTransferManager(c *cache.Cache, blockStore store.BlockStore, objectStore metadata.ObjectStore, cfg TransferConfig) *transfer.TransferManager {
	tmCfg := transfer.Config{
		ParallelUploads:   cfg.Workers.Uploads,
		ParallelDownloads: cfg.Workers.Downloads,
	}

	return transfer.New(c, blockStore, objectStore, tmCfg)
}
