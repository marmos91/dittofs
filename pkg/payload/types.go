package payload

import "github.com/marmos91/dittofs/pkg/payload/offloader"

// ============================================================================
// Type Notes
// ============================================================================
//
// Block-related types are organized as follows:
//
// Type locations:
// - cache.BlockState    - block lifecycle states (Pending → Uploading → Uploaded)
// - cache.PendingBlock  - block ready for upload (includes ChunkIndex, BlockIndex, Data)
// - cache.Stats         - cache statistics (TotalSize, DirtyBytes, UploadedBytes)
// - transfer.FlushResult - result of flush operation
//
// The cache package owns the canonical type definitions for in-memory state,
// while the transfer package handles block store persistence.

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

// FlushResult is an alias to offloader.FlushResult for API compatibility.
// The canonical definition is in pkg/payload/offloader/types.go.
type FlushResult = offloader.FlushResult
