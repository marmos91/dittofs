package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// TestStartOffsetMigration_PreExistingRowClaimsChunkStart reads a row written
// before the manifest carried an in-chunk start offset and pins what it means
// afterwards: the claim still begins at the chunk's first byte.
//
// The database is rewound to the schema that predates the column — the two
// columns dropped, the recorded version rolled back — and the row is written
// through raw SQL in the shape that schema allowed. Reopening runs the migration
// over it, so what the store reads back is a genuinely pre-change row rather
// than a fresh one with the field left unset.
func TestStartOffsetMigration_PreExistingRowClaimsChunkStart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "startoff.db")

	cfg := &sqlite.SQLiteMetadataStoreConfig{Path: dbPath, AutoMigrate: true}
	store, err := sqlite.NewSQLiteMetadataStore(ctx, cfg, sqliteTestCapabilities())
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close after first open: %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite directly: %v", err)
	}
	fileID := "00000000-0000-0000-0000-0000000002a1"
	now := time.Now().UTC()
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{q: `ALTER TABLE file_blocks DROP COLUMN start_offset`},
		{q: `ALTER TABLE file_block_refs DROP COLUMN start_offset`},
		{q: `DELETE FROM schema_migrations WHERE version >= 9`},
		{
			q: `INSERT INTO file_blocks (id, hash, data_size, ref_count, last_access, created_at, state)
			    VALUES (?1, ?2, ?3, 1, ?4, ?4, 2)`,
			args: []any{"legacy-payload/4096", "0000000000000000000000000000000000000000000000000000000000000001", int32(4096), now},
		},
		{
			q:    `INSERT INTO file_block_refs (file_id, "offset", size, hash) VALUES (?1, ?2, ?3, ?4)`,
			args: []any{fileID, int64(4096), int32(4096), make([]byte, block.HashSize)},
		},
	} {
		if _, err := db.Exec(stmt.q, stmt.args...); err != nil {
			t.Fatalf("rewind step %q: %v", stmt.q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close direct handle: %v", err)
	}

	store, err = sqlite.NewSQLiteMetadataStore(ctx, cfg, sqliteTestCapabilities())
	if err != nil {
		t.Fatalf("reopen and migrate: %v", err)
	}
	defer func() { _ = store.Close() }()

	rows, err := store.ListFileChunks(ctx, "legacy-payload")
	if err != nil {
		t.Fatalf("ListFileChunks: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListFileChunks returned %d rows, want 1", len(rows))
	}
	if rows[0].StartOffset != 0 {
		t.Errorf("pre-change row: StartOffset = %d, want 0 — it claimed its chunk "+
			"from the first byte before the column existed and must go on doing so",
			rows[0].StartOffset)
	}
	if rows[0].DataSize != 4096 {
		t.Errorf("pre-change row: DataSize = %d, want 4096", rows[0].DataSize)
	}
}
