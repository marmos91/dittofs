package content

import (
	"context"
	"io"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ContentServiceInterface defines all public operations provided by ContentService.
//
// This interface documents the complete API available to protocol handlers
// and other consumers of the content layer. All methods handle business logic
// including cache coordination, error mapping, and store routing.
//
// The interface is organized into logical groups:
//   - Store Management: Register and retrieve content stores per share
//   - Cache Management: Register and retrieve caches per share
//   - Read Operations: Cache-aware content reading
//   - Write Operations: Cache-aware content writing
//   - Flush Operations: Cache to content store synchronization
//   - Capability Detection: Check for optional store features
//   - Statistics and Health: Storage metrics and health checks
type ContentServiceInterface interface {
	// ========================================================================
	// Store Management
	// ========================================================================

	// RegisterStoreForShare associates a content store with a share.
	// Each share must have exactly one store. Calling this again for the same
	// share will replace the previous store.
	RegisterStoreForShare(shareName string, store ContentStore) error

	// GetStoreForShare returns the content store for a specific share.
	// This is primarily for internal use and testing; protocol handlers
	// should use the high-level methods instead.
	GetStoreForShare(shareName string) (ContentStore, error)

	// ========================================================================
	// Cache Management
	// ========================================================================

	// RegisterCacheForShare associates a cache with a share.
	// Caches are optional - if not registered, operations go directly to the store.
	RegisterCacheForShare(shareName string, c cache.Cache) error

	// GetCacheForShare returns the cache for a share.
	// Returns nil if no cache is configured for the share.
	GetCacheForShare(shareName string) cache.Cache

	// ========================================================================
	// Read Operations (cache-aware)
	// ========================================================================

	// ReadContent reads from cache or content store.
	// Priority: write cache -> read cache -> content store
	//
	// If cache contains the content (and is valid), returns cached data.
	// Otherwise, reads from content store.
	ReadContent(ctx context.Context, shareName string, id metadata.ContentID) (io.ReadCloser, error)

	// ReadAt reads at offset, using cache or ReadAtContentStore if available.
	// This is the preferred method for random-access reads (e.g., NFS READ).
	//
	// Priority: cache -> ReadAtContentStore -> fallback to sequential read
	ReadAt(ctx context.Context, shareName string, id metadata.ContentID, p []byte, offset uint64) (int, error)

	// GetContentSize returns content size (from cache or store).
	GetContentSize(ctx context.Context, shareName string, id metadata.ContentID) (uint64, error)

	// ContentExists checks if content exists in cache or store.
	ContentExists(ctx context.Context, shareName string, id metadata.ContentID) (bool, error)

	// ========================================================================
	// Write Operations (cache-aware)
	// ========================================================================

	// WriteAt writes to cache (if configured) or directly to content store.
	// This is the preferred method for random-access writes (e.g., NFS WRITE).
	WriteAt(ctx context.Context, shareName string, id metadata.ContentID, data []byte, offset uint64) error

	// WriteContent writes complete content to cache or store.
	WriteContent(ctx context.Context, shareName string, id metadata.ContentID, data []byte) error

	// Truncate truncates content in cache or store.
	Truncate(ctx context.Context, shareName string, id metadata.ContentID, newSize uint64) error

	// Delete removes content from store (and cache if present).
	Delete(ctx context.Context, shareName string, id metadata.ContentID) error

	// ========================================================================
	// Flush Operations (Cache -> Store)
	// ========================================================================

	// Flush flushes cached data to content store.
	// This is typically called on NFS COMMIT to persist cached writes.
	// The content may not be fully finalized (for S3, multipart may still be in progress).
	Flush(ctx context.Context, shareName string, id metadata.ContentID) (*FlushResult, error)

	// FlushAndFinalize flushes and finalizes for immediate durability.
	// This ensures all data is persisted and the upload is complete.
	// For S3, this includes completing the multipart upload.
	FlushAndFinalize(ctx context.Context, shareName string, id metadata.ContentID) (*FlushResult, error)

	// ========================================================================
	// Capability Detection
	// ========================================================================

	// SupportsReadAt returns true if the store supports efficient random reads.
	// When true, ReadAt uses the store's native range-read capability.
	SupportsReadAt(shareName string) bool

	// SupportsIncrementalWrite returns true if the store supports incremental writes.
	// When true, Flush uses incremental upload (S3 multipart) instead of full write.
	SupportsIncrementalWrite(shareName string) bool

	// ========================================================================
	// Statistics and Health
	// ========================================================================

	// GetStorageStats returns storage statistics for a share.
	GetStorageStats(ctx context.Context, shareName string) (*StorageStats, error)

	// Healthcheck performs health check for a share's content store.
	Healthcheck(ctx context.Context, shareName string) error
}

// Compile-time check that ContentService implements ContentServiceInterface.
var _ ContentServiceInterface = (*ContentService)(nil)
