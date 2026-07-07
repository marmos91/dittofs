package badger

import (
	"context"
	"path/filepath"
	"testing"

	badger "github.com/dgraph-io/badger/v4"
)

// TestBadgerStore_StrictDurabilityForcesSyncWrites pins the #583/#588
// crash-consistency posture for the DEFAULT (strict) store: SyncWrites=true, so
// every commit fsyncs and no metadata write can be lost between flush
// boundaries. Badger's own default is SyncWrites=false, which loses rolled-up
// FileChunk / FileAttr.Blocks rows on a kill-9 and makes the CAS read path
// return silent zeros — this test guards against that regressing.
func TestBadgerStore_StrictDurabilityForcesSyncWrites(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "badger")

	store, err := NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if !store.db.Opts().SyncWrites {
		t.Fatalf("badger SyncWrites: got false, want true for strict store (crash-consistency regression — see #583)")
	}
}

// TestBadgerStore_StrictEnforcedOnCustomConfig asserts the #588 follow-up: a
// strict store (RelaxedDurability=false) forces SyncWrites=true even when the
// caller passes custom options with SyncWrites explicitly false, so an operator
// tuning unrelated knobs cannot accidentally disable crash durability.
func TestBadgerStore_StrictEnforcedOnCustomConfig(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "badger")

	customOpts := badger.DefaultOptions(dbPath).WithSyncWrites(false)
	store, err := NewBadgerMetadataStore(context.Background(), BadgerMetadataStoreConfig{
		DBPath:        dbPath,
		BadgerOptions: &customOpts,
		// RelaxedDurability defaults false → strict.
	})
	if err != nil {
		t.Fatalf("NewBadgerMetadataStore custom: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if !store.db.Opts().SyncWrites {
		t.Fatalf("badger SyncWrites under strict custom config: got false, want true (#588 enforcement bypass)")
	}
}

// TestBadgerStore_RelaxedDurabilityDisablesSyncWrites is the #1573 Wall 1
// counterpart: a relaxed store opens with SyncWrites=false so namespace-op
// commits do not fsync inline. Durability for data-paired writes is
// re-established by the explicit db.Sync() on the durable path (covered by
// relaxed_durability_test.go), not by SyncWrites.
func TestBadgerStore_RelaxedDurabilityDisablesSyncWrites(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "badger")

	store, err := NewBadgerMetadataStore(context.Background(), BadgerMetadataStoreConfig{
		DBPath:            dbPath,
		RelaxedDurability: true,
	})
	if err != nil {
		t.Fatalf("NewBadgerMetadataStore relaxed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if store.db.Opts().SyncWrites {
		t.Fatalf("badger SyncWrites under relaxed config: got true, want false (#1573 Wall 1)")
	}
}
