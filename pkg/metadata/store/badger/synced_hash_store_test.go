package badger

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestBadgerSyncedHashStore_Suite exercises the shared SyncedHashStore
// conformance suite against the Badger backend. Proves idempotency,
// isolation between hashes, and no-panic / no-error invariants under
// concurrent Mark/Delete on top of Badger's MVCC transactions.
//
// Reuses newRollupTestStore (defined in rollup_test.go) — its return
// type is the broader *BadgerMetadataStore, which already satisfies
// metadata.SyncedHashStore via the var _ assertion in
// synced_hash_store.go.
func TestBadgerSyncedHashStore_Suite(t *testing.T) {
	s := newRollupTestStore(t)
	metadata.RunSyncedHashStoreSuite(t, s)
}
