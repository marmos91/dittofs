package payload

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
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
//	     ├── Cache: In-memory buffer with mmap backing
//	     └── TransferManager: Background upload to block store (S3, filesystem)
//
// Key responsibilities:
//   - Read/write file content using the Chunk/Block model
//   - Coordinate cache and block store for durability
//   - Handle chunk boundary calculations transparently
//
// Usage:
//
//	svc := payload.New(cache, transferManager)
//	err := svc.WriteAt(ctx, payloadID, data, offset)
//	n, err := svc.ReadAt(ctx, payloadID, buf, offset)
//	err := svc.Flush(ctx, payloadID)  // NFS COMMIT / SMB CLOSE
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
// Reads span multiple blocks/chunks if the range crosses boundaries.
//
// On cache miss, uses EnsureAvailable which downloads required blocks and
// triggers prefetch for sequential read optimization.
func (s *PayloadService) ReadAt(ctx context.Context, id metadata.PayloadID, data []byte, offset uint64) (int, error) {
	return s.readAtInternal(ctx, id, "", data, offset)
}

// ReadAtWithCOWSource reads data at the specified offset, using a COW source for lazy copy.
//
// This method is used when reading from a file that has been copy-on-write split.
// If data is not found in the primary payloadID's cache or block store, it will
// be copied from the cowSource payloadID.
//
// Parameters:
//   - ctx: Context for cancellation
//   - id: Primary PayloadID to read from
//   - cowSource: Source PayloadID for lazy copy (can be empty to skip COW)
//   - data: Buffer to read into
//   - offset: Byte offset to read from
//
// Returns:
//   - int: Number of bytes read
//   - error: Error if read failed
func (s *PayloadService) ReadAtWithCOWSource(ctx context.Context, id metadata.PayloadID, cowSource metadata.PayloadID, data []byte, offset uint64) (int, error) {
	return s.readAtInternal(ctx, id, cowSource, data, offset)
}

// readAtInternal is the shared implementation for ReadAt and ReadAtWithCOWSource.
//
// When cowSource is empty, reads from the primary payloadID only.
// When cowSource is provided, falls back to COW source on cache miss and copies
// the data to the primary cache for future reads.
func (s *PayloadService) readAtInternal(ctx context.Context, id metadata.PayloadID, cowSource metadata.PayloadID, data []byte, offset uint64) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	payloadID := string(id)
	sourcePayloadID := string(cowSource)
	hasCOWSource := cowSource != ""

	totalRead := 0
	for blockRange := range chunk.BlockRanges(offset, len(data)) {
		// Destination slice within data for this block range
		dest := data[blockRange.BufOffset : blockRange.BufOffset+int(blockRange.Length)]

		// Calculate chunk-level offset from block coordinates
		chunkOffset := chunk.ChunkOffsetForBlock(blockRange.BlockIndex) + blockRange.Offset

		// Try to read from primary cache first
		found, err := s.cache.ReadAt(ctx, payloadID, blockRange.ChunkIndex, chunkOffset, blockRange.Length, dest)
		if err != nil && err != cache.ErrFileNotInCache {
			return totalRead, fmt.Errorf("read block %d/%d failed: %w", blockRange.ChunkIndex, blockRange.BlockIndex, err)
		}

		if !found {
			if hasCOWSource {
				// Try COW source first
				if err := s.readFromCOWSource(ctx, payloadID, sourcePayloadID, blockRange, chunkOffset, dest); err != nil {
					return totalRead, err
				}
			} else {
				// No COW source - fetch from block store
				if err := s.ensureAndReadFromCache(ctx, payloadID, blockRange, chunkOffset, dest); err != nil {
					return totalRead, err
				}
			}
		}

		totalRead += int(blockRange.Length)
	}

	return totalRead, nil
}

// readFromCOWSource attempts to read from COW source and copies to primary cache.
func (s *PayloadService) readFromCOWSource(ctx context.Context, payloadID, sourcePayloadID string, blockRange chunk.BlockRange, chunkOffset uint32, dest []byte) error {
	// Try COW source cache
	sourceFound, sourceErr := s.cache.ReadAt(ctx, sourcePayloadID, blockRange.ChunkIndex, chunkOffset, blockRange.Length, dest)
	if sourceErr != nil && sourceErr != cache.ErrFileNotInCache {
		return fmt.Errorf("COW source read block %d/%d failed: %w", blockRange.ChunkIndex, blockRange.BlockIndex, sourceErr)
	}

	if !sourceFound {
		// Not in COW source cache - fetch from block store
		err := s.transferManager.EnsureAvailable(ctx, sourcePayloadID, blockRange.ChunkIndex, chunkOffset, blockRange.Length)
		if err != nil {
			return fmt.Errorf("ensure available for COW source block %d/%d failed: %w", blockRange.ChunkIndex, blockRange.BlockIndex, err)
		}

		// Read from source cache (now populated from block store)
		sourceFound, sourceErr = s.cache.ReadAt(ctx, sourcePayloadID, blockRange.ChunkIndex, chunkOffset, blockRange.Length, dest)
		if sourceErr != nil || !sourceFound {
			return fmt.Errorf("COW source data not in cache after download for block %d/%d", blockRange.ChunkIndex, blockRange.BlockIndex)
		}
	}

	// Copy to primary cache for future reads (non-fatal if fails)
	if err := s.cache.WriteAt(ctx, payloadID, blockRange.ChunkIndex, dest, chunkOffset); err != nil {
		logger.Debug("COW cache write failed (non-fatal)", "payloadID", payloadID, "error", err)
	}

	return nil
}

// ensureAndReadFromCache ensures data is available from block store and reads it.
func (s *PayloadService) ensureAndReadFromCache(ctx context.Context, payloadID string, blockRange chunk.BlockRange, chunkOffset uint32, dest []byte) error {
	// Cache miss - ensure data is available (downloads + prefetch)
	err := s.transferManager.EnsureAvailable(ctx, payloadID, blockRange.ChunkIndex, chunkOffset, blockRange.Length)
	if err != nil {
		return fmt.Errorf("ensure available for block %d/%d failed: %w", blockRange.ChunkIndex, blockRange.BlockIndex, err)
	}

	// Now read from cache
	found, err := s.cache.ReadAt(ctx, payloadID, blockRange.ChunkIndex, chunkOffset, blockRange.Length, dest)
	if err != nil || !found {
		return fmt.Errorf("data not in cache after download for block %d/%d", blockRange.ChunkIndex, blockRange.BlockIndex)
	}

	return nil
}

// GetSize returns the size of payload for a file.
//
// Checks cache first, falls back to block store metadata.
func (s *PayloadService) GetSize(ctx context.Context, id metadata.PayloadID) (uint64, error) {
	payloadID := string(id)

	// Check cache first
	size := s.cache.GetFileSize(ctx, payloadID)
	if size > 0 {
		return size, nil
	}

	// Fall back to block store
	return s.transferManager.GetFileSize(ctx, payloadID)
}

// Exists checks if payload exists for the file.
//
// Checks cache first, falls back to block store.
func (s *PayloadService) Exists(ctx context.Context, id metadata.PayloadID) (bool, error) {
	payloadID := string(id)

	// Check cache first
	if s.cache.GetFileSize(ctx, payloadID) > 0 {
		return true, nil
	}

	// Fall back to block store
	return s.transferManager.Exists(ctx, payloadID)
}

// ============================================================================
// Write Operations
// ============================================================================

// WriteAt writes data at the specified offset.
//
// Writes go to cache at block-level granularity (4MB blocks).
// Data is split across block boundaries for hash computation and deduplication.
//
// Eager upload: After each block write, complete 4MB blocks are uploaded
// immediately in background goroutines. This reduces data remaining for
// Flush() and improves SMB CLOSE latency.
func (s *PayloadService) WriteAt(ctx context.Context, id metadata.PayloadID, data []byte, offset uint64) error {
	if len(data) == 0 {
		return nil
	}

	// PayloadID is the sole identifier for file content
	payloadID := string(id)

	for blockRange := range chunk.BlockRanges(offset, len(data)) {
		dataEnd := blockRange.BufOffset + int(blockRange.Length)

		// Calculate chunk-level offset from block coordinates
		chunkOffset := chunk.ChunkOffsetForBlock(blockRange.BlockIndex) + blockRange.Offset

		// Write block range to cache
		err := s.cache.WriteAt(ctx, payloadID, blockRange.ChunkIndex, data[blockRange.BufOffset:dataEnd], chunkOffset)
		if err != nil {
			return fmt.Errorf("write block %d/%d failed: %w", blockRange.ChunkIndex, blockRange.BlockIndex, err)
		}

		// Trigger eager upload for any complete 4MB blocks (non-blocking)
		s.transferManager.OnWriteComplete(ctx, payloadID, blockRange.ChunkIndex, chunkOffset, blockRange.Length)
	}

	return nil
}

// Truncate truncates payload to the specified size.
//
// Updates cache and schedules block store cleanup.
func (s *PayloadService) Truncate(ctx context.Context, id metadata.PayloadID, newSize uint64) error {
	payloadID := string(id)

	// Truncate in cache
	if err := s.cache.Truncate(ctx, payloadID, newSize); err != nil {
		return fmt.Errorf("cache truncate failed: %w", err)
	}

	// Schedule block store cleanup
	return s.transferManager.Truncate(ctx, payloadID, newSize)
}

// Delete removes payload for a file.
//
// Removes from cache and schedules block store cleanup.
func (s *PayloadService) Delete(ctx context.Context, id metadata.PayloadID) error {
	payloadID := string(id)

	// Remove from cache
	if err := s.cache.Remove(ctx, payloadID); err != nil {
		return fmt.Errorf("cache remove failed: %w", err)
	}

	// Schedule block store cleanup
	return s.transferManager.Delete(ctx, payloadID)
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
func (s *PayloadService) GetStorageStats(_ context.Context) (*StorageStats, error) {
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
