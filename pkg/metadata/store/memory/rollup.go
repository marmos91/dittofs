package memory

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// Compile-time assertion: the memory engine implements RollupStore.
var _ metadata.RollupStore = (*MemoryMetadataStore)(nil)

// SetRollupOffset atomically advances payloadID -> newOffset iff newOffset >=
// the currently-stored value. Returns the PREVIOUS stored value on success.
//
// On regression (newOffset < stored), returns (storedOffset,
// metadata.ErrRollupOffsetRegression); the stored value is UNCHANGED.
// INV-03 is enforced here — the read, compare, and write all happen under
// s.rollupMu.
func (s *MemoryMetadataStore) SetRollupOffset(ctx context.Context, payloadID string, newOffset uint64) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.rollupMu.Lock()
	defer s.rollupMu.Unlock()
	if s.rollupOffsets == nil {
		s.rollupOffsets = make(map[string]uint64)
	}
	prev := s.rollupOffsets[payloadID]
	if newOffset < prev {
		// Regression rejected; leave stored value untouched.
		return prev, metadata.ErrRollupOffsetRegression
	}
	s.rollupOffsets[payloadID] = newOffset
	return prev, nil
}

// GetRollupOffset returns the persisted rollup_offset for payloadID, or
// (0, nil) if unset. Matches the contract in metadata.RollupStore.
func (s *MemoryMetadataStore) GetRollupOffset(ctx context.Context, payloadID string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.rollupMu.RLock()
	defer s.rollupMu.RUnlock()
	return s.rollupOffsets[payloadID], nil
}
