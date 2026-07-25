package sqlite_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// TestNarrowUpdateAffectsRow guards the narrow-write benchmark: a narrow UPDATE
// that matched zero rows would be just as fast and prove nothing. This asserts
// the id binding the benchmark uses actually hits a seeded row.
func TestNarrowUpdateAffectsRow(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.NewSQLiteMetadataStore(ctx,
		&sqlite.SQLiteMetadataStoreConfig{Path: t.TempDir() + "/m.db", AutoMigrate: true}, sqliteTestCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	const share = "/hot"
	if _, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	h, err := store.GenerateHandle(ctx, share, "/hot/f")
	if err != nil {
		t.Fatal(err)
	}
	_, id, err := metadata.DecodeFileHandle(h)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutFile(ctx, &metadata.File{ShareName: share, Path: "/hot/f", ID: id,
		FileAttr: metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644, UID: 1000, GID: 1000}}); err != nil {
		t.Fatal(err)
	}

	res, err := store.DBForBench().ExecContext(ctx, narrowUpdate, int64(4096), int64(1_700_000_000), id.String(), share)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("narrow UPDATE affected %d rows, want 1 — benchmark id binding wrong, results are a no-op", n)
	}
}
