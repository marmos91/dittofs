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

// newPostgresStoreFactory returns a factory that creates a fresh
// PostgresMetadataStore for each subtest. Shared by both conformance
// suites so the config and capabilities stay consistent.
func newPostgresStoreFactory() func(t *testing.T) metadata.MetadataStore {
	return func(t *testing.T) metadata.MetadataStore {
		cfg := &postgres.PostgresMetadataStoreConfig{
			Host:        "localhost",
			Port:        5432,
			Database:    "dittofs_test",
			User:        "postgres",
			Password:    "postgres",
			SSLMode:     "disable",
			AutoMigrate: true,
		}

		caps := metadata.FilesystemCapabilities{
			MaxReadSize:         1048576,
			PreferredReadSize:   1048576,
			MaxWriteSize:        1048576,
			PreferredWriteSize:  1048576,
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
	}
}

func TestConformance(t *testing.T) {
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL conformance tests")
	}

	storetest.RunConformanceSuite(t, newPostgresStoreFactory())
}

func TestBackupConformance(t *testing.T) {
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL backup conformance tests")
	}

	storetest.RunBackupConformanceSuite(t, newPostgresStoreFactory())
}

func TestResetThenRestoreConformance(t *testing.T) {
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL reset-then-restore conformance tests")
	}

	storetest.ResetThenRestoreConformance(t, newPostgresStoreFactory())
}
