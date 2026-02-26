//go:build integration

package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

func TestConformance(t *testing.T) {
	// Skip if no PostgreSQL connection string is provided
	connStr := os.Getenv("DITTOFS_TEST_POSTGRES_DSN")
	if connStr == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL conformance tests")
	}

	storetest.RunConformanceSuite(t, func(t *testing.T) metadata.MetadataStore {
		cfg := &postgres.PostgresMetadataStoreConfig{
			Host:        "localhost",
			Port:        5432,
			Database:    "dittofs_test",
			User:        "postgres",
			Password:    "postgres",
			SSLMode:     "disable",
			AutoMigrate: true,
		}

		// If a full DSN is provided, use defaults but override host/port/etc.
		// The DSN environment variable is just a signal to run the test.

		caps := metadata.FilesystemCapabilities{
			MaxReadSize:         1048576,
			PreferredReadSize:   65536,
			MaxWriteSize:        1048576,
			PreferredWriteSize:  65536,
			MaxFileSize:         9223372036854775807,
			MaxFilenameLen:      255,
			MaxPathLen:          4096,
			MaxHardLinkCount:    32767,
			SupportsHardLinks:   true,
			SupportsSymlinks:    true,
			CaseSensitive:       true,
			CasePreserving:      true,
			TimestampResolution: 1,
		}

		store, err := postgres.NewPostgresMetadataStore(context.Background(), cfg, caps)
		if err != nil {
			t.Fatalf("NewPostgresMetadataStore() failed: %v", err)
		}
		t.Cleanup(func() {
			store.Close()
		})
		return store
	})
}
