package payload

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload/chunk"
	"github.com/marmos91/dittofs/pkg/payload/transfer"
)

// PayloadService is the persistence layer for file payload (content) data.
//
// It coordinates between the Cache (fast in-memory/mmap storage) and
// TransferManager (durable block store persistence). Both are required.
//
// Architecture:
//
//	PayloadService
//	     ├── Cache: In-memory buffer with optional mmap backing
//	     └── TransferManager: Background upload to block store (S3, filesystem)
//
// Key responsibilities:
//   - Read/write file content using the Chunk/Slice/Block model
//   - Coordinate cache and block store for durability
//   - Handle chunk boundary calculations transparently
//
// Usage:
//
//	svc := payload.New(cache, transferManager)
//	err := svc.WriteAt(ctx, shareName, payloadID, data, offset)
//	n, err := svc.ReadAt(ctx, shareName, payloadID, buf, offset)
//	err := svc.Flush(ctx, shareName, payloadID)  // NFS COMMIT / SMB CLOSE
type PayloadService struct {
	cache           *cache.Cache
	transferManager *transfer.TransferManager
}

// New creates a new PayloadService with the required cache and transfer manager.
//
// Both parameters are required:
//   - cache: In-memory buffer for reads/writes
//   - transferManager: Handles persistence to block store
func New(c *cache.Cache, tm *transfer.TransferManager) (*PayloadService, error) {
	if c == nil {
		return nil, fmt.Errorf("cache is required")
	}
	if tm == nil {
		return nil, fmt.Errorf("transfer manager is required")
	}

	return &PayloadService{
		cache:           c,
		transferManager: tm,
	}, nil
}

// ============================================================================
// Read Operations
// ============================================================================

// ReadAt reads data at the specified offset.
//
// Data is read from cache first, falling back to block store on cache miss.
// Reads span multiple chunks if the range crosses chunk boundaries,
// with slices merged using newest-wins semantics.
//
// On cache miss, uses EnsureAvailable which downloads required blocks and
// triggers prefetch for sequential read optimization.
func (s *PayloadService) ReadAt(ctx context.Context, _ /* shareName */ string, id metadata.PayloadID, p []byte, offset uint64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	// PayloadID is the sole identifier for file content
	payloadID := string(id)

	totalRead := 0
	for slice := range chunk.Slices(offset, len(p)) {
		// Destination slice within p for this chunk's data
		dest := p[slice.BufOffset : slice.BufOffset+int(slice.Length)]

		// Try to read from cache first
		found, err := s.cache.Read(ctx, payloadID, slice.ChunkIndex, slice.Offset, slice.Length, dest)
		if err != nil && err != cache.ErrFileNotInCache {
			return totalRead, fmt.Errorf("read slice from chunk %d failed: %w", slice.ChunkIndex, err)
		}

		if !found {
			// Cache miss - ensure data is available (downloads + prefetch)
			err := s.transferManager.EnsureAvailable(ctx, payloadID, slice.ChunkIndex, slice.Offset, slice.Length)
			if err != nil {
				return totalRead, fmt.Errorf("ensure available for chunk %d failed: %w", slice.ChunkIndex, err)
			}

			// Now read from cache
			found, err = s.cache.Read(ctx, payloadID, slice.ChunkIndex, slice.Offset, slice.Length, dest)
			if err != nil || !found {
				return totalRead, fmt.Errorf("data not in cache after download for chunk %d", slice.ChunkIndex)
			}
		}

		totalRead += int(slice.Length)
	}

	return totalRead, nil
}

// GetSize returns the size of payload for a file.
//
// Checks cache first, falls back to block store metadata.
func (s *PayloadService) GetSize(ctx context.Context, shareName string, id metadata.PayloadID) (uint64, error) {
	payloadID := string(id)

	// Check cache first
	size := s.cache.GetFileSize(payloadID)
	if size > 0 {
		return size, nil
	}

	// Fall back to block store
	return s.transferManager.GetFileSize(ctx, shareName, payloadID)
}

// Exists checks if payload exists for the file.
//
// Checks cache first, falls back to block store.
func (s *PayloadService) Exists(ctx context.Context, shareName string, id metadata.PayloadID) (bool, error) {
	payloadID := string(id)

	// Check cache first
	if s.cache.GetFileSize(payloadID) > 0 {
		return true, nil
	}

	// Fall back to block store
	return s.transferManager.Exists(ctx, shareName, payloadID)
}

// ============================================================================
// Write Operations
// ============================================================================

// WriteAt writes data at the specified offset.
//
// Writes go to cache using the Chunk/Slice/Block model.
// Data is split across chunk boundaries and stored as slices within each chunk.
//
// Eager upload: After each slice write, complete 4MB blocks are uploaded
// immediately in background goroutines. This reduces data remaining for
// Flush() and improves SMB CLOSE latency.
func (s *PayloadService) WriteAt(ctx context.Context, _ /* shareName */ string, id metadata.PayloadID, data []byte, offset uint64) error {
	if len(data) == 0 {
		return nil
	}

	// PayloadID is the sole identifier for file content
	payloadID := string(id)

	for slice := range chunk.Slices(offset, len(data)) {
		dataEnd := slice.BufOffset + int(slice.Length)

		// Write slice to this chunk
		err := s.cache.Write(ctx, payloadID, slice.ChunkIndex, data[slice.BufOffset:dataEnd], slice.Offset)
		if err != nil {
			return fmt.Errorf("write slice to chunk %d failed: %w", slice.ChunkIndex, err)
		}

		// Trigger eager upload for any complete 4MB blocks (non-blocking)
		s.transferManager.OnWriteComplete(ctx, payloadID, slice.ChunkIndex, slice.Offset, slice.Length)
	}

	return nil
}

// Truncate truncates payload to the specified size.
//
// Updates cache and schedules block store cleanup.
func (s *PayloadService) Truncate(ctx context.Context, shareName string, id metadata.PayloadID, newSize uint64) error {
	payloadID := string(id)

	// Truncate in cache
	if err := s.cache.Truncate(ctx, payloadID, newSize); err != nil {
		return fmt.Errorf("cache truncate failed: %w", err)
	}

	// Schedule block store cleanup
	return s.transferManager.Truncate(ctx, shareName, payloadID, newSize)
}

// Delete removes payload for a file.
//
// Removes from cache and schedules block store cleanup.
func (s *PayloadService) Delete(ctx context.Context, shareName string, id metadata.PayloadID) error {
	payloadID := string(id)

	// Remove from cache
	if err := s.cache.Remove(ctx, payloadID); err != nil {
		return fmt.Errorf("cache remove failed: %w", err)
	}

	// Schedule block store cleanup
	return s.transferManager.Delete(ctx, shareName, payloadID)
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
func (s *PayloadService) Flush(ctx context.Context, _ string, id metadata.PayloadID) (*FlushResult, error) {
	payloadID := string(id)

	// Delegate to TransferManager
	result, err := s.transferManager.Flush(ctx, payloadID)
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
func (s *PayloadService) GetStorageStats(_ context.Context, _ string) (*StorageStats, error) {
	// Count files in cache
	files := s.cache.ListFiles()
	return &StorageStats{
		UsedSize:     0, // TODO: Implement proper stats tracking
		ContentCount: uint64(len(files)),
	}, nil
}

// HealthCheck performs health check on cache and transfer manager.
func (s *PayloadService) HealthCheck(ctx context.Context) error {
	// Check transfer manager (which checks block store)
	return s.transferManager.HealthCheck(ctx)
}
