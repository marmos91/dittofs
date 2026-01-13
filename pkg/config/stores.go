package config

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
	"github.com/mitchellh/mapstructure"
)

// CreateCache creates a cache instance from configuration.
func CreateCache(cfg CacheConfig) (*cache.Cache, error) {
	switch cfg.Type {
	case "memory", "":
		return cache.New(cfg.MaxSize), nil
	case "mmap":
		if cfg.Mmap.Path == "" {
			return nil, fmt.Errorf("mmap cache requires path to be set")
		}
		return cache.NewWithMmap(cfg.Mmap.Path, cfg.MaxSize)
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
