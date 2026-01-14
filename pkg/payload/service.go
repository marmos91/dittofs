package payload

import (
	"context"
	"fmt"
	"sync"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload/chunk"
	"github.com/marmos91/dittofs/pkg/transfer"
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
//	err := svc.FlushAsync(ctx, shareName, payloadID)  // NFS COMMIT
//	err := svc.Flush(ctx, shareName, payloadID)       // SMB CLOSE
type PayloadService struct {
	mu              sync.RWMutex
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
func (s *PayloadService) ReadAt(ctx context.Context, shareName string, id metadata.PayloadID, p []byte, offset uint64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	s.mu.RLock()
	c := s.cache
	tm := s.transferManager
	s.mu.RUnlock()

	// PayloadID is used directly as cache key
	fileHandle := []byte(id)

	totalRead := 0
	for slice := range chunk.Slices(offset, uint64(len(p))) {
		// Try to read from cache first
		data, found, err := c.ReadSlice(ctx, fileHandle, slice.ChunkIndex, slice.Offset, slice.Length)
		if err != nil && err != cache.ErrFileNotInCache {
			return totalRead, fmt.Errorf("read slice from chunk %d failed: %w", slice.ChunkIndex, err)
		}

		if found {
			copy(p[slice.BufOffset:], data)
		} else {
			// Cache miss - fetch from block store
			blockData, err := tm.ReadBlocks(ctx, shareName, fileHandle, string(id), slice.ChunkIndex, slice.Offset, slice.Length)
			if err != nil {
				return totalRead, fmt.Errorf("read blocks from chunk %d failed: %w", slice.ChunkIndex, err)
			}
			copy(p[slice.BufOffset:], blockData)
		}

		totalRead += int(slice.Length)
	}

	return totalRead, nil
}

// GetSize returns the size of payload for a file.
//
// Checks cache first, falls back to block store metadata.
func (s *PayloadService) GetSize(ctx context.Context, shareName string, id metadata.PayloadID) (uint64, error) {
	s.mu.RLock()
	c := s.cache
	tm := s.transferManager
	s.mu.RUnlock()

	fileHandle := []byte(id)

	// Check cache first
	size := c.GetFileSize(fileHandle)
	if size > 0 {
		return size, nil
	}

	// Fall back to block store
	return tm.GetFileSize(ctx, shareName, string(id))
}

// Exists checks if payload exists for the file.
//
// Checks cache first, falls back to block store.
func (s *PayloadService) Exists(ctx context.Context, shareName string, id metadata.PayloadID) (bool, error) {
	s.mu.RLock()
	c := s.cache
	tm := s.transferManager
	s.mu.RUnlock()

	fileHandle := []byte(id)

	// Check cache first
	if c.GetFileSize(fileHandle) > 0 {
		return true, nil
	}

	// Fall back to block store
	return tm.Exists(ctx, shareName, string(id))
}

// ============================================================================
// Write Operations
// ============================================================================

// WriteAt writes data at the specified offset.
//
// Writes go to cache using the Chunk/Slice/Block model.
// Data is split across chunk boundaries and stored as slices within each chunk.
// Block store uploads are triggered on Flush() via background worker pool.
func (s *PayloadService) WriteAt(ctx context.Context, shareName string, id metadata.PayloadID, data []byte, offset uint64) error {
	if len(data) == 0 {
		return nil
	}

	s.mu.RLock()
	c := s.cache
	s.mu.RUnlock()

	fileHandle := []byte(id)

	for slice := range chunk.Slices(offset, uint64(len(data))) {
		dataEnd := slice.BufOffset + int(slice.Length)

		// Write slice to this chunk
		err := c.WriteSlice(ctx, fileHandle, slice.ChunkIndex, data[slice.BufOffset:dataEnd], slice.Offset)
		if err != nil {
			return fmt.Errorf("write slice to chunk %d failed: %w", slice.ChunkIndex, err)
		}
	}

	return nil
}

// Truncate truncates payload to the specified size.
//
// Updates cache and schedules block store cleanup.
func (s *PayloadService) Truncate(ctx context.Context, shareName string, id metadata.PayloadID, newSize uint64) error {
	s.mu.RLock()
	c := s.cache
	tm := s.transferManager
	s.mu.RUnlock()

	fileHandle := []byte(id)

	// Truncate in cache
	if err := c.Truncate(ctx, fileHandle, newSize); err != nil {
		return fmt.Errorf("cache truncate failed: %w", err)
	}

	// Schedule block store cleanup
	return tm.Truncate(ctx, shareName, string(id), newSize)
}

// Delete removes payload for a file.
//
// Removes from cache and schedules block store cleanup.
func (s *PayloadService) Delete(ctx context.Context, shareName string, id metadata.PayloadID) error {
	s.mu.RLock()
	c := s.cache
	tm := s.transferManager
	s.mu.RUnlock()

	fileHandle := []byte(id)

	// Remove from cache
	if err := c.Remove(ctx, fileHandle); err != nil {
		return fmt.Errorf("cache remove failed: %w", err)
	}

	// Schedule block store cleanup
	return tm.Delete(ctx, shareName, string(id))
}

// ============================================================================
// Flush Operations
// ============================================================================

// FlushAsync flushes cached data for a file (non-blocking).
//
// This is called by NFS COMMIT:
//   - Enqueues remaining data for background block store upload
//   - Returns immediately (non-blocking)
//   - Data is safe in mmap cache (crash-safe via OS page cache)
//
// Returns FlushResult indicating the operation status.
func (s *PayloadService) FlushAsync(ctx context.Context, shareName string, id metadata.PayloadID) (*FlushResult, error) {
	s.mu.RLock()
	tm := s.transferManager
	s.mu.RUnlock()

	fileHandle := []byte(id)

	// Non-blocking enqueue - uploads happen in background worker pool
	err := tm.FlushRemainingAsync(ctx, shareName, fileHandle, string(id))
	if err != nil {
		return nil, fmt.Errorf("flush async failed: %w", err)
	}

	return &FlushResult{
		AlreadyFlushed: false,
		Finalized:      true,
	}, nil
}

// Flush flushes and finalizes for immediate durability (blocking).
//
// This is called by SMB CLOSE which requires full durability before returning:
//   - Waits for in-flight uploads to complete
//   - Uploads remaining partial blocks
//   - Blocks until all data is persisted to block store
//
// Returns FlushResult indicating the operation status.
func (s *PayloadService) Flush(ctx context.Context, shareName string, id metadata.PayloadID) (*FlushResult, error) {
	s.mu.RLock()
	tm := s.transferManager
	s.mu.RUnlock()

	fileHandle := []byte(id)
	payloadID := string(id)

	// Wait for any in-flight eager uploads
	if err := tm.WaitForUploads(ctx, payloadID); err != nil {
		return nil, fmt.Errorf("wait for uploads: %w", err)
	}

	// Flush remaining blocks (blocking)
	if err := tm.FlushRemaining(ctx, shareName, fileHandle, payloadID); err != nil {
		return nil, fmt.Errorf("flush remaining: %w", err)
	}

	return &FlushResult{
		AlreadyFlushed: false,
		Finalized:      true,
	}, nil
}

// ============================================================================
// Statistics and Health
// ============================================================================

// GetStorageStats returns storage statistics.
//
// Note: This is inefficient as it lists all files. Consider caching this
// information in the metadata store for production use.
func (s *PayloadService) GetStorageStats(ctx context.Context, shareName string) (*StorageStats, error) {
	s.mu.RLock()
	c := s.cache
	s.mu.RUnlock()

	// Count files in cache
	files := c.ListFiles()
	return &StorageStats{
		UsedSize:     0, // TODO: Implement proper stats tracking
		ContentCount: uint64(len(files)),
	}, nil
}

// HealthCheck performs health check on cache and transfer manager.
func (s *PayloadService) HealthCheck(ctx context.Context) error {
	s.mu.RLock()
	tm := s.transferManager
	s.mu.RUnlock()

	// Check transfer manager (which checks block store)
	return tm.HealthCheck(ctx)
}
