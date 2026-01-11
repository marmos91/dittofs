package content

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/bytesize"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ContentService provides all content operations for the filesystem.
//
// It manages content stores and caches, routing operations to the correct store
// based on share name. All protocol handlers should interact with ContentService
// rather than accessing stores directly.
//
// Cache Integration:
// ContentService owns cache coordination. When a cache is registered for a share:
//   - Writes go to cache first, then are flushed to store on COMMIT
//   - Reads check cache first, falling back to store on miss
//   - Flush operations sync cache to store
//
// Usage:
//
//	contentSvc := content.New()
//	contentSvc.RegisterStoreForShare("/export", memoryStore)
//	contentSvc.RegisterCacheForShare("/export", memoryCache)
//
//	// High-level operations (with caching)
//	err := contentSvc.WriteAt(ctx, "/export", contentID, data, offset)
//
//	// Low-level operations (direct store access)
//	store, err := contentSvc.GetStoreForShare("/export")
type ContentService struct {
	mu     sync.RWMutex
	stores map[string]ContentStore // shareName -> store
	caches map[string]cache.Cache  // shareName -> cache (optional)
}

// New creates a new empty ContentService instance.
// Use RegisterStoreForShare to configure stores for each share.
func New() *ContentService {
	return &ContentService{
		stores: make(map[string]ContentStore),
		caches: make(map[string]cache.Cache),
	}
}

// RegisterStoreForShare associates a content store with a share.
// Each share must have exactly one store. Calling this again for the same
// share will replace the previous store.
func (s *ContentService) RegisterStoreForShare(shareName string, store ContentStore) error {
	if store == nil {
		return fmt.Errorf("cannot register nil store for share %q", shareName)
	}
	if shareName == "" {
		return fmt.Errorf("cannot register store for empty share name")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.stores[shareName] = store
	return nil
}

// GetStoreForShare returns the content store for a specific share.
// This is primarily for internal use and testing; protocol handlers
// should use the high-level methods instead.
func (s *ContentService) GetStoreForShare(shareName string) (ContentStore, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if store, ok := s.stores[shareName]; ok {
		return store, nil
	}

	return nil, fmt.Errorf("%w: %s", ErrNoStoreForShare, shareName)
}

// RegisterCacheForShare associates a cache with a share.
// Caches are optional - if not registered, operations go directly to the store.
func (s *ContentService) RegisterCacheForShare(shareName string, c cache.Cache) error {
	if c == nil {
		return fmt.Errorf("cannot register nil cache for share %q", shareName)
	}
	if shareName == "" {
		return fmt.Errorf("cannot register cache for empty share name")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.caches[shareName] = c
	return nil
}

// GetCacheForShare returns the cache for a share.
// Returns nil if no cache is configured for the share.
func (s *ContentService) GetCacheForShare(shareName string) cache.Cache {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.caches[shareName]
}

// ============================================================================
// Read Operations
// ============================================================================

// ReadContent reads from cache or content store.
func (s *ContentService) ReadContent(ctx context.Context, shareName string, id metadata.ContentID) (io.ReadCloser, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, err
	}

	c := s.GetCacheForShare(shareName)
	if c != nil {
		// Check if content is in cache
		state := c.GetState(id)
		if state != cache.StateNone {
			// Read from cache
			data, err := c.Read(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("cache read error: %w", err)
			}
			return io.NopCloser(newBytesReader(data)), nil
		}
	}

	// Fall back to content store
	return store.ReadContent(ctx, id)
}

// ReadAt reads at offset, using cache or ReadAtContentStore if available.
func (s *ContentService) ReadAt(ctx context.Context, shareName string, id metadata.ContentID, p []byte, offset uint64) (int, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return 0, err
	}

	c := s.GetCacheForShare(shareName)
	if c != nil {
		// Check if content is in cache
		state := c.GetState(id)
		if state != cache.StateNone {
			// Read from cache
			return c.ReadAt(ctx, id, p, offset)
		}
	}

	// Try ReadAtContentStore if available
	if readAtStore, ok := store.(ReadAtContentStore); ok {
		return readAtStore.ReadAt(ctx, id, p, offset)
	}

	// Fall back to sequential read
	reader, err := store.ReadContent(ctx, id)
	if err != nil {
		return 0, err
	}
	defer func() { _ = reader.Close() }()

	// Skip to offset
	if offset > 0 {
		_, err = io.CopyN(io.Discard, reader, int64(offset))
		if err != nil {
			if err == io.EOF {
				return 0, io.EOF
			}
			return 0, fmt.Errorf("seek error: %w", err)
		}
	}

	// Read requested bytes
	return io.ReadFull(reader, p)
}

// GetContentSize returns content size (from cache or store).
func (s *ContentService) GetContentSize(ctx context.Context, shareName string, id metadata.ContentID) (uint64, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return 0, err
	}

	c := s.GetCacheForShare(shareName)
	if c != nil {
		// Check if content is in cache
		state := c.GetState(id)
		if state != cache.StateNone {
			return c.Size(id), nil
		}
	}

	return store.GetContentSize(ctx, id)
}

// ContentExists checks if content exists in cache or store.
func (s *ContentService) ContentExists(ctx context.Context, shareName string, id metadata.ContentID) (bool, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return false, err
	}

	c := s.GetCacheForShare(shareName)
	if c != nil && c.Exists(id) {
		return true, nil
	}

	return store.ContentExists(ctx, id)
}

// ============================================================================
// Write Operations
// ============================================================================

// WriteAt writes to cache (if configured) or directly to content store.
func (s *ContentService) WriteAt(ctx context.Context, shareName string, id metadata.ContentID, data []byte, offset uint64) error {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return err
	}

	c := s.GetCacheForShare(shareName)
	if c != nil {
		// Write to cache
		return c.WriteAt(ctx, id, data, offset)
	}

	// No cache - write directly to store
	return store.WriteAt(ctx, id, data, offset)
}

// WriteContent writes complete content to cache or store.
func (s *ContentService) WriteContent(ctx context.Context, shareName string, id metadata.ContentID, data []byte) error {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return err
	}

	c := s.GetCacheForShare(shareName)
	if c != nil {
		// Write to cache
		return c.Write(ctx, id, data)
	}

	// No cache - write directly to store
	return store.WriteContent(ctx, id, data)
}

// Truncate truncates content in cache or store.
func (s *ContentService) Truncate(ctx context.Context, shareName string, id metadata.ContentID, newSize uint64) error {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return err
	}

	// For truncate, we need to handle both cache and store
	// If content is in cache, truncate there (will be flushed later)
	// If content is only in store, truncate in store directly

	c := s.GetCacheForShare(shareName)
	if c != nil {
		state := c.GetState(id)
		if state != cache.StateNone {
			// Content is cached - we need to handle truncate in cache
			// For now, just clear the cache entry and truncate in store
			// A more sophisticated approach would truncate the cached data
			if err := c.Remove(id); err != nil {
				logger.Warn("Failed to remove cache entry on truncate", "content_id", id, "error", err)
			}
		}
	}

	return store.Truncate(ctx, id, newSize)
}

// Delete removes content from store (and cache if present).
func (s *ContentService) Delete(ctx context.Context, shareName string, id metadata.ContentID) error {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return err
	}

	// Remove from cache first if present
	c := s.GetCacheForShare(shareName)
	if c != nil {
		if err := c.Remove(id); err != nil {
			logger.Warn("Failed to remove cache entry on delete", "content_id", id, "error", err)
		}
	}

	// Delete from store
	return store.Delete(ctx, id)
}

// ============================================================================
// Flush Operations
// ============================================================================

// Flush flushes cached data to content store.
func (s *ContentService) Flush(ctx context.Context, shareName string, id metadata.ContentID) (*FlushResult, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, err
	}

	c := s.GetCacheForShare(shareName)
	if c == nil {
		// No cache - nothing to flush
		return &FlushResult{AlreadyFlushed: true}, nil
	}

	return s.flushCacheToStore(ctx, c, store, id)
}

// FlushAndFinalize flushes and finalizes for immediate durability.
func (s *ContentService) FlushAndFinalize(ctx context.Context, shareName string, id metadata.ContentID) (*FlushResult, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, err
	}

	c := s.GetCacheForShare(shareName)
	if c == nil {
		// No cache - nothing to flush
		return &FlushResult{AlreadyFlushed: true, Finalized: true}, nil
	}

	// First, flush any pending data
	result, err := s.flushCacheToStore(ctx, c, store, id)
	if err != nil {
		return nil, err
	}

	// For incremental stores (S3), we need to finalize the upload
	if incStore, ok := store.(IncrementalWriteStore); ok {
		err := incStore.CompleteIncrementalWrite(ctx, id, c)
		if err != nil {
			return nil, fmt.Errorf("failed to complete incremental write: %w", err)
		}

		// Transition to StateCached (clean, can be evicted)
		c.SetState(id, cache.StateCached)
		result.Finalized = true

		logger.Info("Flush: finalized upload", "content_id", id)
	} else {
		result.Finalized = true
	}

	return result, nil
}

// flushCacheToStore is the core flush logic.
func (s *ContentService) flushCacheToStore(
	ctx context.Context,
	c cache.Cache,
	store ContentStore,
	id metadata.ContentID,
) (*FlushResult, error) {
	cacheSize := c.Size(id)
	flushedOffset := c.GetFlushedOffset(id)

	// Check for incremental write support first (S3)
	if incStore, ok := store.(IncrementalWriteStore); ok {
		// Incremental write (S3): parallel multipart uploads
		flushed, err := incStore.FlushIncremental(ctx, id, c)
		if err != nil {
			return nil, fmt.Errorf("incremental flush error: %w", err)
		}

		// Transition to StateUploading so the background flusher can complete
		c.SetState(id, cache.StateUploading)

		logger.Info("Flush: flushed incrementally", "bytes", bytesize.ByteSize(flushed), "content_id", id)

		return &FlushResult{
			BytesFlushed: flushed,
			Incremental:  true,
		}, nil
	}

	// WriteAt-capable store (filesystem, memory): write only new bytes
	bytesToFlush := cacheSize - flushedOffset
	if bytesToFlush <= 0 {
		logger.Info("Flush: already up to date", "bytes", bytesize.ByteSize(0), "content_id", id)

		return &FlushResult{
			BytesFlushed:   0,
			AlreadyFlushed: true,
		}, nil
	}

	buf := make([]byte, bytesToFlush)
	n, err := c.ReadAt(ctx, id, buf, flushedOffset)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("cache read error: %w", err)
	}

	err = store.WriteAt(ctx, id, buf[:n], flushedOffset)
	if err != nil {
		return nil, fmt.Errorf("content store write error: %w", err)
	}

	c.SetFlushedOffset(id, flushedOffset+uint64(n))

	// Transition to StateUploading so the background flusher can finalize
	c.SetState(id, cache.StateUploading)

	logger.Info("Flush: flushed", "bytes", bytesize.ByteSize(n), "offset", bytesize.ByteSize(flushedOffset), "content_id", id)

	return &FlushResult{
		BytesFlushed: uint64(n),
	}, nil
}

// ============================================================================
// Capability Detection
// ============================================================================

// SupportsReadAt returns true if the store supports efficient random reads.
func (s *ContentService) SupportsReadAt(shareName string) bool {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return false
	}

	_, ok := store.(ReadAtContentStore)
	return ok
}

// SupportsIncrementalWrite returns true if the store supports incremental writes.
func (s *ContentService) SupportsIncrementalWrite(shareName string) bool {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return false
	}

	_, ok := store.(IncrementalWriteStore)
	return ok
}

// ============================================================================
// Statistics and Health
// ============================================================================

// GetStorageStats returns storage statistics for a share.
func (s *ContentService) GetStorageStats(ctx context.Context, shareName string) (*StorageStats, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, err
	}

	return store.GetStorageStats(ctx)
}

// Healthcheck performs health check for a share's content store.
func (s *ContentService) Healthcheck(ctx context.Context, shareName string) error {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return err
	}

	return store.Healthcheck(ctx)
}

// ============================================================================
// Helper Types
// ============================================================================

// bytesReader wraps a byte slice to implement io.Reader
type bytesReader struct {
	data   []byte
	offset int
}

func newBytesReader(data []byte) *bytesReader {
	return &bytesReader{data: data}
}

func (r *bytesReader) Read(p []byte) (n int, err error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n = copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}
