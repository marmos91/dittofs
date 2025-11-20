package e2e

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/pkg/store/content"
	contentfs "github.com/marmos91/dittofs/pkg/store/content/fs"
	contentmemory "github.com/marmos91/dittofs/pkg/store/content/memory"
	contents3 "github.com/marmos91/dittofs/pkg/store/content/s3"
	"github.com/marmos91/dittofs/pkg/store/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/store/metadata/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/store/metadata/memory"
)

// MetadataStoreType represents the type of metadata store
type MetadataStoreType string

const (
	MetadataMemory MetadataStoreType = "memory"
	MetadataBadger MetadataStoreType = "badger"
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

	// S3-specific fields (set by localstack setup)
	s3Client *s3.Client
	s3Bucket string
}

// String returns a string representation of the configuration
func (tc *TestConfig) String() string {
	return fmt.Sprintf("%s/%s", tc.MetadataStore, tc.ContentStore)
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

	default:
		return nil, fmt.Errorf("unknown metadata store type: %s", tc.MetadataStore)
	}
}

// CreateContentStore creates a content store based on the configuration
func (tc *TestConfig) CreateContentStore(ctx context.Context, testCtx TestContextProvider) (content.WritableContentStore, error) {
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

		bucketName := fmt.Sprintf("dittofs-e2e-test-%d", testCtx.GetPort())
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

		// Store bucket name for cleanup
		config.s3Bucket = bucketName

		return store, nil

	default:
		return nil, fmt.Errorf("unknown content store type: %s", tc.ContentStore)
	}
}

// AllConfigurations returns all test configurations to run
func AllConfigurations() []*TestConfig {
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

// S3Configurations returns configurations that use S3 (requires localstack)
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

// GetConfiguration returns a specific configuration by name
func GetConfiguration(name string) *TestConfig {
	// Check standard configurations
	for _, config := range AllConfigurations() {
		if config.Name == name {
			return config
		}
	}

	// Check S3 configurations
	for _, config := range S3Configurations() {
		if config.Name == name {
			return config
		}
	}

	return nil
}
