package cache

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/cache/wal"
)

// ============================================================================
// Write Operations
// ============================================================================

// WriteSlice writes a slice to the cache.
//
// Optimization: If the write is adjacent to an existing pending slice (sequential write),
// we extend that slice instead of creating a new one. This is critical for performance
// since NFS clients write in 16KB-32KB chunks, so a 10MB file = 320 writes.
// Without this optimization, we'd create 320 slices instead of 1.
func (c *Cache) WriteSlice(ctx context.Context, fileHandle string, chunkIdx uint32, data []byte, offset uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return ErrCacheClosed
	}
	c.globalMu.RUnlock()

	// Validate parameters
	if offset+uint32(len(data)) > ChunkSize {
		return ErrInvalidOffset
	}

	// Enforce maxSize by evicting LRU flushed data if needed
	if c.maxSize > 0 {
		dataLen := uint64(len(data))
		if c.totalSize.Load()+dataLen > c.maxSize {
			c.evictLRUUntilFits(dataLen)
		}
	}

	entry := c.getFileEntry(fileHandle)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	// Update LRU access time
	c.touchFile(entry)

	// Ensure chunk exists
	chunk, exists := entry.chunks[chunkIdx]
	if !exists {
		chunk = &chunkEntry{
			slices: make([]Slice, 0),
		}
		entry.chunks[chunkIdx] = chunk
	}

	// Try to extend an existing adjacent pending slice (sequential write optimization)
	if c.tryExtendAdjacentSlice(chunk, offset, data) {
		return nil
	}

	// Create new slice
	sliceID := uuid.New().String()

	slice := Slice{
		ID:        sliceID,
		Offset:    offset,
		Length:    uint32(len(data)),
		Data:      make([]byte, len(data)),
		State:     SliceStatePending,
		CreatedAt: time.Now(),
	}
	copy(slice.Data, data)

	// Prepend to slices (newest first)
	chunk.slices = append([]Slice{slice}, chunk.slices...)
	c.totalSize.Add(uint64(len(data)))

	// Persist to WAL if enabled
	if c.persister.IsEnabled() {
		// Note: We release the file lock before persister write to avoid deadlock
		// This is safe because persister has its own mutex
		entry.mu.Unlock()
		walEntry := c.sliceToWALEntry(fileHandle, chunkIdx, &slice)
		err := c.persister.AppendSlice(walEntry)
		entry.mu.Lock() // Re-acquire for deferred unlock
		if err != nil {
			return err
		}
	}

	return nil
}

// sliceToWALEntry converts a cache.Slice to wal.SliceEntry (types unified via aliases).
func (c *Cache) sliceToWALEntry(fileHandle string, chunkIdx uint32, slice *Slice) *wal.SliceEntry {
	return &wal.SliceEntry{
		FileHandle: fileHandle,
		ChunkIdx:   chunkIdx,
		SliceID:    slice.ID,
		Offset:     slice.Offset,
		Length:     slice.Length,
		Data:       slice.Data,
		State:      slice.State,     // Same type via alias
		CreatedAt:  slice.CreatedAt,
		BlockRefs:  slice.BlockRefs, // Direct assignment - same type via alias
	}
}

// tryExtendAdjacentSlice attempts to extend an existing pending slice.
// Uses Go's append() for amortized O(1) growth on sequential appends.
// Returns true if extended, false if no adjacent slice found.
func (c *Cache) tryExtendAdjacentSlice(chunk *chunkEntry, offset uint32, data []byte) bool {
	writeEnd := offset + uint32(len(data))

	for i := range chunk.slices {
		slice := &chunk.slices[i]
		if slice.State != SliceStatePending {
			continue
		}

		sliceEnd := slice.Offset + slice.Length

		// Case 1: Appending (write starts where slice ends)
		if offset == sliceEnd {
			oldLen := len(slice.Data)
			slice.Data = append(slice.Data, data...)
			slice.Length += uint32(len(data))
			c.totalSize.Add(uint64(len(slice.Data) - oldLen))
			return true
		}

		// Case 2: Prepending (write ends where slice starts)
		if writeEnd == slice.Offset {
			oldLen := len(slice.Data)
			newData := make([]byte, len(data)+len(slice.Data))
			copy(newData, data)
			copy(newData[len(data):], slice.Data)
			slice.Data = newData
			slice.Offset = offset
			slice.Length += uint32(len(data))
			c.totalSize.Add(uint64(len(newData) - oldLen))
			return true
		}
	}

	return false
}
