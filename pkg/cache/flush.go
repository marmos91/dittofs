package cache

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// Flush Coordination
// ============================================================================

// GetDirtySlices returns all pending (unflushed) slices for a file, ready for upload.
//
// Coalesces adjacent writes first, then returns slices sorted by (ChunkIndex, Offset).
// The returned PendingSlice.Data references the cache's internal buffer - do not modify.
func (c *Cache) GetDirtySlices(ctx context.Context, payloadID string) ([]PendingSlice, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return nil, ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(payloadID)

	// Coalesce under write lock, then collect under read lock
	entry.mu.Lock()
	if len(entry.chunks) == 0 {
		entry.mu.Unlock()
		return nil, ErrFileNotInCache
	}
	for _, chunk := range entry.chunks {
		coalesceChunk(chunk)
	}
	entry.mu.Unlock()

	entry.mu.RLock()
	defer entry.mu.RUnlock()

	var result []PendingSlice
	for chunkIdx, chunk := range entry.chunks {
		for _, s := range chunk.slices {
			if s.State != SliceStatePending {
				continue
			}
			result = append(result, PendingSlice{
				Slice:      s,
				ChunkIndex: chunkIdx,
			})
		}
	}

	slices.SortFunc(result, func(a, b PendingSlice) int {
		return cmp.Or(cmp.Compare(a.ChunkIndex, b.ChunkIndex), cmp.Compare(a.Offset, b.Offset))
	})

	return result, nil
}

// MarkSliceFlushed marks a slice as successfully flushed to the block store.
//
// This should be called by the TransferManager after successfully uploading a slice's data.
// The slice transitions from SliceStatePending to SliceStateFlushed, making it eligible
// for LRU eviction when cache pressure requires freeing memory.
//
// Parameters:
//   - payloadID: Unique identifier for the file content
//   - sliceID: The ID from PendingSlice returned by GetDirtySlices
//   - blockRefs: Block references from the block store (used for future reads)
//
// Errors:
//   - ErrSliceNotFound: slice ID doesn't exist (possibly already evicted or removed)
//   - ErrCacheClosed: cache has been closed
//   - context.Canceled/DeadlineExceeded: context was cancelled
func (c *Cache) MarkSliceFlushed(ctx context.Context, payloadID string, sliceID string, blockRefs []BlockRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(payloadID)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	// Find the slice
	for _, chunk := range entry.chunks {
		for i := range chunk.slices {
			if chunk.slices[i].ID == sliceID {
				chunk.slices[i].State = SliceStateFlushed
				if blockRefs != nil {
					chunk.slices[i].BlockRefs = make([]BlockRef, len(blockRefs))
					copy(chunk.slices[i].BlockRefs, blockRefs)
				}
				return nil
			}
		}
	}

	return ErrSliceNotFound
}

// ============================================================================
// Write Optimization
// ============================================================================

// CoalesceWrites merges adjacent pending writes into fewer slices.
//
// Called automatically by GetDirtySlices before returning pending slices.
// Reduces the number of block store uploads by combining adjacent writes.
//
// Errors:
//   - ErrFileNotInCache: file has no cached data
//   - ErrCacheClosed: cache has been closed
//   - context.Canceled/DeadlineExceeded: context was cancelled
func (c *Cache) CoalesceWrites(ctx context.Context, payloadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(payloadID)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	if len(entry.chunks) == 0 {
		return ErrFileNotInCache
	}

	for _, chunk := range entry.chunks {
		coalesceChunk(chunk)
	}

	return nil
}

// coalesceChunk merges adjacent/overlapping pending slices within a chunk.
//
// Only pending slices are merged - flushed slices are preserved as-is.
// After merging, the chunk contains fewer, larger slices which reduces
// the number of block store operations needed during flush.
func coalesceChunk(chunk *chunkEntry) {
	pending, flushed := partitionByState(chunk.slices, SliceStatePending)
	if len(pending) <= 1 {
		return
	}

	slices.SortFunc(pending, func(a, b Slice) int {
		return cmp.Compare(a.Offset, b.Offset)
	})

	chunk.slices = append(mergeAdjacent(pending), flushed...)
}

// partitionByState splits slices into matching and non-matching groups.
func partitionByState(slices []Slice, state SliceState) (matching, other []Slice) {
	for _, s := range slices {
		if s.State == state {
			matching = append(matching, s)
		} else {
			other = append(other, s)
		}
	}
	return
}

// mergeAdjacent combines overlapping/adjacent slices into fewer slices.
// Input must be sorted by offset.
func mergeAdjacent(slices []Slice) []Slice {
	result := []Slice{newSliceFrom(slices[0])}

	for _, s := range slices[1:] {
		last := &result[len(result)-1]
		if s.Offset <= last.Offset+last.Length {
			extendSlice(last, &s)
		} else {
			result = append(result, newSliceFrom(s))
		}
	}
	return result
}

// newSliceFrom creates a deep copy with a new ID.
func newSliceFrom(s Slice) Slice {
	data := make([]byte, len(s.Data))
	copy(data, s.Data)
	return Slice{
		ID:        uuid.New().String(),
		Offset:    s.Offset,
		Length:    s.Length,
		Data:      data,
		State:     SliceStatePending,
		CreatedAt: time.Now(),
	}
}

// extendSlice merges src data into dst, growing the buffer if needed.
func extendSlice(dst, src *Slice) {
	newEnd := max(dst.Offset+dst.Length, src.Offset+src.Length)
	newLen := newEnd - dst.Offset

	if newLen > uint32(len(dst.Data)) {
		grown := make([]byte, newLen)
		copy(grown, dst.Data)
		dst.Data = grown
		dst.Length = newLen
	}

	copy(dst.Data[src.Offset-dst.Offset:], src.Data)
}
