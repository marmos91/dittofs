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

// TestBlockLayoutConformance runs the per-share BlockLayout conformance
// scenarios against the Postgres metadata store (Phase 14 Plan 01,
// MIG-03 / D-A6). Gated by DITTOFS_TEST_POSTGRES_DSN matching the
// existing convention (postgres_conformance_test.go) — skip cleanly
// outside the dedicated CI lane.
func TestBlockLayoutConformance(t *testing.T) {
	connStr := os.Getenv("DITTOFS_TEST_POSTGRES_DSN")
	if connStr == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL BlockLayout conformance")
	}

	storetest.RunBlockLayoutSuite(t, func(t *testing.T) metadata.MetadataStore {
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
			_ = store.Close()
		})
		return store
	})
}
