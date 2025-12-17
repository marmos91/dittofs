//go:build e2e

// Package framework provides the test infrastructure for DittoFS e2e tests.
package framework

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/pkg/cache"
	memorycache "github.com/marmos91/dittofs/pkg/cache/memory"
	"github.com/marmos91/dittofs/pkg/store/content"
	contentfs "github.com/marmos91/dittofs/pkg/store/content/fs"
	contentmemory "github.com/marmos91/dittofs/pkg/store/content/memory"
	contents3 "github.com/marmos91/dittofs/pkg/store/content/s3"
	"github.com/marmos91/dittofs/pkg/store/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/store/metadata/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/store/metadata/memory"
	metadatapostgres "github.com/marmos91/dittofs/pkg/store/metadata/postgres"
)

// MetadataStoreType represents the type of metadata store.
type MetadataStoreType string

const (
	MetadataMemory   MetadataStoreType = "memory"
	MetadataBadger   MetadataStoreType = "badger"
	MetadataPostgres MetadataStoreType = "postgres"
)

// ContentStoreType represents the type of content store.
type ContentStoreType string

const (
	ContentMemory     ContentStoreType = "memory"
	ContentFilesystem ContentStoreType = "filesystem"
	ContentS3         ContentStoreType = "s3"
)

// TestConfig holds the configuration for a test run.
type TestConfig struct {
	Name          string
	MetadataStore MetadataStoreType
	ContentStore  ContentStoreType
	ShareName     string
	UseCache      bool // Enable cache layer (always true for S3)

	// S3-specific fields (set by localstack setup)
	S3Client *s3.Client
	S3Bucket string

	// PostgreSQL-specific fields (set by postgres setup)
	PostgresConfig *PostgresConfig
}

// String returns a human-readable representation of the configuration.
func (tc *TestConfig) String() string {
	if tc.UseCache {
		return fmt.Sprintf("%s-%s-cached", tc.MetadataStore, tc.ContentStore)
	}
	return fmt.Sprintf("%s-%s", tc.MetadataStore, tc.ContentStore)
}

// RequiresDocker returns true if this configuration requires Docker.
func (tc *TestConfig) RequiresDocker() bool {
	return tc.MetadataStore == MetadataPostgres || tc.ContentStore == ContentS3
}

// RequiresS3 returns true if this configuration requires S3/Localstack.
func (tc *TestConfig) RequiresS3() bool {
	return tc.ContentStore == ContentS3
}

// RequiresPostgres returns true if this configuration requires PostgreSQL.
func (tc *TestConfig) RequiresPostgres() bool {
	return tc.MetadataStore == MetadataPostgres
}

// CreateCache creates a memory cache if UseCache is enabled.
// Returns nil if caching is not enabled.
func (tc *TestConfig) CreateCache() cache.Cache {
	if !tc.UseCache {
		return nil
	}
	// 256MB cache - enough for large file tests
	return memorycache.NewMemoryCache(256*1024*1024, nil)
}

// TestContextProvider is an interface for providing test context dependencies.
type TestContextProvider interface {
	CreateTempDir(prefix string) string
	GetConfig() *TestConfig
	GetPort() int
}

// CreateMetadataStore creates a metadata store based on the configuration.
func (tc *TestConfig) CreateMetadataStore(ctx context.Context, provider TestContextProvider) (metadata.MetadataStore, error) {
	switch tc.MetadataStore {
	case MetadataMemory:
		return metadatamemory.NewMemoryMetadataStoreWithDefaults(), nil

	case MetadataBadger:
		dbPath := filepath.Join(provider.CreateTempDir("dittofs-badger-*"), "metadata.db")
		store, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
		if err != nil {
			return nil, fmt.Errorf("failed to create badger metadata store: %w", err)
		}
		return store, nil

	case MetadataPostgres:
		config := provider.GetConfig()
		if config.PostgresConfig == nil {
			return nil, fmt.Errorf("PostgreSQL config not initialized (postgres container not running?)")
		}

		pgConfig := &metadatapostgres.PostgresMetadataStoreConfig{
			Host:        config.PostgresConfig.Host,
			Port:        config.PostgresConfig.Port,
			Database:    config.PostgresConfig.Database,
			User:        config.PostgresConfig.User,
			Password:    config.PostgresConfig.Password,
			SSLMode:     "disable",
			MaxConns:    10,
			MinConns:    2,
			AutoMigrate: true,
		}

		capabilities := metadata.FilesystemCapabilities{
			MaxReadSize:         1024 * 1024,
			PreferredReadSize:   64 * 1024,
			MaxWriteSize:        1024 * 1024,
			PreferredWriteSize:  64 * 1024,
			MaxFileSize:         1024 * 1024 * 1024 * 100, // 100 GB
			MaxFilenameLen:      255,
			MaxPathLen:          4096,
			MaxHardLinkCount:    32767,
			SupportsHardLinks:   true,
			SupportsSymlinks:    true,
			CaseSensitive:       true,
			CasePreserving:      true,
			SupportsACLs:        false,
			TimestampResolution: time.Nanosecond,
		}

		store, err := metadatapostgres.NewPostgresMetadataStore(ctx, pgConfig, capabilities)
		if err != nil {
			return nil, fmt.Errorf("failed to create postgres metadata store: %w", err)
		}
		return store, nil

	default:
		return nil, fmt.Errorf("unknown metadata store type: %s", tc.MetadataStore)
	}
}

// CreateContentStore creates a content store based on the configuration.
func (tc *TestConfig) CreateContentStore(ctx context.Context, provider TestContextProvider) (content.ContentStore, error) {
	switch tc.ContentStore {
	case ContentMemory:
		store, err := contentmemory.NewMemoryContentStore(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to create memory content store: %w", err)
		}
		return store, nil

	case ContentFilesystem:
		contentPath := provider.CreateTempDir("dittofs-content-*")
		store, err := contentfs.NewFSContentStore(ctx, contentPath)
		if err != nil {
			return nil, fmt.Errorf("failed to create filesystem content store: %w", err)
		}
		return store, nil

	case ContentS3:
		config := provider.GetConfig()
		if config.S3Client == nil {
			return nil, fmt.Errorf("S3 client not initialized (localstack not running?)")
		}

		bucketName := config.S3Bucket
		if bucketName == "" {
			bucketName = fmt.Sprintf("dittofs-e2e-test-%d", provider.GetPort())
			config.S3Bucket = bucketName
		}

		store, err := contents3.NewS3ContentStore(ctx, contents3.S3ContentStoreConfig{
			Client:        config.S3Client,
			Bucket:        bucketName,
			KeyPrefix:     "test/",
			PartSize:      5 * 1024 * 1024, // 5MB parts
			StatsCacheTTL: 1,               // 1ns - effectively disabled for tests
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create S3 content store: %w", err)
		}
		return store, nil

	default:
		return nil, fmt.Errorf("unknown content store type: %s", tc.ContentStore)
	}
}

// AllConfigurations returns all 8 test configurations.
// S3 configurations always have cache enabled.
// Requires Docker for PostgreSQL and S3 configurations.
func AllConfigurations() []*TestConfig {
	return []*TestConfig{
		// Local backends (no Docker required)
		{
			Name:          "memory-memory",
			MetadataStore: MetadataMemory,
			ContentStore:  ContentMemory,
			ShareName:     "/export",
			UseCache:      false,
		},
		{
			Name:          "memory-filesystem",
			MetadataStore: MetadataMemory,
			ContentStore:  ContentFilesystem,
			ShareName:     "/export",
			UseCache:      false,
		},
		{
			Name:          "badger-filesystem",
			MetadataStore: MetadataBadger,
			ContentStore:  ContentFilesystem,
			ShareName:     "/export",
			UseCache:      false,
		},
		// Local with cache (for cache testing without Docker)
		{
			Name:          "memory-memory-cached",
			MetadataStore: MetadataMemory,
			ContentStore:  ContentMemory,
			ShareName:     "/export",
			UseCache:      true,
		},
		// PostgreSQL backends (requires Docker)
		{
			Name:          "postgres-filesystem",
			MetadataStore: MetadataPostgres,
			ContentStore:  ContentFilesystem,
			ShareName:     "/export",
			UseCache:      false,
		},
		// S3 backends (requires Docker/Localstack, cache always enabled)
		{
			Name:          "memory-s3",
			MetadataStore: MetadataMemory,
			ContentStore:  ContentS3,
			ShareName:     "/export",
			UseCache:      true, // Always use cache with S3
		},
		{
			Name:          "badger-s3",
			MetadataStore: MetadataBadger,
			ContentStore:  ContentS3,
			ShareName:     "/export",
			UseCache:      true, // Always use cache with S3
		},
		// PostgreSQL + S3 (requires both Docker services)
		{
			Name:          "postgres-s3",
			MetadataStore: MetadataPostgres,
			ContentStore:  ContentS3,
			ShareName:     "/export",
			UseCache:      true, // Always use cache with S3
		},
	}
}

// LocalConfigurations returns configurations that don't require Docker.
func LocalConfigurations() []*TestConfig {
	var configs []*TestConfig
	for _, c := range AllConfigurations() {
		if !c.RequiresDocker() {
			configs = append(configs, c)
		}
	}
	return configs
}

// S3Configurations returns configurations that use S3.
func S3Configurations() []*TestConfig {
	var configs []*TestConfig
	for _, c := range AllConfigurations() {
		if c.RequiresS3() {
			configs = append(configs, c)
		}
	}
	return configs
}

// PostgresConfigurations returns configurations that use PostgreSQL.
func PostgresConfigurations() []*TestConfig {
	var configs []*TestConfig
	for _, c := range AllConfigurations() {
		if c.RequiresPostgres() {
			configs = append(configs, c)
		}
	}
	return configs
}

// CachedConfigurations returns configurations with cache enabled.
func CachedConfigurations() []*TestConfig {
	var configs []*TestConfig
	for _, c := range AllConfigurations() {
		if c.UseCache {
			configs = append(configs, c)
		}
	}
	return configs
}

// GetConfiguration returns a specific configuration by name.
func GetConfiguration(name string) *TestConfig {
	for _, config := range AllConfigurations() {
		if config.Name == name {
			return config
		}
	}
	return nil
}
