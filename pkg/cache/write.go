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

// WriteSlice writes data to the cache at the specified chunk and offset.
//
// This is the primary write path for all file data. The slice is stored in memory
// with SliceStatePending until flushed to the block store via MarkSliceFlushed.
//
// Sequential Write Optimization:
// If the write is adjacent to an existing pending slice, we extend that slice
// instead of creating a new one. This is critical for performance since NFS clients
// write in 16KB-32KB chunks, so a 10MB file = 320 writes. Without this optimization,
// we'd create 320 slices instead of 1.
//
// Parameters:
//   - fileHandle: Unique identifier for the file (from metadata store)
//   - chunkIdx: Which 64MB chunk this write belongs to
//   - data: The bytes to write (copied into cache, safe to modify after call)
//   - offset: Byte offset within the chunk (0 to ChunkSize-1)
//
// Errors:
//   - ErrInvalidOffset: offset + len(data) exceeds ChunkSize
//   - ErrCacheClosed: cache has been closed
//   - context.Canceled/DeadlineExceeded: context was cancelled
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

	// Extend an existing adjacent pending slice if possible (sequential write optimization)
	if c.extendAdjacentSlice(chunk, offset, data) {
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
	if c.persister != nil {
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

// sliceToWALEntry wraps a Slice with WAL context for persistence.
//
// Since SliceEntry embeds Slice directly, no field copying is needed.
// The entry contains the file context (handle, chunk index) required
// for recovery.
func (c *Cache) sliceToWALEntry(fileHandle string, chunkIdx uint32, slice *Slice) *wal.SliceEntry {
	return &wal.SliceEntry{
		FileHandle: fileHandle,
		ChunkIdx:   chunkIdx,
		Slice:      *slice, // Embed directly - same type
	}
}

// extendAdjacentSlice extends an existing pending slice if the new write is adjacent.
//
// This implements the sequential write optimization. When NFS clients write
// sequentially (which is the common case), we extend the existing slice rather
// than creating a new one. This dramatically reduces slice count and improves
// flush performance.
//
// Two cases are handled:
//   - Appending: New write starts exactly where existing slice ends
//   - Prepending: New write ends exactly where existing slice starts
//
// Uses Go's append() for amortized O(1) growth on sequential appends.
//
// Returns true if a slice was extended, false if no adjacent slice was found.
func (c *Cache) extendAdjacentSlice(chunk *chunkEntry, offset uint32, data []byte) bool {
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
