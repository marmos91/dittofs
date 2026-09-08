//go:build integration

package metabench

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// BenchmarkWriteComparePostgres is the postgres cell of the write-path
// comparison. It runs the same data-write RMW as the embedded backends, but at
// postgres's DEFAULT durability: RelaxedDurability is left false, so every
// transaction commits with synchronous_commit=on and waits for a WAL fsync.
// That is deliberate — this cell exists to measure the per-commit fsync, which
// is the one cost the store layer cannot shape away, so equalizing it to
// fsync-free the way the badger and sqlite cells do would erase the subject.
//
// It reads no knobs of its own. commit_delay and commit_siblings are server
// GUCs, so an A/B over them is applied to the server between runs
// (ALTER SYSTEM SET ... ; SELECT pg_reload_conf()) and needs no code here.
// Both are SIGHUP-scoped, so a reload is enough and no restart is required.
//
//	ALTER SYSTEM SET commit_delay = 50; SELECT pg_reload_conf();
//	DITTOFS_LOGGING_LEVEL=ERROR DITTOFS_TEST_POSTGRES_DSN=1 \
//	  go test -tags=integration -run '^$' -bench WriteComparePostgres \
//	  -benchtime 20000x -cpu 8 ./pkg/metadata/store/metabench/
//
// Report the server's commit_delay alongside the number: the benchmark cannot
// see it, so a result recorded without it cannot be placed in the A/B.
func BenchmarkWriteComparePostgres(b *testing.B) {
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		b.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping postgres write-path cell")
	}

	cfg := &postgres.PostgresMetadataStoreConfig{
		Host:        "localhost",
		Port:        5432,
		Database:    "dittofs_test",
		User:        "postgres",
		Password:    "postgres",
		SSLMode:     "disable",
		AutoMigrate: true,
		// RelaxedDurability deliberately left false: see the comment above.
	}
	store, err := postgres.NewPostgresMetadataStore(context.Background(), cfg, caps())
	if err != nil {
		b.Fatalf("NewPostgresMetadataStore: %v", err)
	}

	// The database persists across runs, so a fixed share name would collide
	// with every previous run's seeded population.
	benchmarkDataWriteRMW(b, store, fmt.Sprintf("/hot-%d", time.Now().UnixNano()))
}

// interface assertion: the cell is only meaningful if the store it opens really
// is the one the service layer commits through.
var _ metadata.Store = (*postgres.PostgresMetadataStore)(nil)
