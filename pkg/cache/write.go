package cache

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/cache/wal"
)

// minSliceCapacity is the minimum initial capacity for new slices.
// This reduces reallocations for sequential writes by pre-allocating a larger buffer.
// Value chosen to cover most NFS write patterns (32KB-1MB writes).
const minSliceCapacity = 256 * 1024 // 256KB

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
//   - payloadID: Unique identifier for the file content
//   - chunkIdx: Which 64MB chunk this write belongs to
//   - data: The bytes to write (copied into cache, safe to modify after call)
//   - offset: Byte offset within the chunk (0 to ChunkSize-1)
//
// Errors:
//   - ErrInvalidOffset: offset + len(data) exceeds ChunkSize
//   - ErrCacheClosed: cache has been closed
//   - context.Canceled/DeadlineExceeded: context was cancelled
func (c *Cache) WriteSlice(ctx context.Context, payloadID string, chunkIdx uint32, data []byte, offset uint32) error {
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
			c.evictLRUUntilFits(ctx, dataLen)
		}
	}

	entry := c.getFileEntry(payloadID)
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
	extResult := c.extendAdjacentSlice(chunk, offset, data)
	if extResult.extended {
		// Log extension to WAL if enabled
		// We log the NEW data portion as a separate slice entry for crash recovery.
		// On recovery, multiple overlapping slices are merged with newest-wins semantics.
		if c.persister != nil {
			extSlice := Slice{
				ID:        uuid.New().String(),
				Offset:    offset,
				Length:    uint32(len(data)),
				Data:      data, // Note: uses original data, not a copy
				State:     SliceStatePending,
				CreatedAt: time.Now(),
			}
			entry.mu.Unlock()
			walEntry := c.sliceToWALEntry(payloadID, chunkIdx, &extSlice)
			err := c.persister.AppendSlice(walEntry)
			entry.mu.Lock()
			if err != nil {
				return err
			}
		}
		return nil
	}

	// Create new slice with pre-allocated capacity to reduce reallocations
	sliceID := uuid.New().String()

	// Pre-allocate with minSliceCapacity to reduce reallocs for sequential writes
	capacity := max(len(data), minSliceCapacity)
	sliceData := make([]byte, len(data), capacity)
	copy(sliceData, data)

	slice := Slice{
		ID:        sliceID,
		Offset:    offset,
		Length:    uint32(len(data)),
		Data:      sliceData,
		State:     SliceStatePending,
		CreatedAt: time.Now(),
	}

	// Prepend to slices (newest first)
	chunk.slices = append([]Slice{slice}, chunk.slices...)
	c.totalSize.Add(uint64(len(data)))

	// Persist to WAL if enabled
	if c.persister != nil {
		// Note: We release the file lock before persister write to avoid deadlock
		// This is safe because persister has its own mutex
		entry.mu.Unlock()
		walEntry := c.sliceToWALEntry(payloadID, chunkIdx, &slice)
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
// The entry contains the file context (payloadID, chunk index) required
// for recovery.
func (c *Cache) sliceToWALEntry(payloadID string, chunkIdx uint32, slice *Slice) *wal.SliceEntry {
	return &wal.SliceEntry{
		PayloadID: payloadID,
		ChunkIdx:  chunkIdx,
		Slice:     *slice, // Embed directly - same type
	}
}

// extendResult contains information about a slice extension for WAL logging.
type extendResult struct {
	extended bool   // Whether a slice was extended
	sliceID  string // ID of the extended slice
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
// Returns extendResult with extended=true and the slice ID if extended.
func (c *Cache) extendAdjacentSlice(chunk *chunkEntry, offset uint32, data []byte) extendResult {
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
			// Pre-grow capacity using exponential growth to minimize reallocations.
			// For sequential writes, this typically results in O(log n) allocations
			// instead of O(n/256KB) allocations.
			if cap(slice.Data)-len(slice.Data) < len(data) {
				// Exponential growth: double capacity, minimum 256KB
				newCap := max(cap(slice.Data)*2, len(slice.Data)+len(data), minSliceCapacity)
				// Cap at 64MB (chunk size) to avoid over-allocation
				newCap = min(newCap, int(ChunkSize))
				newData := make([]byte, len(slice.Data), newCap)
				copy(newData, slice.Data)
				slice.Data = newData
			}
			slice.Data = append(slice.Data, data...)
			slice.Length += uint32(len(data))
			c.totalSize.Add(uint64(len(slice.Data) - oldLen))
			return extendResult{extended: true, sliceID: slice.ID}
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
			return extendResult{extended: true, sliceID: slice.ID}
		}
	}

	return extendResult{extended: false}
}
