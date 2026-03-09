package blockstore

import (
	"context"
	"time"
)

// ============================================================================
// FileBlockStore Interface (Content-Addressed Block Management)
// ============================================================================

// FileBlockStore defines operations for content-addressed file block management.
//
// FileBlock is the single block entity in DittoFS. Each block is content-addressed
// by its SHA-256 hash and reference-counted for dedup and GC.
type FileBlockStore interface {
	// GetFileBlock retrieves a file block by its ID.
	// Returns ErrFileBlockNotFound if not found.
	GetFileBlock(ctx context.Context, id string) (*FileBlock, error)

	// PutFileBlock stores or updates a file block.
	PutFileBlock(ctx context.Context, block *FileBlock) error

	// DeleteFileBlock removes a file block by its ID.
	// Returns ErrFileBlockNotFound if not found.
	DeleteFileBlock(ctx context.Context, id string) error

	// IncrementRefCount atomically increments a block's RefCount.
	IncrementRefCount(ctx context.Context, id string) error

	// DecrementRefCount atomically decrements a block's RefCount.
	// Returns the new count. When 0, the block is a GC candidate.
	DecrementRefCount(ctx context.Context, id string) (uint32, error)

	// FindFileBlockByHash looks up a finalized block by its content hash.
	// Returns nil without error if not found (used for dedup checks).
	FindFileBlockByHash(ctx context.Context, hash ContentHash) (*FileBlock, error)

	// ListLocalBlocks returns blocks that are in Local state (complete, on disk,
	// not yet synced to remote) and older than the given duration.
	// If limit > 0, at most limit blocks are returned. If limit <= 0, all are returned.
	ListLocalBlocks(ctx context.Context, olderThan time.Duration, limit int) ([]*FileBlock, error)

	// ListRemoteBlocks returns blocks that are both cached locally and confirmed
	// in remote store, ordered by LRU (oldest LastAccess first), up to limit.
	ListRemoteBlocks(ctx context.Context, limit int) ([]*FileBlock, error)

	// ListUnreferenced returns blocks with RefCount=0, up to limit.
	// These are candidates for garbage collection.
	ListUnreferenced(ctx context.Context, limit int) ([]*FileBlock, error)

	// ListFileBlocks returns all blocks belonging to a file, ordered by block index.
	// Block IDs follow the format "{payloadID}/{blockIdx}", so this method returns
	// all blocks whose ID starts with "{payloadID}/".
	// Returns empty slice (not nil) if no blocks found.
	ListFileBlocks(ctx context.Context, payloadID string) ([]*FileBlock, error)
}

// ============================================================================
// Store Interface (Orchestrator)
// ============================================================================

// Reader defines read operations on the block store.
type Reader interface {
	// ReadAt reads data from storage at the given offset into dest.
	ReadAt(ctx context.Context, payloadID string, data []byte, offset uint64) (int, error)

	// ReadAtWithCOWSource reads data with copy-on-write source fallback.
	ReadAtWithCOWSource(ctx context.Context, payloadID, cowSource string, data []byte, offset uint64) (int, error)

	// GetSize returns the stored size of a payload.
	GetSize(ctx context.Context, payloadID string) (uint64, error)

	// Exists checks whether a payload exists.
	Exists(ctx context.Context, payloadID string) (bool, error)
}

// Writer defines write operations on the block store.
type Writer interface {
	// WriteAt writes data to storage at the given offset.
	WriteAt(ctx context.Context, payloadID string, data []byte, offset uint64) error

	// Truncate changes the size of a payload.
	Truncate(ctx context.Context, payloadID string, newSize uint64) error

	// Delete removes all data for a payload.
	Delete(ctx context.Context, payloadID string) error
}

// Flusher defines flush/sync operations on the block store.
type Flusher interface {
	// Flush ensures all dirty data for a payload is persisted.
	Flush(ctx context.Context, payloadID string) (*FlushResult, error)

	// DrainAllUploads waits for all pending uploads to complete.
	DrainAllUploads(ctx context.Context) error
}

// Store is the composed block store interface that combines all sub-interfaces
// with lifecycle and health operations.
type Store interface {
	Reader
	Writer
	Flusher

	// Stats returns storage statistics.
	Stats() (*Stats, error)

	// HealthCheck verifies the store is operational.
	HealthCheck(ctx context.Context) error

	// Start initializes the store and starts background goroutines.
	Start(ctx context.Context) error

	// Close releases resources held by the store.
	Close() error
}

// ============================================================================
// Store Types
// ============================================================================

// FlushResult indicates the outcome of a flush operation.
type FlushResult struct {
	// Finalized indicates all blocks have been synced to the backend store.
	Finalized bool
}

// Stats contains storage statistics.
type Stats struct {
	TotalSize     uint64 // Total storage capacity in bytes
	UsedSize      uint64 // Space consumed by content in bytes
	AvailableSize uint64 // Remaining available space in bytes
	ContentCount  uint64 // Total number of content items
	AverageSize   uint64 // Average size of content items in bytes
}
