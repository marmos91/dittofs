package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// persistentHalf decodes the little-endian uint64 the FileID counter writes
// into bytes 0..7, mirroring GenerateFileID's encoding.
func persistentHalf(fileID [16]byte) uint64 {
	return binary.LittleEndian.Uint64(fileID[0:8])
}

// durableWithFileID builds a persisted durable handle whose FileID persistent
// half equals id (volatile half zeroed, as real persisted records store it).
func durableWithFileID(idStr string, id uint64) *lock.PersistedDurableHandle {
	var fileID [16]byte
	binary.LittleEndian.PutUint64(fileID[0:8], id)
	return &lock.PersistedDurableHandle{ID: idStr, FileID: fileID}
}

// TestSeedFileIDFromDurableHandles verifies the post-restart FileID counter is
// bumped past the highest persisted durable-handle FileID, so a fresh CREATE
// cannot re-mint a FileID a V1 reconnect would match (MS-SMB2 §3.3.5.9.7).
func TestSeedFileIDFromDurableHandles(t *testing.T) {
	store := newMockDurableStore()
	_ = store.PutDurableHandle(context.Background(), durableWithFileID("a", 5))
	_ = store.PutDurableHandle(context.Background(), durableWithFileID("b", 42))
	_ = store.PutDurableHandle(context.Background(), durableWithFileID("c", 17))

	h := NewHandler() // starts nextFileID at 1
	h.SeedFileIDFromDurableHandles(context.Background(), store)

	// First CREATE after seeding must land strictly above the max persisted
	// FileID (42), so it cannot collide with any reclaimable durable open.
	got := persistentHalf(h.GenerateFileID())
	if got <= 42 {
		t.Fatalf("first FileID after seed = %d, want > 42 (max persisted)", got)
	}
	if got != 43 {
		t.Fatalf("first FileID after seed = %d, want 43 (max+1)", got)
	}
}

// TestSeedFileIDFromDurableHandles_NeverLowers verifies the counter is only
// bumped upward: a store whose handles are all below the current counter
// leaves it untouched.
func TestSeedFileIDFromDurableHandles_NeverLowers(t *testing.T) {
	store := newMockDurableStore()
	_ = store.PutDurableHandle(context.Background(), durableWithFileID("a", 3))

	h := NewHandler()
	// Advance the counter well past the persisted max.
	for i := 0; i < 100; i++ {
		h.GenerateFileID()
	}
	before := h.nextFileID.Load()

	h.SeedFileIDFromDurableHandles(context.Background(), store)

	if after := h.nextFileID.Load(); after != before {
		t.Fatalf("counter moved from %d to %d; seed must never lower it", before, after)
	}
}

// TestSeedFileIDFromDurableHandles_EmptyAndNil verifies the no-op paths: an
// empty store and a nil store both leave the default start intact and never
// panic.
func TestSeedFileIDFromDurableHandles_EmptyAndNil(t *testing.T) {
	h := NewHandler()
	start := h.nextFileID.Load()

	h.SeedFileIDFromDurableHandles(context.Background(), newMockDurableStore())
	if got := h.nextFileID.Load(); got != start {
		t.Fatalf("empty store changed counter to %d, want %d", got, start)
	}

	h.SeedFileIDFromDurableHandles(context.Background(), nil)
	if got := h.nextFileID.Load(); got != start {
		t.Fatalf("nil store changed counter to %d, want %d", got, start)
	}
}
