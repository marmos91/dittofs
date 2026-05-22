package badger

import (
	"context"
	"path/filepath"
	"testing"
)

// TestBadgerStore_SyncWritesEnabledByDefault asserts the crash-consistency
// fix from #583: badger.DefaultOptions has SyncWrites=false, which causes
// FileBlock rows (and every other metadata write) to live in the memtable
// + WAL buffer rather than fsyncing to disk on commit. A `kill -9`
// between flush boundaries loses every metadata write since the last
// sync, including the rollup-produced FileBlock manifest rows — the
// engine's CAS read path then falls into the sparse-block zero-fill
// branch and returns silent zeros for files whose chunks survived on
// disk.
//
// The store constructor (NewBadgerMetadataStore) now applies
// WithSyncWrites(true) over the default options. This test pins that
// invariant.
func TestBadgerStore_SyncWritesEnabledByDefault(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "badger")

	store, err := NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	opts := store.db.Opts()
	if !opts.SyncWrites {
		t.Fatalf("badger SyncWrites: got false, want true (crash-consistency regression — see #583)")
	}
}
