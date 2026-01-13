package content

import (
	"context"
	"fmt"
	"sync"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/flusher"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ContentService provides all content operations for the filesystem.
//
// It manages a single global Cache that serves all shares.
// ContentID uniqueness guarantees data isolation between files.
// All protocol handlers should interact with ContentService rather than
// accessing the cache directly.
//
// Cache Model:
//   - All writes go to Cache using WriteSlice API
//   - Reads merge slices using newest-wins semantics
//   - Flusher handles cache-to-S3 persistence (optional)
//
// Usage:
//
//	contentSvc := content.New()
//	contentSvc.SetCache(sliceCache)
//	contentSvc.SetFlusher(flusherInstance) // Optional: enables S3 persistence
//	err := contentSvc.WriteAt(ctx, shareName, contentID, data, offset)
type ContentService struct {
	mu      sync.RWMutex
	cache   *cache.Cache     // Single global cache for all shares
	flusher *flusher.Flusher // Optional: handles cache-to-S3 persistence
}

// New creates a new empty ContentService instance.
// Use SetCache to configure the global cache.
func New() *ContentService {
	return &ContentService{}
}

// SetCache sets the global Cache for the service.
//
// Cache is the Chunk/Slice/Block model that provides:
//   - Slice-aware caching with newest-wins merge semantics
//   - Efficient random writes (no read-modify-write on S3)
//   - Write coalescing for better performance
//
// This should be called once during initialization.
func (s *ContentService) SetCache(sc *cache.Cache) error {
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
func (s *ContentService) GetCache() *cache.Cache {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.cache
}

// SetFlusher sets the flusher for cache-to-S3 persistence.
//
// When a flusher is configured:
//   - Writes trigger eager block uploads (4MB blocks uploaded as they become ready)
//   - Flush() waits for in-flight uploads and flushes remaining data to S3
//   - ReadAt() falls back to S3 on cache miss
//
// This is optional - without a flusher, the service operates in cache-only mode.
func (s *ContentService) SetFlusher(f *flusher.Flusher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flusher = f
}

// GetFlusher returns the flusher, or nil if not configured.
func (s *ContentService) GetFlusher() *flusher.Flusher {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.flusher
}

// RegisterCacheForShare associates a cache with a share.
// Since we use a single global cache, the shareName is ignored.
func (s *ContentService) RegisterCacheForShare(shareName string, sc *cache.Cache) error {
	return s.SetCache(sc)
}

// GetCacheForShare returns the cache for a share.
// Since we use a single global cache, the shareName is ignored.
func (s *ContentService) GetCacheForShare(shareName string) *cache.Cache {
	return s.GetCache()
}

// HasCache returns true if a cache is configured for the share.
// Since we use a single global cache, the shareName is ignored.
func (s *ContentService) HasCache(shareName string) bool {
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
// When flusher is configured and data is not in cache:
//   - Falls back to S3 to fetch the data
//   - Downloaded blocks are cached for future reads
func (s *ContentService) ReadAt(ctx context.Context, shareName string, id metadata.ContentID, p []byte, offset uint64) (int, error) {
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
	f := s.GetFlusher()

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
			// Cache miss - try to fetch from S3 if flusher is configured
			if f != nil {
				s3Data, err := f.ReadBlocks(ctx, shareName, fileHandle, contentID, chunkIdx, offsetInChunk, length)
				if err != nil {
					// S3 fetch failed - fall back to zeros (sparse file behavior)
					logger.Debug("S3 fetch failed, using zeros", "chunk", chunkIdx, "error", err)
					for i := bufStart; i < bufStart+int(length); i++ {
						p[i] = 0
					}
				} else {
					copy(p[bufStart:], s3Data)
				}
			} else {
				// No flusher - fill with zeros (sparse file behavior for cache-only mode)
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
func (s *ContentService) GetContentSize(ctx context.Context, shareName string, id metadata.ContentID) (uint64, error) {
	sc := s.GetCache()
	if sc == nil {
		return 0, ErrNoCacheConfigured
	}

	fileHandle := []byte(id)
	return sc.GetFileSize(fileHandle), nil
}

// ContentExists checks if content exists for the file.
func (s *ContentService) ContentExists(ctx context.Context, shareName string, id metadata.ContentID) (bool, error) {
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
// S3 uploads are triggered on Flush() via background worker pool.
func (s *ContentService) WriteAt(ctx context.Context, shareName string, id metadata.ContentID, data []byte, offset uint64) error {
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
func (s *ContentService) Truncate(ctx context.Context, shareName string, id metadata.ContentID, newSize uint64) error {
	sc := s.GetCache()
	if sc == nil {
		return ErrNoCacheConfigured
	}

	fileHandle := []byte(id)
	return sc.Truncate(ctx, fileHandle, newSize)
}

// Delete removes content for a file.
func (s *ContentService) Delete(ctx context.Context, shareName string, id metadata.ContentID) error {
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
// When flusher is configured (hybrid flush):
//   - Syncs cache to disk (fast, local) for crash recovery
//   - Enqueues remaining data for background S3 upload (non-blocking)
//
// When cache-only (no flusher):
//   - Coalesces writes to optimize slice count
func (s *ContentService) Flush(ctx context.Context, shareName string, id metadata.ContentID) (*FlushResult, error) {
	sc := s.GetCache()
	if sc == nil {
		return nil, ErrNoCacheConfigured
	}

	fileHandle := []byte(id)
	f := s.GetFlusher()

	// If flusher is configured, enqueue background upload
	// Data is in mmap cache (crash-safe), S3 upload is async
	if f != nil {
		contentID := string(id)
		// Non-blocking enqueue - uploads happen in background worker pool
		_ = f.FlushRemainingAsync(ctx, shareName, fileHandle, contentID)
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
// Unlike Flush(), this BLOCKS until all data is persisted to S3.
func (s *ContentService) FlushAndFinalize(ctx context.Context, shareName string, id metadata.ContentID) (*FlushResult, error) {
	sc := s.GetCache()
	if sc == nil {
		return nil, ErrNoCacheConfigured
	}

	fileHandle := []byte(id)
	contentID := string(id)
	f := s.GetFlusher()

	// If flusher is configured, use blocking flush for full durability
	if f != nil {
		// Wait for any in-flight eager uploads
		if err := f.WaitForUploads(ctx, contentID); err != nil {
			return nil, fmt.Errorf("wait for uploads: %w", err)
		}

		// Flush remaining blocks (blocking)
		if err := f.FlushRemaining(ctx, shareName, fileHandle, contentID); err != nil {
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
func (s *ContentService) SupportsReadAt(shareName string) bool {
	return s.HasCache(shareName)
}

// ============================================================================
// Statistics and Health
// ============================================================================

// GetStorageStats returns storage statistics.
func (s *ContentService) GetStorageStats(ctx context.Context, shareName string) (*StorageStats, error) {
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
func (s *ContentService) Healthcheck(ctx context.Context, shareName string) error {
	if !s.HasCache(shareName) {
		return ErrNoCacheConfigured
	}
	return nil
}
