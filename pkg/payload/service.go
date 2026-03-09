package payload

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload/offloader"
)

// PayloadService coordinates read/write I/O between cache and offloader,
// plus flush, stats, and health operations.
type PayloadService struct {
	cache     *cache.BlockCache
	offloader *offloader.Offloader
}

// New creates a new PayloadService. Both cache and offloader are required.
func New(c *cache.BlockCache, tm *offloader.Offloader) (*PayloadService, error) {
	if c == nil {
		return nil, fmt.Errorf("cache is required")
	}
	if tm == nil {
		return nil, fmt.Errorf("offloader is required")
	}

	return &PayloadService{
		cache:     c,
		offloader: tm,
	}, nil
}

// ReadAt reads data at the specified offset.
// Data is read from cache first, falling back to block store on cache miss.
func (s *PayloadService) ReadAt(ctx context.Context, id metadata.PayloadID, data []byte, offset uint64) (int, error) {
	return s.readAtInternal(ctx, id, "", data, offset)
}

// ReadAtWithCOWSource reads data using a COW source for lazy copy.
// If data is not found in the primary payloadID, it falls back to cowSource.
func (s *PayloadService) ReadAtWithCOWSource(ctx context.Context, id metadata.PayloadID, cowSource metadata.PayloadID, data []byte, offset uint64) (int, error) {
	return s.readAtInternal(ctx, id, cowSource, data, offset)
}

// readAtInternal reads from primary payloadID, falling back to cowSource on miss.
func (s *PayloadService) readAtInternal(ctx context.Context, id metadata.PayloadID, cowSource metadata.PayloadID, data []byte, offset uint64) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	payloadID := string(id)
	hasCOWSource := cowSource != ""

	// Try primary cache first
	found, err := s.cache.ReadAt(ctx, payloadID, data, offset)
	if err != nil {
		return 0, fmt.Errorf("cache read failed: %w", err)
	}
	if found {
		return len(data), nil
	}

	if hasCOWSource {
		if err := s.readFromCOWSource(ctx, payloadID, string(cowSource), data, offset); err != nil {
			return 0, err
		}
	} else {
		if err := s.ensureAndReadFromCache(ctx, payloadID, data, offset); err != nil {
			return 0, err
		}
	}

	return len(data), nil
}

// readFromCOWSource reads from the COW source and copies data to primary cache.
func (s *PayloadService) readFromCOWSource(ctx context.Context, payloadID, sourcePayloadID string, dest []byte, offset uint64) error {
	sourceFound, sourceErr := s.cache.ReadAt(ctx, sourcePayloadID, dest, offset)
	if sourceErr != nil {
		return fmt.Errorf("COW source read failed: %w", sourceErr)
	}

	if !sourceFound {
		err := s.offloader.EnsureAvailable(ctx, sourcePayloadID, offset, uint32(len(dest)))
		if err != nil {
			return fmt.Errorf("ensure available for COW source failed: %w", err)
		}

		sourceFound, sourceErr = s.cache.ReadAt(ctx, sourcePayloadID, dest, offset)
		if sourceErr != nil {
			return fmt.Errorf("COW source read after download failed: %w", sourceErr)
		}
		if !sourceFound {
			clear(dest)
			logger.Debug("Sparse COW block: returning zeros",
				"payloadID", sourcePayloadID)
		}
	}

	// Copy to primary cache for future reads (non-fatal if fails)
	if err := s.cache.WriteAt(ctx, payloadID, dest, offset); err != nil {
		logger.Debug("COW cache write failed (non-fatal)", "payloadID", payloadID, "error", err)
	}

	return nil
}

// ensureAndReadFromCache downloads blocks from the store if needed and reads from cache.
func (s *PayloadService) ensureAndReadFromCache(ctx context.Context, payloadID string, dest []byte, offset uint64) error {
	length := uint32(len(dest))

	// Fast path: direct-serve copies S3 data directly to dest, skipping a second ReadAt.
	filled, err := s.offloader.EnsureAvailableAndRead(ctx, payloadID, offset, length, dest)
	if err != nil {
		return fmt.Errorf("direct download failed: %w", err)
	}
	if filled {
		return nil
	}

	found, err := s.cache.ReadAt(ctx, payloadID, dest, offset)
	if err != nil {
		return fmt.Errorf("read after download failed: %w", err)
	}
	if !found {
		// Sparse block: cache did not store the data. Explicitly zero dest
		// since it may be a sub-slice of the caller's buffer with stale data.
		clear(dest)
		logger.Debug("Sparse block: cache miss after download, returning zeros",
			"payloadID", payloadID)
	}

	return nil
}

// WriteAt writes data at the specified offset.
// Writes go directly to cache. The periodic uploader handles background upload.
func (s *PayloadService) WriteAt(ctx context.Context, id metadata.PayloadID, data []byte, offset uint64) error {
	if len(data) == 0 {
		return nil
	}

	if err := s.cache.WriteAt(ctx, string(id), data, offset); err != nil {
		return fmt.Errorf("cache write failed: %w", err)
	}

	return nil
}

// Truncate truncates payload to the specified size in both cache and block store.
func (s *PayloadService) Truncate(ctx context.Context, id metadata.PayloadID, newSize uint64) error {
	payloadID := string(id)
	if err := s.cache.Truncate(ctx, payloadID, newSize); err != nil {
		return fmt.Errorf("cache truncate failed: %w", err)
	}
	return s.offloader.Truncate(ctx, payloadID, newSize)
}

// Delete removes payload from both cache and block store.
func (s *PayloadService) Delete(ctx context.Context, id metadata.PayloadID) error {
	payloadID := string(id)
	if err := s.cache.EvictMemory(ctx, payloadID); err != nil {
		return fmt.Errorf("cache evict memory failed: %w", err)
	}
	return s.offloader.Delete(ctx, payloadID)
}

// GetSize returns the size of payload, checking cache first then block store.
func (s *PayloadService) GetSize(ctx context.Context, id metadata.PayloadID) (uint64, error) {
	payloadID := string(id)
	if size, found := s.cache.GetFileSize(ctx, payloadID); found {
		return size, nil
	}
	return s.offloader.GetFileSize(ctx, payloadID)
}

// Exists checks if payload exists, checking cache first then block store.
func (s *PayloadService) Exists(ctx context.Context, id metadata.PayloadID) (bool, error) {
	payloadID := string(id)
	if _, found := s.cache.GetFileSize(ctx, payloadID); found {
		return true, nil
	}
	return s.offloader.Exists(ctx, payloadID)
}

// Flush enqueues remaining dirty data for background upload (non-blocking).
func (s *PayloadService) Flush(ctx context.Context, id metadata.PayloadID) (*FlushResult, error) {
	result, err := s.offloader.Flush(ctx, string(id))
	if err != nil {
		return nil, fmt.Errorf("flush failed: %w", err)
	}

	return result, nil
}

// DrainAllUploads waits for all in-flight uploads across all files to complete.
// Useful for benchmarking and testing to ensure clean boundaries between workloads.
func (s *PayloadService) DrainAllUploads(ctx context.Context) error {
	return s.offloader.DrainAllUploads(ctx)
}

// ============================================================================
// Statistics and Health
// ============================================================================

// GetStorageStats returns storage statistics.
func (s *PayloadService) GetStorageStats(_ context.Context) (*StorageStats, error) {
	files := s.cache.ListFiles()
	return &StorageStats{
		UsedSize:     0, // TODO: Implement proper stats tracking
		ContentCount: uint64(len(files)),
	}, nil
}

// HealthCheck verifies the block store is accessible.
func (s *PayloadService) HealthCheck(ctx context.Context) error {
	return s.offloader.HealthCheck(ctx)
}
