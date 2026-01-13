//go:build e2e

// Package framework provides the test infrastructure for DittoFS e2e tests.
package framework

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	metadatapostgres "github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// MetadataStoreType represents the type of metadata store.
type MetadataStoreType string

const (
	MetadataMemory   MetadataStoreType = "memory"
	MetadataBadger   MetadataStoreType = "badger"
	MetadataPostgres MetadataStoreType = "postgres"
)

// TestConfig holds the configuration for a test run.
// Note: Content storage is now handled entirely by the SliceCache,
// which is automatically created by the Registry. No content store
// configuration is needed.
type TestConfig struct {
	Name          string
	MetadataStore MetadataStoreType
	ShareName     string

	// S3-specific fields (for future block storage integration)
	S3Client *s3.Client
	S3Bucket string

	// PostgreSQL-specific fields (set by postgres setup)
	PostgresConfig *PostgresConfig
}

// String returns a human-readable representation of the configuration.
func (tc *TestConfig) String() string {
	return string(tc.MetadataStore)
}

// RequiresDocker returns true if this configuration requires Docker.
func (tc *TestConfig) RequiresDocker() bool {
	return tc.MetadataStore == MetadataPostgres
}

// RequiresS3 returns true if this configuration requires S3/Localstack.
// Currently always false since cache-only model doesn't use S3 content stores.
func (tc *TestConfig) RequiresS3() bool {
	return false
}

// RequiresPostgres returns true if this configuration requires PostgreSQL.
func (tc *TestConfig) RequiresPostgres() bool {
	return tc.MetadataStore == MetadataPostgres
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

// AllConfigurations returns all test configurations.
// Note: Content storage is handled by the Registry's auto-created SliceCache.
// Requires Docker for PostgreSQL configurations.
func AllConfigurations() []*TestConfig {
	return []*TestConfig{
		// Memory metadata (fast, volatile)
		{
			Name:          "memory",
			MetadataStore: MetadataMemory,
			ShareName:     "/export",
		},
		// BadgerDB metadata (persistent, embedded)
		{
			Name:          "badger",
			MetadataStore: MetadataBadger,
			ShareName:     "/export",
		},
		// PostgreSQL metadata (requires Docker)
		{
			Name:          "postgres",
			MetadataStore: MetadataPostgres,
			ShareName:     "/export",
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
// Currently empty since cache-only model doesn't use S3 content stores.
func S3Configurations() []*TestConfig {
	return []*TestConfig{}
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

// GetConfiguration returns a specific configuration by name.
func GetConfiguration(name string) *TestConfig {
	for _, config := range AllConfigurations() {
		if config.Name == name {
			return config
		}
	}
	return nil
}
