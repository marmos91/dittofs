package memory

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

// TestMemorySyncedHashStore_Suite runs the shared conformance suite
// against the memory backend, so every SyncedHashStore implementation
// (memory, badger, postgres) exercises the same contract from a single
// source of truth.
func TestMemorySyncedHashStore_Suite(t *testing.T) {
	s := NewMemoryMetadataStoreWithDefaults()
	storetest.RunSyncedHashStoreSuite(t, s)
}

// TestMemorySyncedHashEnumerator_Suite exercises the LIST-free GC sweep's
// EnumerateSynced contract against the memory backend.
func TestMemorySyncedHashEnumerator_Suite(t *testing.T) {
	s := NewMemoryMetadataStoreWithDefaults()
	storetest.RunSyncedHashEnumeratorSuite(t, s)
}
