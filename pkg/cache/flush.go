package cache

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// Flush Coordination
// ============================================================================

// GetDirtySlices returns all pending slices for a file.
func (c *Cache) GetDirtySlices(ctx context.Context, fileHandle string) ([]PendingSlice, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return nil, ErrCacheClosed
	}
	c.globalMu.RUnlock()

	if err := c.CoalesceWrites(ctx, fileHandle); err != nil && err != ErrFileNotInCache {
		return nil, err
	}

	entry := c.getFileEntry(fileHandle)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	if len(entry.chunks) == 0 {
		return nil, ErrFileNotInCache
	}

	var result []PendingSlice

	for chunkIdx, chunk := range entry.chunks {
		for _, slice := range chunk.slices {
			if slice.State == SliceStatePending {
				result = append(result, PendingSlice{
					ID:         slice.ID,
					ChunkIndex: chunkIdx,
					Offset:     slice.Offset,
					Length:     slice.Length,
					Data:       slice.Data,
					CreatedAt:  slice.CreatedAt,
				})
			}
		}
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].ChunkIndex != result[j].ChunkIndex {
			return result[i].ChunkIndex < result[j].ChunkIndex
		}
		return result[i].Offset < result[j].Offset
	})

	return result, nil
}

// MarkSliceFlushed marks a slice as successfully flushed.
func (c *Cache) MarkSliceFlushed(ctx context.Context, fileHandle string, sliceID string, blockRefs []BlockRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(fileHandle)
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
func (c *Cache) CoalesceWrites(ctx context.Context, fileHandle string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(fileHandle)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	if len(entry.chunks) == 0 {
		return ErrFileNotInCache
	}

	for chunkIdx := range entry.chunks {
		if err := c.coalesceChunk(entry.chunks[chunkIdx]); err != nil {
			return err
		}
	}

	return nil
}

// coalesceChunk merges adjacent pending slices within a chunk.
func (c *Cache) coalesceChunk(chunk *chunkEntry) error {
	if len(chunk.slices) <= 1 {
		return nil
	}

	var pending []Slice
	var other []Slice

	for _, slice := range chunk.slices {
		if slice.State == SliceStatePending {
			pending = append(pending, slice)
		} else {
			other = append(other, slice)
		}
	}

	if len(pending) <= 1 {
		return nil
	}

	sort.Slice(pending, func(i, j int) bool {
		return pending[i].Offset < pending[j].Offset
	})

	merged := make([]Slice, 0)
	var current *Slice

	for _, slice := range pending {
		if current == nil {
			newSlice := Slice{
				ID:        uuid.New().String(),
				Offset:    slice.Offset,
				Length:    slice.Length,
				Data:      make([]byte, slice.Length),
				State:     SliceStatePending,
				CreatedAt: time.Now(),
			}
			copy(newSlice.Data, slice.Data)
			current = &newSlice
			continue
		}

		currentEnd := current.Offset + current.Length

		if slice.Offset <= currentEnd {
			sliceEnd := slice.Offset + slice.Length
			newEnd := max(currentEnd, sliceEnd)
			newLength := newEnd - current.Offset

			if newLength > uint32(len(current.Data)) {
				newData := make([]byte, newLength)
				copy(newData, current.Data)
				current.Data = newData
				current.Length = newLength
			}

			dstOffset := slice.Offset - current.Offset
			copy(current.Data[dstOffset:], slice.Data)
		} else {
			merged = append(merged, *current)
			newSlice := Slice{
				ID:        uuid.New().String(),
				Offset:    slice.Offset,
				Length:    slice.Length,
				Data:      make([]byte, slice.Length),
				State:     SliceStatePending,
				CreatedAt: time.Now(),
			}
			copy(newSlice.Data, slice.Data)
			current = &newSlice
		}
	}

	if current != nil {
		merged = append(merged, *current)
	}

	chunk.slices = append(merged, other...)
	return nil
}
