//go:build e2e

package e2e

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

// MetadataStoreType represents the type of metadata store
type MetadataStoreType string

const (
	MetadataMemory   MetadataStoreType = "memory"
	MetadataBadger   MetadataStoreType = "badger"
	MetadataPostgres MetadataStoreType = "postgres"
)

// ContentStoreType represents the type of content store
type ContentStoreType string

const (
	ContentMemory     ContentStoreType = "memory"
	ContentFilesystem ContentStoreType = "filesystem"
	ContentS3         ContentStoreType = "s3"
)

// TestContextProvider is an interface for providing test context dependencies
type TestContextProvider interface {
	CreateTempDir(prefix string) string
	GetConfig() *TestConfig
	GetPort() int
}

// TestConfig holds the configuration for a test run
type TestConfig struct {
	Name          string
	MetadataStore MetadataStoreType
	ContentStore  ContentStoreType
	ShareName     string
	UseCache      bool // Enable cache layer (recommended for S3 with large files)

	// S3-specific fields (set by localstack setup)
	s3Client *s3.Client
	s3Bucket string

	// PostgreSQL-specific fields (set by postgres setup)
	postgresConfig *PostgresConfig
}

// String returns a string representation of the configuration
func (tc *TestConfig) String() string {
	if tc.UseCache {
		return fmt.Sprintf("%s/%s-cached", tc.MetadataStore, tc.ContentStore)
	}
	return fmt.Sprintf("%s/%s", tc.MetadataStore, tc.ContentStore)
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

// CreateMetadataStore creates a metadata store based on the configuration
func (tc *TestConfig) CreateMetadataStore(ctx context.Context, testCtx TestContextProvider) (metadata.MetadataStore, error) {
	switch tc.MetadataStore {
	case MetadataMemory:
		return metadatamemory.NewMemoryMetadataStoreWithDefaults(), nil

	case MetadataBadger:
		dbPath := filepath.Join(testCtx.CreateTempDir("dittofs-badger-*"), "metadata.db")
		store, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
		if err != nil {
			return nil, fmt.Errorf("failed to create badger metadata store: %w", err)
		}
		return store, nil

	case MetadataPostgres:
		// PostgreSQL requires setup via SetupPostgresConfig
		config := testCtx.GetConfig()
		if config.postgresConfig == nil {
			return nil, fmt.Errorf("PostgreSQL config not initialized (postgres container not running?)")
		}

		pgConfig := &metadatapostgres.PostgresMetadataStoreConfig{
			Host:        config.postgresConfig.Host,
			Port:        config.postgresConfig.Port,
			Database:    config.postgresConfig.Database,
			User:        config.postgresConfig.User,
			Password:    config.postgresConfig.Password,
			SSLMode:     "disable",
			MaxConns:    10,
			MinConns:    2,
			AutoMigrate: true, // Enable auto-migration for tests
		}

		// Set reasonable default capabilities for testing
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

// CreateContentStore creates a content store based on the configuration
func (tc *TestConfig) CreateContentStore(ctx context.Context, testCtx TestContextProvider) (content.ContentStore, error) {
	switch tc.ContentStore {
	case ContentMemory:
		store, err := contentmemory.NewMemoryContentStore(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to create memory content store: %w", err)
		}
		return store, nil

	case ContentFilesystem:
		contentPath := testCtx.CreateTempDir("dittofs-content-*")
		store, err := contentfs.NewFSContentStore(ctx, contentPath)
		if err != nil {
			return nil, fmt.Errorf("failed to create filesystem content store: %w", err)
		}
		return store, nil

	case ContentS3:
		// S3 requires localstack setup
		config := testCtx.GetConfig()
		if config.s3Client == nil {
			return nil, fmt.Errorf("S3 client not initialized (localstack not running?)")
		}

		// Use bucket name from SetupS3Config if available, otherwise create default name
		bucketName := config.s3Bucket
		if bucketName == "" {
			bucketName = fmt.Sprintf("dittofs-e2e-test-%d", testCtx.GetPort())
			config.s3Bucket = bucketName
		}

		store, err := contents3.NewS3ContentStore(ctx, contents3.S3ContentStoreConfig{
			Client:        config.s3Client,
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

// AllConfigurations returns all test configurations to run, including S3 and PostgreSQL.
// S3 tests require Localstack to be running (use run-e2e.sh --s3).
// PostgreSQL tests require Docker or external PostgreSQL (use run-e2e.sh --postgres).
// Use LocalConfigurations() to run only non-S3, non-PostgreSQL tests.
//
// Note: S3 tests without cache are included for testing fallback behavior
// with small files. For large file tests (>5MB), use S3CachedConfigurations()
// which enables the cache layer for efficient writes.
func AllConfigurations() []*TestConfig {
	return []*TestConfig{
		// Local backends (no external dependencies)
		{
			Name:          "memory-memory",
			MetadataStore: MetadataMemory,
			ContentStore:  ContentMemory,
			ShareName:     "/export",
		},
		{
			Name:          "memory-filesystem",
			MetadataStore: MetadataMemory,
			ContentStore:  ContentFilesystem,
			ShareName:     "/export",
		},
		{
			Name:          "badger-filesystem",
			MetadataStore: MetadataBadger,
			ContentStore:  ContentFilesystem,
			ShareName:     "/export",
		},
		// PostgreSQL backends (requires Docker or external PostgreSQL)
		{
			Name:          "postgres-memory",
			MetadataStore: MetadataPostgres,
			ContentStore:  ContentMemory,
			ShareName:     "/export",
		},
		{
			Name:          "postgres-filesystem",
			MetadataStore: MetadataPostgres,
			ContentStore:  ContentFilesystem,
			ShareName:     "/export",
		},
		// S3 backends without cache (requires Localstack)
		// Good for testing small files and verifying fallback behavior
		{
			Name:          "memory-s3",
			MetadataStore: MetadataMemory,
			ContentStore:  ContentS3,
			ShareName:     "/export",
		},
		{
			Name:          "badger-s3",
			MetadataStore: MetadataBadger,
			ContentStore:  ContentS3,
			ShareName:     "/export",
		},
		// S3 backends with cache (requires Localstack)
		// Recommended for large file tests (10MB+) to avoid timeout
		{
			Name:          "memory-s3-cached",
			MetadataStore: MetadataMemory,
			ContentStore:  ContentS3,
			ShareName:     "/export",
			UseCache:      true,
		},
		{
			Name:          "badger-s3-cached",
			MetadataStore: MetadataBadger,
			ContentStore:  ContentS3,
			ShareName:     "/export",
			UseCache:      true,
		},
		// PostgreSQL + S3 (requires both Docker/PostgreSQL and Localstack)
		{
			Name:          "postgres-s3",
			MetadataStore: MetadataPostgres,
			ContentStore:  ContentS3,
			ShareName:     "/export",
		},
		{
			Name:          "postgres-s3-cached",
			MetadataStore: MetadataPostgres,
			ContentStore:  ContentS3,
			ShareName:     "/export",
			UseCache:      true,
		},
	}
}

// LocalConfigurations returns configurations that don't require external services.
// Use this when Localstack is not available.
func LocalConfigurations() []*TestConfig {
	return []*TestConfig{
		{
			Name:          "memory-memory",
			MetadataStore: MetadataMemory,
			ContentStore:  ContentMemory,
			ShareName:     "/export",
		},
		{
			Name:          "memory-filesystem",
			MetadataStore: MetadataMemory,
			ContentStore:  ContentFilesystem,
			ShareName:     "/export",
		},
		{
			Name:          "badger-filesystem",
			MetadataStore: MetadataBadger,
			ContentStore:  ContentFilesystem,
			ShareName:     "/export",
		},
	}
}

// S3Configurations returns S3 configurations without cache (requires Localstack).
// Use for small file tests where fallback behavior should be tested.
func S3Configurations() []*TestConfig {
	return []*TestConfig{
		{
			Name:          "memory-s3",
			MetadataStore: MetadataMemory,
			ContentStore:  ContentS3,
			ShareName:     "/export",
		},
		{
			Name:          "badger-s3",
			MetadataStore: MetadataBadger,
			ContentStore:  ContentS3,
			ShareName:     "/export",
		},
	}
}

// S3CachedConfigurations returns S3 configurations with cache enabled (requires Localstack).
// Use for large file tests (10MB+) where cache is necessary for efficient writes.
func S3CachedConfigurations() []*TestConfig {
	return []*TestConfig{
		{
			Name:          "memory-s3-cached",
			MetadataStore: MetadataMemory,
			ContentStore:  ContentS3,
			ShareName:     "/export",
			UseCache:      true,
		},
		{
			Name:          "badger-s3-cached",
			MetadataStore: MetadataBadger,
			ContentStore:  ContentS3,
			ShareName:     "/export",
			UseCache:      true,
		},
	}
}

// AllS3Configurations returns all S3 configurations (both with and without cache).
func AllS3Configurations() []*TestConfig {
	return append(S3Configurations(), S3CachedConfigurations()...)
}

// PostgresConfigurations returns PostgreSQL configurations (requires Docker or external PostgreSQL).
func PostgresConfigurations() []*TestConfig {
	return []*TestConfig{
		{
			Name:          "postgres-memory",
			MetadataStore: MetadataPostgres,
			ContentStore:  ContentMemory,
			ShareName:     "/export",
		},
		{
			Name:          "postgres-filesystem",
			MetadataStore: MetadataPostgres,
			ContentStore:  ContentFilesystem,
			ShareName:     "/export",
		},
	}
}

// PostgresS3Configurations returns PostgreSQL + S3 configurations (requires both PostgreSQL and Localstack).
func PostgresS3Configurations() []*TestConfig {
	return []*TestConfig{
		{
			Name:          "postgres-s3",
			MetadataStore: MetadataPostgres,
			ContentStore:  ContentS3,
			ShareName:     "/export",
		},
		{
			Name:          "postgres-s3-cached",
			MetadataStore: MetadataPostgres,
			ContentStore:  ContentS3,
			ShareName:     "/export",
			UseCache:      true,
		},
	}
}

// AllPostgresConfigurations returns all PostgreSQL configurations (with and without S3).
func AllPostgresConfigurations() []*TestConfig {
	return append(PostgresConfigurations(), PostgresS3Configurations()...)
}

// GetConfiguration returns a specific configuration by name
func GetConfiguration(name string) *TestConfig {
	for _, config := range AllConfigurations() {
		if config.Name == name {
			return config
		}
	}
	return nil
}
