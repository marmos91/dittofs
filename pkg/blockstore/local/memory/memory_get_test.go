package memory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/memory"
)

// TestMemoryStore_Get_ReturnsErrChunkNotFound asserts the documented stub
// behavior of MemoryStore.Get (Phase 16 Plan 01): the memory backend has
// no CAS chunk layer, so every Get returns blockstore.ErrChunkNotFound.
// The method exists only to satisfy the LocalStore interface; Phase 17
// may expand it when the unified BlockStore interface lands.
func TestMemoryStore_Get_ReturnsErrChunkNotFound(t *testing.T) {
	s := memory.New()
	defer func() { _ = s.Close() }()

	var h blockstore.ContentHash
	h[0] = 0xAB
	h[31] = 0xCD

	_, err := s.Get(context.Background(), h)
	if !errors.Is(err, blockstore.ErrChunkNotFound) {
		t.Fatalf("MemoryStore.Get: want ErrChunkNotFound, got %v", err)
	}
}
