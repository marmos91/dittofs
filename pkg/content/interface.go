package content

import (
	"context"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ContentServiceInterface defines all public operations provided by ContentService.
//
// This interface documents the complete API available to protocol handlers
// and other consumers of the content layer. All methods handle business logic
// including cache coordination and share routing.
//
// The interface is organized into logical groups:
//   - Cache Management: Register and retrieve slice caches per share
//   - Read Operations: Content reading via Cache
//   - Write Operations: Content writing via Cache
//   - Flush Operations: Write coalescing and future block store flush
//   - Capability Detection: Check for optional features
//   - Statistics and Health: Storage metrics and health checks
type ContentServiceInterface interface {
	// ========================================================================
	// Cache Management
	// ========================================================================

	// RegisterCacheForShare associates a slice cache with a share.
	// Each share must have exactly one Cache.
	RegisterCacheForShare(shareName string, sc *cache.Cache) error

	// GetCacheForShare returns the slice cache for a share.
	// Returns nil if no slice cache is configured for the share.
	GetCacheForShare(shareName string) *cache.Cache

	// HasCache returns true if a slice cache is registered for the share.
	HasCache(shareName string) bool

	// ========================================================================
	// Read Operations
	// ========================================================================

	// ReadAt reads at offset from the slice cache.
	// This is the preferred method for random-access reads (e.g., NFS READ).
	ReadAt(ctx context.Context, shareName string, id metadata.ContentID, p []byte, offset uint64) (int, error)

	// GetContentSize returns content size from slice cache.
	GetContentSize(ctx context.Context, shareName string, id metadata.ContentID) (uint64, error)

	// ContentExists checks if content exists in slice cache.
	ContentExists(ctx context.Context, shareName string, id metadata.ContentID) (bool, error)

	// ========================================================================
	// Write Operations
	// ========================================================================

	// WriteAt writes to slice cache.
	// This is the preferred method for random-access writes (e.g., NFS WRITE).
	WriteAt(ctx context.Context, shareName string, id metadata.ContentID, data []byte, offset uint64) error

	// Truncate truncates content in slice cache.
	Truncate(ctx context.Context, shareName string, id metadata.ContentID, newSize uint64) error

	// Delete removes content from slice cache.
	Delete(ctx context.Context, shareName string, id metadata.ContentID) error

	// ========================================================================
	// Flush Operations
	// ========================================================================

	// Flush flushes cached data (coalesces writes in Phase 1).
	// This is typically called on NFS COMMIT.
	Flush(ctx context.Context, shareName string, id metadata.ContentID) (*FlushResult, error)

	// FlushAndFinalize flushes and finalizes for immediate durability.
	// Phase 1: Same as Flush (coalesces writes).
	// Phase 2: Will flush to block store and finalize.
	FlushAndFinalize(ctx context.Context, shareName string, id metadata.ContentID) (*FlushResult, error)

	// ========================================================================
	// Capability Detection
	// ========================================================================

	// SupportsReadAt returns true if the share supports efficient random reads.
	// Always true when Cache is registered.
	SupportsReadAt(shareName string) bool

	// ========================================================================
	// Statistics and Health
	// ========================================================================

	// GetStorageStats returns storage statistics for a share.
	GetStorageStats(ctx context.Context, shareName string) (*StorageStats, error)

	// Healthcheck performs health check for a share.
	Healthcheck(ctx context.Context, shareName string) error
}

// Compile-time check that ContentService implements ContentServiceInterface.
var _ ContentServiceInterface = (*ContentService)(nil)
