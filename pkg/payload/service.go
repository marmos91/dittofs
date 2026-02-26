package payload

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload/offloader"

	payloadio "github.com/marmos91/dittofs/pkg/payload/io"
)

func init() {
	// Wire cache sentinel errors into the io sub-package so it can detect
	// cache-miss and cache-full conditions without importing the cache package.
	payloadio.CacheFileNotFoundError = cache.ErrFileNotInCache
	payloadio.CacheFullError = cache.ErrCacheFull
}

// PayloadService is the persistence layer for file payload (content) data.
//
// It composes the I/O sub-service (read/write operations coordinated between
// cache and offloader) with orchestration operations (flush, stats, health).
//
// Architecture:
//
//	PayloadService
//	     ├── io.ServiceImpl: Read/write operations (cache + offloader coordination)
//	     ├── Cache: In-memory buffer with mmap backing
//	     └── Offloader: Background upload to block store (S3, filesystem)
//
// Key responsibilities:
//   - Read/write file content using the Chunk/Block model (via io.ServiceImpl)
//   - Coordinate cache and block store for durability
//   - Handle flush, stats, and health check operations
//
// Usage:
//
//	svc := payload.New(cache, offloader)
//	err := svc.WriteAt(ctx, payloadID, data, offset)
//	n, err := svc.ReadAt(ctx, payloadID, buf, offset)
//	err := svc.Flush(ctx, payloadID)  // NFS COMMIT / SMB CLOSE
type PayloadService struct {
	*payloadio.ServiceImpl // Embedded I/O sub-service for read/write operations

	cache     *cache.Cache
	offloader *offloader.Offloader
}

// New creates a new PayloadService with the required cache and offloader.
//
// Both parameters are required:
//   - cache: In-memory buffer for reads/writes
//   - offloader: Handles persistence to block store
func New(c *cache.Cache, tm *offloader.Offloader) (*PayloadService, error) {
	if c == nil {
		return nil, fmt.Errorf("cache is required")
	}
	if tm == nil {
		return nil, fmt.Errorf("offloader is required")
	}

	// Create the I/O sub-service with cache and offloader satisfying the local interfaces.
	// *cache.Cache satisfies CacheReader, CacheWriter, and CacheStateManager.
	// *offloader.Offloader satisfies BlockDownloader and BlockUploader.
	ioSvc := payloadio.New(c, c, c, tm, tm)

	return &PayloadService{
		ServiceImpl: ioSvc,
		cache:       c,
		offloader:   tm,
	}, nil
}

// ============================================================================
// Flush Operations
// ============================================================================

// Flush enqueues remaining dirty data for background upload and returns immediately.
//
// Used by both NFS COMMIT and SMB CLOSE:
//   - Enqueues remaining data for background block store upload
//   - Returns immediately (non-blocking)
//   - Data is safe in mmap cache (crash-safe via OS page cache)
//
// Returns FlushResult indicating the operation status.
func (s *PayloadService) Flush(ctx context.Context, id metadata.PayloadID) (*FlushResult, error) {
	payloadID := string(id)

	// Delegate to Offloader
	result, err := s.offloader.Flush(ctx, payloadID)
	if err != nil {
		return nil, fmt.Errorf("flush failed: %w", err)
	}

	return result, nil
}

// ============================================================================
// Statistics and Health
// ============================================================================

// GetStorageStats returns storage statistics.
//
// Note: This is inefficient as it lists all files. Consider caching this
// information in the metadata store for production use.
func (s *PayloadService) GetStorageStats(_ context.Context) (*StorageStats, error) {
	// Count files in cache
	files := s.cache.ListFiles()
	return &StorageStats{
		UsedSize:     0, // TODO: Implement proper stats tracking
		ContentCount: uint64(len(files)),
	}, nil
}

// HealthCheck performs health check on cache and offloader.
func (s *PayloadService) HealthCheck(ctx context.Context) error {
	// Check offloader (which checks block store)
	return s.offloader.HealthCheck(ctx)
}
