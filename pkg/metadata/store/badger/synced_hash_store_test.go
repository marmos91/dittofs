package badger

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// newSyncedHashTestStore opens a fresh Badger store in a temp dir for the
// SyncedHashStore/enumerator suites. The return type is the broader
// *BadgerMetadataStore, which satisfies metadata.SyncedHashStore via the
// var _ assertion in synced_hash_store.go.
func newSyncedHashTestStore(t *testing.T) *BadgerMetadataStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	s, err := NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestBadgerSyncedHashStore_Suite exercises the shared SyncedHashStore
// conformance suite against the Badger backend. Proves idempotency,
// isolation between hashes, and no-panic / no-error invariants under
// concurrent Mark/Delete on top of Badger's MVCC transactions.
func TestBadgerSyncedHashStore_Suite(t *testing.T) {
	s := newSyncedHashTestStore(t)
	metadata.RunSyncedHashStoreSuite(t, s)
}

// TestBadgerSyncedHashEnumerator_Suite exercises the LIST-free GC sweep's
// EnumerateSynced contract against the Badger backend, including its
// timestamped marker value (first-write-wins nanos) and prefix-scan
// enumeration.
func TestBadgerSyncedHashEnumerator_Suite(t *testing.T) {
	s := newSyncedHashTestStore(t)
	metadata.RunSyncedHashEnumeratorSuite(t, s)
}
