package fs

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// TestFSStore_ListUnsynced_SQLiteIndex is the restart-seed regression guard for
// production metadata backends. An FSStore whose LocalChunkIndex is a real
// SQLite store holds unsynced chunks only in the logblob index (no CAS files).
// ListUnsynced must re-discover them via FSStore.Walk, which enumerates the
// index through the localChunkWalker capability. Before SQLite implemented
// WalkLocalLocations the type-assertion in Walk failed, Walk silently skipped
// the index, and ListUnsynced returned nothing — so the syncer never re-seeded
// crash-stranded chunks on restart. This test fails without that method.
func TestFSStore_ListUnsynced_SQLiteIndex(t *testing.T) {
	ctx := context.Background()

	cfg := &sqlite.SQLiteMetadataStoreConfig{
		Path:        filepath.Join(t.TempDir(), "restart-seed.db"),
		AutoMigrate: true,
	}
	idx, err := sqlite.NewSQLiteMetadataStore(ctx, cfg, metadata.FilesystemCapabilities{})
	if err != nil {
		t.Fatalf("NewSQLiteMetadataStore: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	// SyncedHashStore with nothing marked synced → every logblob chunk is
	// unsynced. Wire the SQLite store as the LocalChunkIndex so the seed path
	// must walk a production backend's index, not the memory one.
	bc := newFSStoreForTestWithFBS(t, idx, FSStoreOptions{})

	want := seedChunks(t, bc, 3)

	got, errs := collectIter(bc.ListUnsynced(ctx))
	for _, e := range errs {
		if e != nil {
			t.Fatalf("ListUnsynced yielded error: %v", e)
		}
	}
	if !hashSetEqual(got, want) {
		t.Fatalf("ListUnsynced returned %d chunks, want %d (SQLite index not walked — restart-seed broken)", len(got), len(want))
	}
}
