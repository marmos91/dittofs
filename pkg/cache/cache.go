// Package cache implements buffering for content stores.
//
// Cache provides a slice-aware caching layer for the Chunk/Slice/Block
// storage model. It buffers writes as slices and serves reads by merging
// slices (newest-wins semantics).
//
// Key Design Principles:
//   - Slice-aware: WriteSlice/ReadSlice API maps directly to data model
//   - Storage-backend agnostic: Cache doesn't know about S3/filesystem/etc.
//   - Mandatory: All content operations go through the cache
//   - Write coalescing: Adjacent writes merged before flush
//   - Newest-wins reads: Overlapping slices resolved by creation time
//
// Architecture:
//
//	Cache (business logic + storage)
//	    - In-memory data structures
//	    - Optional mmap backing (future)
//
// See docs/ARCHITECTURE.md for the full Chunk/Slice/Block model.
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

// chunkEntry holds all slices for a single chunk.
type chunkEntry struct {
	slices []Slice // Ordered newest-first (prepended on add)
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
// It understands slices as first-class citizens and stores them directly
// in memory. Optional WAL persistence can be enabled via a Persister.
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

	// WAL persistence (optional, nil uses NullPersister)
	persister wal.Persister
}

// New creates a new in-memory cache with no persistence.
//
// Parameters:
//   - maxSize: Maximum total cache size in bytes. Use 0 for unlimited.
func New(maxSize uint64) *Cache {
	return &Cache{
		files:     make(map[string]*fileEntry),
		maxSize:   maxSize,
		persister: wal.NewNullPersister(),
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
//   - persister: WAL persister for crash recovery (create externally)
func NewWithWal(maxSize uint64, persister wal.Persister) (*Cache, error) {
	c := &Cache{
		files:     make(map[string]*fileEntry),
		maxSize:   maxSize,
		persister: persister,
	}

	// Recover existing data if persister is enabled
	if persister.IsEnabled() {
		if err := c.recoverFromWal(); err != nil {
			return nil, err
		}
	}

	return c, nil
}

// recoverFromWal recovers cache state from the WAL persister.
func (c *Cache) recoverFromWal() error {
	entries, err := c.persister.Recover()
	if err != nil {
		return err
	}

	for _, entry := range entries {
		fileEntry := c.getFileEntry(entry.FileHandle)
		fileEntry.mu.Lock()

		chunk, exists := fileEntry.chunks[entry.ChunkIdx]
		if !exists {
			chunk = &chunkEntry{
				slices: make([]Slice, 0),
			}
			fileEntry.chunks[entry.ChunkIdx] = chunk
		}

		// Create cache.Slice from wal.SliceEntry (types unified via aliases)
		slice := Slice{
			ID:        entry.SliceID,
			Offset:    entry.Offset,
			Length:    entry.Length,
			Data:      entry.Data,
			State:     entry.State,
			CreatedAt: entry.CreatedAt,
			BlockRefs: entry.BlockRefs, // Direct assignment - same type via alias
		}

		// Prepend to slices (newest first)
		chunk.slices = append([]Slice{slice}, chunk.slices...)
		c.totalSize.Add(uint64(entry.Length))

		fileEntry.mu.Unlock()
	}

	return nil
}

// getFileEntry returns or creates a file entry with its mutex.
func (c *Cache) getFileEntry(fileHandle string) *fileEntry {
	key := fileHandle

	c.globalMu.RLock()
	entry, exists := c.files[key]
	c.globalMu.RUnlock()

	if exists {
		return entry
	}

	c.globalMu.Lock()
	defer c.globalMu.Unlock()

	// Double-check after acquiring write lock
	if entry, exists = c.files[key]; exists {
		return entry
	}

	entry = &fileEntry{
		chunks:     make(map[uint32]*chunkEntry),
		lastAccess: time.Now(),
	}
	c.files[key] = entry
	return entry
}

// touchFile updates the last access time for LRU tracking.
// Must be called with entry.mu held (read or write lock).
func (c *Cache) touchFile(entry *fileEntry) {
	entry.lastAccess = time.Now()
}
