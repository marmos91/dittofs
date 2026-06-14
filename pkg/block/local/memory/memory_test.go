package memory_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockstoretest"
	"github.com/marmos91/dittofs/pkg/block/local/memory"
)

// TestMemoryStore_BlockStoreConformance runs the unified
// BlockStoreConformance suite against the local in-memory backend.
//
// -07 lands the BlockStoreAppend-contributed methods
// (Put/Has/Walk/Head/Delete/GetRange/AppendWrite/DeleteAppendLog) on
// *memory.MemoryStore; this wiring is checked in now so the conformance
// contract is documented at the call site before the implementation
// closes the gap (per mega-PR commit ordering — interfaces wired
// first, implementations follow).
func TestMemoryStore_BlockStoreConformance(t *testing.T) {
	factory := func(t *testing.T) (block.Store, func()) {
		t.Helper()
		s := memory.New()
		cleanup := func() {}
		return s, cleanup
	}
	blockstoretest.BlockStoreConformance(t, factory)
}

// TestMemoryStore_BlockStoreAppendConformance runs the random-write
// absorber suite from pkg/block/blockstoretest against the local
// in-memory backend. Three scenarios `t.Skip` on the interface-only
// surface (require fs-internal probes); the two portable scenarios
// (AppendLogRoundTrip, ConcurrentStorm) exercise the public surface
// via Walk-polling.
//
// -07 lands AppendWrite + DeleteAppendLog on *memory.MemoryStore so
// this test can run.
func TestMemoryStore_BlockStoreAppendConformance(t *testing.T) {
	factory := func(t *testing.T) (block.BlockStoreAppend, func()) {
		t.Helper()
		s := memory.New()
		cleanup := func() {}
		return s, cleanup
	}
	blockstoretest.BlockStoreAppendConformance(t, factory)
}
