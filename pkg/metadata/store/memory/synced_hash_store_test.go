package memory

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestMemorySyncedHashStore_Suite runs the shared conformance suite
// against the memory backend, so every SyncedHashStore implementation
// (memory, badger, postgres) exercises the same contract from a single
// source of truth.
func TestMemorySyncedHashStore_Suite(t *testing.T) {
	s := NewMemoryMetadataStoreWithDefaults()
	metadata.RunSyncedHashStoreSuite(t, s)
}
