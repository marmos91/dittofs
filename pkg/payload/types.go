package payload

// ============================================================================
// Type Notes
// ============================================================================
//
// BlockRef, Slice, SliceState, and Chunk-related types are defined in pkg/cache
// because:
// 1. The cache package owns these concepts (it's the in-memory representation)
// 2. Circular dependency: blocks imports cache, so cache cannot import blocks
//
// The WAL package (pkg/wal) defines its own SliceEntry and BlockRef for
// persistence format independence - this is standard practice for persistence
// layers that may need to version or migrate data formats.
//
// Type locations:
// - cache.BlockRef    - canonical block reference type
// - cache.Slice       - in-memory slice representation
// - cache.SliceState  - slice lifecycle states
// - wal.SliceEntry    - WAL persistence format
// - wal.BlockRef      - WAL's copy (for format independence)

// ============================================================================
// Supporting Types
// ============================================================================

// StorageStats contains statistics about block storage.
//
// This provides information about storage capacity, usage, and health.
type StorageStats struct {
	// TotalSize is the total storage capacity in bytes.
	// For cache-only mode, this may be the configured cache limit.
	TotalSize uint64

	// UsedSize is the actual space consumed by content in bytes.
	UsedSize uint64

	// AvailableSize is the remaining available space in bytes.
	AvailableSize uint64

	// ContentCount is the total number of content items (slices).
	ContentCount uint64

	// AverageSize is the average size of content items in bytes.
	AverageSize uint64
}

// FlushResult contains information about a flush operation.
type FlushResult struct {
	// BytesFlushed is the number of bytes written.
	BytesFlushed uint64

	// AlreadyFlushed indicates all data was already flushed (no-op).
	AlreadyFlushed bool

	// Finalized indicates whether the content was finalized (complete and durable).
	Finalized bool
}
