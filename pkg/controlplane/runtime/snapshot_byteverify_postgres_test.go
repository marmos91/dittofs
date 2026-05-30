//go:build integration

package runtime

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	metadatapostgres "github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// init installs the postgres backend into the byte-verify matrix. Built only
// under -tags=integration so the memory+badger cases stay runnable under plain
// `go test`. The actual DSN gate lives in open(): if DITTOFS_TEST_POSTGRES_DSN
// is unset the case Skips rather than fails, keeping the integration binary
// runnable without a database.
func init() {
	postgresByteVerifyBackend = &byteVerifyBackend{
		name: "postgres",
		open: openPostgresByteVerify,
	}
}

func openPostgresByteVerify(t *testing.T) (metadata.MetadataStore, string) {
	t.Helper()
	dsn := os.Getenv("DITTOFS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping postgres byte-verify case")
	}
	cfg := parsePostgresDSN(t, dsn)
	cfg.AutoMigrate = true
	caps := byteVerifyPostgresCaps()
	ctx := context.Background()

	// Reset-then-reopen for a clean isolated schema (mirrors the postgres
	// conformance factory): open, truncate any prior data, re-open so the
	// singleton capability/store_id rows are re-seeded on the empty schema.
	seed, err := metadatapostgres.NewPostgresMetadataStore(ctx, cfg, caps)
	if err != nil {
		t.Fatalf("NewPostgresMetadataStore (seed): %v", err)
	}
	if err := seed.Reset(ctx); err != nil {
		_ = seed.Close()
		t.Fatalf("postgres Reset: %v", err)
	}
	_ = seed.Close()

	store, err := metadatapostgres.NewPostgresMetadataStore(ctx, cfg, caps)
	if err != nil {
		t.Fatalf("NewPostgresMetadataStore (reopen): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, "postgres"
}

// parsePostgresDSN turns a libpq key=value DSN
// ("host=localhost port=5432 dbname=... user=... password=... sslmode=...")
// into the typed config the postgres store constructor expects. The store
// factory hardcodes dittofs_test elsewhere, so this test reads the dbname
// straight from the DSN env to keep its usage isolated to the dedicated
// dittofs_test_matrix database.
func parsePostgresDSN(t *testing.T, dsn string) *metadatapostgres.PostgresMetadataStoreConfig {
	t.Helper()
	cfg := &metadatapostgres.PostgresMetadataStoreConfig{
		Host:    "localhost",
		Port:    5432,
		SSLMode: "disable",
	}
	for _, field := range strings.Fields(dsn) {
		kv := strings.SplitN(field, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, val := kv[0], kv[1]
		switch key {
		case "host":
			cfg.Host = val
		case "port":
			p, err := strconv.Atoi(val)
			if err != nil {
				t.Fatalf("parse DSN port %q: %v", val, err)
			}
			cfg.Port = p
		case "dbname", "database":
			cfg.Database = val
		case "user":
			cfg.User = val
		case "password":
			cfg.Password = val
		case "sslmode", "ssl_mode":
			cfg.SSLMode = val
		}
	}
	if cfg.Database == "" {
		t.Fatalf("DITTOFS_TEST_POSTGRES_DSN %q has no dbname", dsn)
	}
	return cfg
}

func byteVerifyPostgresCaps() metadata.FilesystemCapabilities {
	return metadata.FilesystemCapabilities{
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
}
