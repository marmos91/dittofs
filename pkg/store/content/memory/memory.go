package memory

import (
	"context"
	"sync"

	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// MemoryContentStore implements ContentStore using in-memory storage.
//
// This implementation stores all content in memory using a map. It's designed for:
//   - Testing and development
//   - Small-scale deployments
//   - Temporary/ephemeral storage
//   - Performance-critical scenarios with small data
//
// Characteristics:
//   - Fast: All operations are memory-speed
//   - Volatile: Data lost on restart
//   - Memory-bound: Limited by available RAM
//   - Thread-safe: Protected by RWMutex
//   - Full-featured: Implements all optional interfaces
//
// Implemented Interfaces:
//   - ContentStore (base read operations)
//   - WritableContentStore (write operations)
//   - SeekableContentStore (seekable reads)
//   - GarbageCollectableStore (cleanup operations)
//
// Thread Safety:
// All operations are protected by a sync.RWMutex. Multiple concurrent readers
// are allowed, but writes are exclusive. Copying data on read/write prevents
// data races with caller-owned buffers.
//
// Memory Management:
// Content is stored as byte slices. Large content may cause memory pressure.
// Consider using filesystem or S3 storage for large files.
type MemoryContentStore struct {
	// data stores the actual file content keyed by ContentID
	data map[metadata.ContentID][]byte

	// mu protects concurrent access to data map
	mu sync.RWMutex
}

// NewMemoryContentStore creates a new in-memory content store.
//
// The store starts empty. All data is stored in memory and will be lost
// when the store is garbage collected or the process exits.
//
// Parameters:
//   - ctx: Context for cancellation (checked before initialization)
//
// Returns:
//   - *MemoryContentStore: Initialized store
//   - error: Only returns error if context is cancelled
func NewMemoryContentStore(ctx context.Context) (*MemoryContentStore, error) {
	// ========================================================================
	// Step 1: Check context before initialization
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ========================================================================
	// Step 2: Initialize the store
	// ========================================================================

	return &MemoryContentStore{
		data: make(map[metadata.ContentID][]byte),
	}, nil
}

// GetStorageStats returns statistics about the in-memory storage.
//
// Statistics are calculated on-the-fly from the current state:
//   - TotalSize: Unlimited (^uint64(0))
//   - UsedSize: Sum of all content sizes
//   - AvailableSize: Unlimited (^uint64(0))
//   - ContentCount: Number of content items
//   - AverageSize: UsedSize / ContentCount (0 if empty)
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//
// Returns:
//   - *StorageStats: Current storage statistics
//   - error: Only returns error for context cancellation
func (s *MemoryContentStore) GetStorageStats(ctx context.Context) (*content.StorageStats, error) {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ========================================================================
	// Step 2: Acquire read lock and calculate statistics
	// ========================================================================

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Calculate current statistics
	usedSize := uint64(0)
	for _, data := range s.data {
		usedSize += uint64(len(data))
	}

	contentCount := uint64(len(s.data))

	averageSize := uint64(0)
	if contentCount > 0 {
		averageSize = usedSize / contentCount
	}

	return &content.StorageStats{
		TotalSize:     ^uint64(0), // Unlimited
		UsedSize:      usedSize,
		AvailableSize: ^uint64(0), // Unlimited
		ContentCount:  contentCount,
		AverageSize:   averageSize,
	}, nil
}
