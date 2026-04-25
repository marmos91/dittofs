package badger

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// newRollupTestStore creates a fresh BadgerMetadataStore under t.TempDir().
// Badger is embedded — no external service required — so this test can run
// without the `integration` build tag, matching the memory-store rollup
// tests for uniform coverage.
func newRollupTestStore(t *testing.T) *BadgerMetadataStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	store, err := NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestBadgerRollupStore_Suite exercises the shared RollupStore conformance
// suite against the Badger backend. Proves atomic-monotone semantics
// (INV-03) on top of Badger's MVCC transactions.
func TestBadgerRollupStore_Suite(t *testing.T) {
	s := newRollupTestStore(t)
	metadata.RunRollupStoreSuite(t, s)
}
