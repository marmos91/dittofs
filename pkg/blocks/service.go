package blocks

import (
	"context"
	"fmt"
	"sync"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/transfer"
)

// BlockService provides all block operations for the filesystem.
//
// It manages a single global Cache that serves all shares.
// ContentID uniqueness guarantees data isolation between files.
// All protocol handlers should interact with BlockService rather than
// accessing the cache directly.
//
// Cache Model:
//   - All writes go to Cache using WriteSlice API
//   - Reads merge slices using newest-wins semantics
//   - TransferManager handles cache-to-block-store persistence (optional)
//
// Usage:
//
//	blockSvc := blocks.New()
//	blockSvc.SetCache(sliceCache)
//	blockSvc.SetTransferManager(transferMgr) // Optional: enables block store persistence
//	err := blockSvc.WriteAt(ctx, shareName, contentID, data, offset)
type BlockService struct {
	mu              sync.RWMutex
	cache           *cache.Cache            // Single global cache for all shares
	transferManager *transfer.TransferManager // Optional: handles cache-to-block-store persistence
}

// New creates a new empty BlockService instance.
// Use SetCache to configure the global cache.
func New() *BlockService {
	return &BlockService{}
}

// SetCache sets the global Cache for the service.
//
// Cache is the Chunk/Slice/Block model that provides:
//   - Slice-aware caching with newest-wins merge semantics
//   - Efficient random writes (no read-modify-write on block store)
//   - Write coalescing for better performance
//
// This should be called once during initialization.
func (s *BlockService) SetCache(sc *cache.Cache) error {
	if sc == nil {
		return fmt.Errorf("cannot set nil cache")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.cache = sc
	return nil
}

// GetCache returns the global cache.
// Returns nil if no cache is configured.
func (s *BlockService) GetCache() *cache.Cache {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.cache
}

// SetTransferManager sets the transfer manager for cache-to-block-store persistence.
//
// When a transfer manager is configured:
//   - Writes trigger eager block uploads (4MB blocks uploaded as they become ready)
//   - Flush() waits for in-flight uploads and flushes remaining data to block store
//   - ReadAt() falls back to block store on cache miss
//
// This is optional - without a transfer manager, the service operates in cache-only mode.
func (s *BlockService) SetTransferManager(tm *transfer.TransferManager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transferManager = tm
}

// GetTransferManager returns the transfer manager, or nil if not configured.
func (s *BlockService) GetTransferManager() *transfer.TransferManager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.transferManager
}

// RegisterCacheForShare associates a cache with a share.
// Since we use a single global cache, the shareName is ignored.
func (s *BlockService) RegisterCacheForShare(shareName string, sc *cache.Cache) error {
	return s.SetCache(sc)
}

// GetCacheForShare returns the cache for a share.
// Since we use a single global cache, the shareName is ignored.
func (s *BlockService) GetCacheForShare(shareName string) *cache.Cache {
	return s.GetCache()
}

// HasCache returns true if a cache is configured for the share.
// Since we use a single global cache, the shareName is ignored.
func (s *BlockService) HasCache(shareName string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.cache != nil
}

// ============================================================================
// Read Operations
// ============================================================================

// ReadAt reads data at the specified offset.
//
// Data is read from Cache using the Chunk/Slice/Block model.
// Reads span multiple chunks if the range crosses chunk boundaries,
// with slices merged using newest-wins semantics.
//
// When transfer manager is configured and data is not in cache:
//   - Falls back to block store to fetch the data
//   - Downloaded blocks are cached for future reads
func (s *BlockService) ReadAt(ctx context.Context, shareName string, id metadata.ContentID, p []byte, offset uint64) (int, error) {
	sc := s.GetCache()
	if sc == nil {
		return 0, ErrNoCacheConfigured
	}

	if len(p) == 0 {
		return 0, nil
	}

	// Use ContentID as file handle
	fileHandle := []byte(id)
	contentID := string(id)
	tm := s.GetTransferManager()

	// Calculate which chunks this read spans
	startChunk, endChunk := cache.ChunkRange(offset, uint64(len(p)))

	totalRead := 0
	for chunkIdx := startChunk; chunkIdx <= endChunk; chunkIdx++ {
		// Calculate the portion of buffer for this chunk
		chunkStart := uint64(chunkIdx) * cache.ChunkSize
		chunkEnd := chunkStart + cache.ChunkSize

		// Calculate read range within this chunk
		readStart := max(offset+uint64(totalRead), chunkStart)
		readEnd := min(offset+uint64(len(p)), chunkEnd)

		// Calculate offsets
		offsetInChunk := uint32(readStart - chunkStart)
		length := uint32(readEnd - readStart)
		bufStart := int(readStart - offset)

		// Read from this chunk
		data, found, err := sc.ReadSlice(ctx, fileHandle, chunkIdx, offsetInChunk, length)
		if err != nil && err != cache.ErrFileNotInCache {
			return totalRead, fmt.Errorf("read slice from chunk %d failed: %w", chunkIdx, err)
		}

		if !found {
			// Cache miss - try to fetch from block store if transfer manager is configured
			if tm != nil {
				blockData, err := tm.ReadBlocks(ctx, shareName, fileHandle, contentID, chunkIdx, offsetInChunk, length)
				if err != nil {
					// Block store fetch failed - fall back to zeros (sparse file behavior)
					logger.Debug("Block store fetch failed, using zeros", "chunk", chunkIdx, "error", err)
					for i := bufStart; i < bufStart+int(length); i++ {
						p[i] = 0
					}
				} else {
					copy(p[bufStart:], blockData)
				}
			} else {
				// No transfer manager - fill with zeros (sparse file behavior for cache-only mode)
				for i := bufStart; i < bufStart+int(length); i++ {
					p[i] = 0
				}
			}
		} else {
			// Copy data to buffer
			copy(p[bufStart:], data)
		}

		totalRead = bufStart + int(length)
	}

	return totalRead, nil
}

// GetContentSize returns the size of content for a file.
func (s *BlockService) GetContentSize(ctx context.Context, shareName string, id metadata.ContentID) (uint64, error) {
	sc := s.GetCache()
	if sc == nil {
		return 0, ErrNoCacheConfigured
	}

	fileHandle := []byte(id)
	return sc.GetFileSize(fileHandle), nil
}

// ContentExists checks if content exists for the file.
func (s *BlockService) ContentExists(ctx context.Context, shareName string, id metadata.ContentID) (bool, error) {
	sc := s.GetCache()
	if sc == nil {
		return false, ErrNoCacheConfigured
	}

	fileHandle := []byte(id)
	// File exists if it has any cached data
	return sc.GetFileSize(fileHandle) > 0, nil
}

// ============================================================================
// Write Operations
// ============================================================================

// WriteAt writes data at the specified offset.
//
// Writes go to Cache using the Chunk/Slice/Block model.
// Data is split across chunk boundaries and stored as slices within each chunk.
// Block store uploads are triggered on Flush() via background worker pool.
func (s *BlockService) WriteAt(ctx context.Context, shareName string, id metadata.ContentID, data []byte, offset uint64) error {
	sc := s.GetCache()
	if sc == nil {
		return ErrNoCacheConfigured
	}

	if len(data) == 0 {
		return nil
	}

	// Use ContentID as file handle
	fileHandle := []byte(id)

	// Calculate which chunks this write spans
	startChunk, endChunk := cache.ChunkRange(offset, uint64(len(data)))

	dataOffset := 0
	for chunkIdx := startChunk; chunkIdx <= endChunk; chunkIdx++ {
		// Calculate the portion of data that goes into this chunk
		chunkStart := uint64(chunkIdx) * cache.ChunkSize
		chunkEnd := chunkStart + cache.ChunkSize

		// Calculate write range within this chunk
		writeStart := max(offset+uint64(dataOffset), chunkStart)
		writeEnd := min(offset+uint64(len(data)), chunkEnd)

		// Calculate offsets
		offsetInChunk := uint32(writeStart - chunkStart)
		dataStart := int(writeStart - offset)
		dataEnd := int(writeEnd - offset)

		// Write slice to this chunk
		err := sc.WriteSlice(ctx, fileHandle, chunkIdx, data[dataStart:dataEnd], offsetInChunk)
		if err != nil {
			return fmt.Errorf("write slice to chunk %d failed: %w", chunkIdx, err)
		}

		dataOffset = dataEnd
	}

	return nil
}

// Truncate truncates content to the specified size.
func (s *BlockService) Truncate(ctx context.Context, shareName string, id metadata.ContentID, newSize uint64) error {
	sc := s.GetCache()
	if sc == nil {
		return ErrNoCacheConfigured
	}

	fileHandle := []byte(id)
	return sc.Truncate(ctx, fileHandle, newSize)
}

// Delete removes content for a file.
func (s *BlockService) Delete(ctx context.Context, shareName string, id metadata.ContentID) error {
	sc := s.GetCache()
	if sc == nil {
		return ErrNoCacheConfigured
	}

	fileHandle := []byte(id)
	return sc.Remove(ctx, fileHandle)
}

// ============================================================================
// Flush Operations
// ============================================================================

// Flush flushes cached data for a file.
//
// When transfer manager is configured (hybrid flush):
//   - Syncs cache to disk (fast, local) for crash recovery
//   - Enqueues remaining data for background block store upload (non-blocking)
//
// When cache-only (no transfer manager):
//   - Coalesces writes to optimize slice count
func (s *BlockService) Flush(ctx context.Context, shareName string, id metadata.ContentID) (*FlushResult, error) {
	sc := s.GetCache()
	if sc == nil {
		return nil, ErrNoCacheConfigured
	}

	fileHandle := []byte(id)
	tm := s.GetTransferManager()

	// If transfer manager is configured, enqueue background upload
	// Data is in mmap cache (crash-safe), block store upload is async
	if tm != nil {
		contentID := string(id)
		// Non-blocking enqueue - uploads happen in background worker pool
		_ = tm.FlushRemainingAsync(ctx, shareName, fileHandle, contentID)
		return &FlushResult{
			AlreadyFlushed: false,
			Finalized:      true,
		}, nil
	}

	// Cache-only mode: coalesce writes for optimization
	if sc.HasDirtyData(fileHandle) {
		if err := sc.CoalesceWrites(ctx, fileHandle); err != nil {
			logger.Warn("Failed to coalesce writes", "file", string(fileHandle), "error", err)
		}
	}

	return &FlushResult{
		AlreadyFlushed: true,
		Finalized:      true,
	}, nil
}

// FlushAndFinalize flushes and finalizes for immediate durability.
//
// This is called by SMB CLOSE which requires full durability before returning.
// Unlike Flush(), this BLOCKS until all data is persisted to block store.
func (s *BlockService) FlushAndFinalize(ctx context.Context, shareName string, id metadata.ContentID) (*FlushResult, error) {
	sc := s.GetCache()
	if sc == nil {
		return nil, ErrNoCacheConfigured
	}

	fileHandle := []byte(id)
	contentID := string(id)
	tm := s.GetTransferManager()

	// If transfer manager is configured, use blocking flush for full durability
	if tm != nil {
		// Wait for any in-flight eager uploads
		if err := tm.WaitForUploads(ctx, contentID); err != nil {
			return nil, fmt.Errorf("wait for uploads: %w", err)
		}

		// Flush remaining blocks (blocking)
		if err := tm.FlushRemaining(ctx, shareName, fileHandle, contentID); err != nil {
			return nil, fmt.Errorf("flush remaining: %w", err)
		}

		return &FlushResult{
			AlreadyFlushed: false,
			Finalized:      true,
		}, nil
	}

	// Cache-only mode: coalesce writes
	if sc.HasDirtyData(fileHandle) {
		if err := sc.CoalesceWrites(ctx, fileHandle); err != nil {
			logger.Warn("Failed to coalesce writes", "file", string(fileHandle), "error", err)
		}
	}

	return &FlushResult{
		AlreadyFlushed: true,
		Finalized:      true,
	}, nil
}

// ============================================================================
// Capability Detection
// ============================================================================

// SupportsReadAt returns true if the service supports efficient random reads.
// Always true when a cache is configured.
func (s *BlockService) SupportsReadAt(shareName string) bool {
	return s.HasCache(shareName)
}

// ============================================================================
// Statistics and Health
// ============================================================================

// GetStorageStats returns storage statistics.
func (s *BlockService) GetStorageStats(ctx context.Context, shareName string) (*StorageStats, error) {
	sc := s.GetCache()
	if sc == nil {
		return nil, ErrNoCacheConfigured
	}

	// Count files in cache
	files := sc.ListFiles()
	return &StorageStats{
		UsedSize:     0, // Stats tracking removed for simplicity
		ContentCount: uint64(len(files)),
	}, nil
}

// Healthcheck performs health check.
func (s *BlockService) Healthcheck(ctx context.Context, shareName string) error {
	if !s.HasCache(shareName) {
		return ErrNoCacheConfigured
	}
	return nil
}
