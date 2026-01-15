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

// CreateCache creates a cache instance from configuration.
func CreateCache(cfg CacheConfig) (*cache.Cache, error) {
	switch cfg.Type {
	case "memory", "":
		return cache.New(cfg.MaxSize), nil
	case "wal":
		if cfg.Wal.Path == "" {
			return nil, fmt.Errorf("wal cache requires path to be set")
		}
		persister, err := wal.NewMmapPersister(cfg.Wal.Path)
		if err != nil {
			return nil, fmt.Errorf("failed to create WAL persister: %w", err)
		}
		return cache.NewWithWal(cfg.MaxSize, persister)
	default:
		return nil, fmt.Errorf("unknown cache type: %q", cfg.Type)
	}
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
// Returns nil, nil if block store is not configured (cache-only mode).
func CreateBlockStore(ctx context.Context, cfg BlockStoreConfig) (store.BlockStore, error) {
	switch cfg.Type {
	case "":
		// No block store configured - cache-only mode
		return nil, nil
	case "memory":
		return blockmemory.New(), nil
	case "s3":
		return createS3BlockStore(ctx, cfg.S3)
	case "filesystem":
		return createFSBlockStore(ctx, cfg.Filesystem)
	default:
		return nil, fmt.Errorf("unknown block store type: %q", cfg.Type)
	}
}

// createS3BlockStore creates an S3-backed block store.
func createS3BlockStore(ctx context.Context, cfg BlockStoreS3Config) (store.BlockStore, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("S3 block store requires bucket to be set")
	}

	s3Cfg := blocks3.Config{
		Bucket:         cfg.Bucket,
		Region:         cfg.Region,
		Endpoint:       cfg.Endpoint,
		KeyPrefix:      cfg.KeyPrefix,
		MaxRetries:     cfg.MaxRetries,
		ForcePathStyle: cfg.ForcePathStyle,
	}

	return blocks3.NewFromConfig(ctx, s3Cfg)
}

// createFSBlockStore creates a filesystem-backed block store.
func createFSBlockStore(_ context.Context, cfg BlockStoreFSConfig) (store.BlockStore, error) {
	if cfg.BasePath == "" {
		return nil, fmt.Errorf("filesystem block store requires base_path to be set")
	}

	// Build config - fs.New() applies defaults for zero values
	fsCfg := blockfs.Config{
		BasePath:  cfg.BasePath,
		CreateDir: cfg.CreateDir,
		DirMode:   os.FileMode(cfg.DirMode),
		FileMode:  os.FileMode(cfg.FileMode),
	}

	return blockfs.New(fsCfg)
}

// CreateTransferManager creates a transfer manager instance from configuration.
// Returns nil if block store is nil (cache-only mode, no S3 persistence).
func CreateTransferManager(c *cache.Cache, blockStore store.BlockStore, cfg FlusherConfig) *transfer.TransferManager {
	if blockStore == nil {
		// No block store - cache-only mode, no transfer manager needed
		return nil
	}

	tmCfg := transfer.Config{
		ParallelUploads:   cfg.ParallelUploads,
		ParallelDownloads: cfg.ParallelDownloads,
	}

	return transfer.New(c, blockStore, tmCfg)
}
