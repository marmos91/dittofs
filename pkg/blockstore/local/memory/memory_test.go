package memory_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore/local"
	"github.com/marmos91/dittofs/pkg/blockstore/local/localtest"
	"github.com/marmos91/dittofs/pkg/blockstore/local/memory"
)

func TestMemoryStoreConformance(t *testing.T) {
	factory := func(t *testing.T) local.LocalStore {
		t.Helper()
		return memory.New()
	}
	localtest.RunSuite(t, factory)
}

// TestMemoryStore_GetConformance wires the LocalStore.Get conformance
// suite against memory.MemoryStore. The memory backend does not
// implement StoreChunk, so RunGetSuite auto-skips the round-trip +
// fresh-allocation subtests and exercises only the missing-hash →
// ErrChunkNotFound assertion — matching the documented stub behavior of
// MemoryStore.Get.
func TestMemoryStore_GetConformance(t *testing.T) {
	factory := func(t *testing.T) local.LocalStore {
		t.Helper()
		return memory.New()
	}
	localtest.RunGetSuite(t, factory)
}
