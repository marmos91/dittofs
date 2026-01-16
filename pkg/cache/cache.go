// Package cache implements buffering for content stores.
//
// Cache provides a block-aligned caching layer for the Chunk/Block storage
// model. It buffers writes directly into 4MB block buffers and serves reads
// from those buffers (zero-filling gaps for sparse files).
//
// Key Design Principles:
//   - Block-aligned: Writes go directly to 4MB block buffers
//   - Storage-backend agnostic: Cache doesn't know about S3/filesystem/etc.
//   - Mandatory: All content operations go through the cache
//   - Immediate eviction: After block upload, buffer can be freed immediately
//   - Coverage tracking: Bitmap tracks which bytes are written (sparse files)
//
// Architecture:
//
//	Cache (business logic + storage)
//	    - In-memory block buffers (4MB each)
//	    - Coverage bitmap per block
//	    - Optional WAL backing for crash recovery
//
// See docs/ARCHITECTURE.md for the full Chunk/Block model.
package cache

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/pkg/cache/wal"
)

// ============================================================================
// Internal Types
// ============================================================================

// chunkEntry holds all block buffers for a single chunk.
type chunkEntry struct {
	blocks map[uint32]*blockBuffer // blockIdx -> buffer
}

// fileEntry holds all cached data for a single file.
type fileEntry struct {
	mu         sync.RWMutex
	chunks     map[uint32]*chunkEntry // chunkIndex -> chunkEntry
	lastAccess time.Time              // LRU tracking
}

// ============================================================================
// Cache Implementation
// ============================================================================

// Cache is the mandatory cache layer for all content operations.
//
// It uses 4MB block buffers as first-class citizens, storing data directly
// at the correct position. Optional WAL persistence can be enabled via MmapPersister.
//
// Thread Safety:
// Uses two-level locking for efficiency:
//   - globalMu: Protects the files map
//   - per-file mutexes: Protect individual file operations
//
// This allows concurrent operations on different files.
type Cache struct {
	globalMu  sync.RWMutex
	files     map[string]*fileEntry
	maxSize   uint64
	totalSize atomic.Uint64
	closed    bool

	// Pending size tracking for backpressure
	// Pending = blocks that haven't been uploaded yet (can't be evicted)
	pendingSize    atomic.Uint64
	maxPendingSize uint64 // 0 = use default (256MB)

	// WAL persistence (nil = disabled)
	persister *wal.MmapPersister
}

// New creates a new in-memory cache with no persistence.
//
// Parameters:
//   - maxSize: Maximum total cache size in bytes. Use 0 for unlimited.
func New(maxSize uint64) *Cache {
	return &Cache{
		files:   make(map[string]*fileEntry),
		maxSize: maxSize,
	}
}

// NewWithWal creates a new cache with WAL persistence for crash recovery.
//
// The persister is used to persist cache operations. On creation, existing
// data is recovered from the persister.
//
// Example:
//
//	persister, err := wal.NewMmapPersister("/var/lib/dittofs/wal")
//	if err != nil {
//	    return err
//	}
//	cache, err := cache.NewWithWal(1<<30, persister)
//
// Parameters:
//   - maxSize: Maximum total cache size in bytes. Use 0 for unlimited.
//   - persister: MmapPersister for crash recovery
func NewWithWal(maxSize uint64, persister *wal.MmapPersister) (*Cache, error) {
	c := &Cache{
		files:     make(map[string]*fileEntry),
		maxSize:   maxSize,
		persister: persister,
	}

	// Recover existing data
	if err := c.recoverFromWal(); err != nil {
		return nil, err
	}

	return c, nil
}

// recoverFromWal recovers cache state from the WAL persister.
//
// Called automatically during NewWithWal. Replays all WAL entries to restore
// block buffers to their pre-crash state. Blocks that were already uploaded to
// S3 (tracked in WAL) are marked as Uploaded to avoid re-upload.
//
// After recovery, unflushed blocks (those not in UploadedBlocks) can be
// re-uploaded via the TransferManager.
func (c *Cache) recoverFromWal() error {
	result, err := c.persister.Recover()
	if err != nil {
		return err
	}

	for _, entry := range result.Entries {
		fileEntry := c.getFileEntry(entry.PayloadID)
		fileEntry.mu.Lock()

		chunk, exists := fileEntry.chunks[entry.ChunkIdx]
		if !exists {
			chunk = &chunkEntry{
				blocks: make(map[uint32]*blockBuffer),
			}
			fileEntry.chunks[entry.ChunkIdx] = chunk
		}

		// Check if this block was already uploaded to S3
		blockKey := wal.BlockKey{
			PayloadID: entry.PayloadID,
			ChunkIdx:  entry.ChunkIdx,
			BlockIdx:  entry.BlockIdx,
		}
		wasUploaded := result.UploadedBlocks[blockKey]

		// Determine initial state: Uploaded if already in S3, Pending otherwise
		initialState := BlockStatePending
		if wasUploaded {
			initialState = BlockStateUploaded
		}

		// Get or create block buffer
		block, exists := chunk.blocks[entry.BlockIdx]
		if !exists {
			block = &blockBuffer{
				data:     make([]byte, BlockSize),
				coverage: newCoverageBitmap(),
				state:    initialState,
			}
			chunk.blocks[entry.BlockIdx] = block
			// Track BlockSize for new block buffers (memory allocation tracking)
			c.totalSize.Add(BlockSize)
			// Only count against pendingSize if block is Pending (needs upload)
			if initialState == BlockStatePending {
				c.pendingSize.Add(BlockSize)
			}
		}

		// Copy data to block buffer at correct offset
		copy(block.data[entry.OffsetInBlock:], entry.Data)
		markCoverage(block.coverage, entry.OffsetInBlock, uint32(len(entry.Data)))

		// Update data size
		if end := entry.OffsetInBlock + uint32(len(entry.Data)); end > block.dataSize {
			block.dataSize = end
		}

		fileEntry.mu.Unlock()
	}

	return nil
}

// getFileEntry returns or creates a file entry for the given payload ID.
//
// Thread-safe: Uses double-checked locking for efficiency. First attempts
// a read lock, then upgrades to write lock only if the entry doesn't exist.
//
// The returned entry has its own mutex for fine-grained locking.
func (c *Cache) getFileEntry(payloadID string) *fileEntry {
	c.globalMu.RLock()
	entry, exists := c.files[payloadID]
	c.globalMu.RUnlock()

	if exists {
		return entry
	}

	c.globalMu.Lock()
	defer c.globalMu.Unlock()

	// Double-check after acquiring write lock
	if entry, exists = c.files[payloadID]; exists {
		return entry
	}

	entry = &fileEntry{
		chunks:     make(map[uint32]*chunkEntry),
		lastAccess: time.Now(),
	}
	c.files[payloadID] = entry
	return entry
}

// touchFile updates the last access time for LRU tracking.
// Must be called with entry.mu held (read or write lock).
func (c *Cache) touchFile(entry *fileEntry) {
	entry.lastAccess = time.Now()
}

// getOrCreateBlock returns or creates a block buffer for the given coordinates.
// Returns the block buffer and whether it was newly created (or re-allocated after detach).
// Must be called with entry.mu held for write.
func (c *Cache) getOrCreateBlock(entry *fileEntry, chunkIdx, blockIdx uint32) (*blockBuffer, bool) {
	chunk, exists := entry.chunks[chunkIdx]
	if !exists {
		chunk = &chunkEntry{
			blocks: make(map[uint32]*blockBuffer),
		}
		entry.chunks[chunkIdx] = chunk
	}

	block, exists := chunk.blocks[blockIdx]
	if !exists {
		block = &blockBuffer{
			data:     make([]byte, BlockSize),
			coverage: newCoverageBitmap(),
			state:    BlockStatePending,
		}
		chunk.blocks[blockIdx] = block
		return block, true // newly created
	}

	// If block exists but data was detached (nil), re-allocate the buffer.
	// This happens when reading data that was previously uploaded via DetachBlockForUpload.
	if block.data == nil {
		block.data = make([]byte, BlockSize)
		block.coverage = newCoverageBitmap()
		// Keep existing state (likely Uploaded) - don't reset to Pending
		return block, true // treated as new for memory tracking
	}

	return block, false // existing with data
}

// blockExists checks if a block buffer exists WITH data for the given coordinates.
// Returns false if the block entry exists but data was detached (nil).
// Must be called with entry.mu held (read or write lock).
func (c *Cache) blockExists(entry *fileEntry, chunkIdx, blockIdx uint32) bool {
	chunk, exists := entry.chunks[chunkIdx]
	if !exists {
		return false
	}
	block, exists := chunk.blocks[blockIdx]
	if !exists {
		return false
	}
	// Block without data (detached) doesn't count as existing for memory purposes
	return block.data != nil
}
