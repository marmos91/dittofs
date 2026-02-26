package io

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload/chunk"
)

// CacheReader abstracts cache read operations to avoid importing the cache package directly.
type CacheReader interface {
	// ReadAt reads data from the cache at the specified chunk offset.
	// Returns (found, error) where found indicates whether data was available.
	ReadAt(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32, dest []byte) (bool, error)

	// GetFileSize returns the cached file size and whether the file exists in cache.
	// Returns (0, false) if the file is not in cache.
	// Returns (0, true) if the file is in cache but has zero length (e.g., after truncation).
	GetFileSize(ctx context.Context, payloadID string) (uint64, bool)
}

// CacheWriter abstracts cache write operations to avoid importing the cache package directly.
type CacheWriter interface {
	// WriteAt writes data to the cache at the specified chunk offset.
	WriteAt(ctx context.Context, payloadID string, chunkIdx uint32, data []byte, offset uint32) error
}

// CacheStateManager abstracts cache state operations (remove, truncate).
type CacheStateManager interface {
	// Remove completely removes all cached data for a file.
	Remove(ctx context.Context, payloadID string) error

	// Truncate truncates cached data to the specified size.
	Truncate(ctx context.Context, payloadID string, newSize uint64) error
}

// BlockDownloader abstracts offloader download operations to avoid importing the offloader package directly.
type BlockDownloader interface {
	// EnsureAvailable downloads required blocks from the block store if not cached.
	// May also trigger prefetch for sequential read optimization.
	EnsureAvailable(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32) error

	// GetFileSize returns the file size from the block store.
	GetFileSize(ctx context.Context, payloadID string) (uint64, error)

	// Exists checks if payload exists in the block store.
	Exists(ctx context.Context, payloadID string) (bool, error)
}

// BlockUploader abstracts offloader write-path operations to avoid importing the offloader package directly.
type BlockUploader interface {
	// OnWriteComplete notifies the offloader that a write to a block has completed.
	// The offloader may trigger eager upload for complete 4MB blocks.
	OnWriteComplete(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32)

	// Truncate truncates the payload in the block store to the specified size.
	Truncate(ctx context.Context, payloadID string, newSize uint64) error

	// Delete removes the payload from the block store.
	Delete(ctx context.Context, payloadID string) error
}

// CacheFileNotFoundError is used to detect cache miss errors without importing the cache package.
// Must be set by the caller before using the io package (typically in an init function
// or during service construction). Defaults to nil, which means cache miss errors are not
// distinguished from other errors.
var CacheFileNotFoundError error

// CacheFullError is used to detect cache-full errors without importing the cache package.
// Must be set by the caller before using the io package (typically in an init function
// or during service construction). Defaults to nil, which means cache-full errors are
// not retried.
var CacheFullError error

// ServiceImpl provides read and write I/O operations for the payload service.
//
// It coordinates between the cache (fast in-memory/mmap storage) and the
// offloader (durable block store persistence) for data access.
type ServiceImpl struct {
	cacheReader     CacheReader
	cacheWriter     CacheWriter
	cacheState      CacheStateManager
	blockDownloader BlockDownloader
	blockUploader   BlockUploader
}

// New creates a new ServiceImpl with the required cache and offloader dependencies.
//
// The cache and offloader parameters satisfy separate read/write/state interfaces.
// In practice, the same concrete object (e.g., *cache.Cache) implements CacheReader,
// CacheWriter, and CacheStateManager.
func New(cr CacheReader, cw CacheWriter, cs CacheStateManager, bd BlockDownloader, bu BlockUploader) *ServiceImpl {
	return &ServiceImpl{
		cacheReader:     cr,
		cacheWriter:     cw,
		cacheState:      cs,
		blockDownloader: bd,
		blockUploader:   bu,
	}
}

// ReadAt reads data at the specified offset.
//
// Data is read from cache first, falling back to block store on cache miss.
// Reads span multiple blocks/chunks if the range crosses boundaries.
//
// On cache miss, uses EnsureAvailable which downloads required blocks and
// triggers prefetch for sequential read optimization.
func (s *ServiceImpl) ReadAt(ctx context.Context, id metadata.PayloadID, data []byte, offset uint64) (int, error) {
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
func (s *ServiceImpl) ReadAtWithCOWSource(ctx context.Context, id metadata.PayloadID, cowSource metadata.PayloadID, data []byte, offset uint64) (int, error) {
	return s.readAtInternal(ctx, id, cowSource, data, offset)
}

// readAtInternal is the shared implementation for ReadAt and ReadAtWithCOWSource.
//
// When cowSource is empty, reads from the primary payloadID only.
// When cowSource is provided, falls back to COW source on cache miss and copies
// the data to the primary cache for future reads.
func (s *ServiceImpl) readAtInternal(ctx context.Context, id metadata.PayloadID, cowSource metadata.PayloadID, data []byte, offset uint64) (int, error) {
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
		found, err := s.cacheReader.ReadAt(ctx, payloadID, blockRange.ChunkIndex, chunkOffset, blockRange.Length, dest)
		if err != nil && err != CacheFileNotFoundError {
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
func (s *ServiceImpl) readFromCOWSource(ctx context.Context, payloadID, sourcePayloadID string, blockRange chunk.BlockRange, chunkOffset uint32, dest []byte) error {
	// Try COW source cache
	sourceFound, sourceErr := s.cacheReader.ReadAt(ctx, sourcePayloadID, blockRange.ChunkIndex, chunkOffset, blockRange.Length, dest)
	if sourceErr != nil && sourceErr != CacheFileNotFoundError {
		return fmt.Errorf("COW source read block %d/%d failed: %w", blockRange.ChunkIndex, blockRange.BlockIndex, sourceErr)
	}

	if !sourceFound {
		// Not in COW source cache - fetch from block store
		err := s.blockDownloader.EnsureAvailable(ctx, sourcePayloadID, blockRange.ChunkIndex, chunkOffset, blockRange.Length)
		if err != nil {
			return fmt.Errorf("ensure available for COW source block %d/%d failed: %w", blockRange.ChunkIndex, blockRange.BlockIndex, err)
		}

		// Read from source cache (now populated from block store)
		sourceFound, sourceErr = s.cacheReader.ReadAt(ctx, sourcePayloadID, blockRange.ChunkIndex, chunkOffset, blockRange.Length, dest)
		if sourceErr != nil || !sourceFound {
			return fmt.Errorf("COW source data not in cache after download for block %d/%d", blockRange.ChunkIndex, blockRange.BlockIndex)
		}
	}

	// Copy to primary cache for future reads (non-fatal if fails)
	if err := s.cacheWriter.WriteAt(ctx, payloadID, blockRange.ChunkIndex, dest, chunkOffset); err != nil {
		logger.Debug("COW cache write failed (non-fatal)", "payloadID", payloadID, "error", err)
	}

	return nil
}

// ensureAndReadFromCache ensures data is available from block store and reads it.
func (s *ServiceImpl) ensureAndReadFromCache(ctx context.Context, payloadID string, blockRange chunk.BlockRange, chunkOffset uint32, dest []byte) error {
	// Cache miss - ensure data is available (downloads + prefetch)
	err := s.blockDownloader.EnsureAvailable(ctx, payloadID, blockRange.ChunkIndex, chunkOffset, blockRange.Length)
	if err != nil {
		return fmt.Errorf("ensure available for block %d/%d failed: %w", blockRange.ChunkIndex, blockRange.BlockIndex, err)
	}

	// Now read from cache
	found, err := s.cacheReader.ReadAt(ctx, payloadID, blockRange.ChunkIndex, chunkOffset, blockRange.Length, dest)
	if err != nil || !found {
		return fmt.Errorf("data not in cache after download for block %d/%d", blockRange.ChunkIndex, blockRange.BlockIndex)
	}

	return nil
}

// GetSize returns the size of payload for a file.
//
// Checks cache first, falls back to block store metadata.
func (s *ServiceImpl) GetSize(ctx context.Context, id metadata.PayloadID) (uint64, error) {
	payloadID := string(id)

	// Check cache first. The (size, found) return distinguishes
	// "not in cache" from "zero-length file in cache".
	if size, found := s.cacheReader.GetFileSize(ctx, payloadID); found {
		return size, nil
	}

	// Fall back to block store
	return s.blockDownloader.GetFileSize(ctx, payloadID)
}

// Exists checks if payload exists for the file.
//
// Checks cache first, falls back to block store.
func (s *ServiceImpl) Exists(ctx context.Context, id metadata.PayloadID) (bool, error) {
	payloadID := string(id)

	// Check cache first. The (size, found) return correctly handles
	// zero-length files that are in cache but have size 0.
	if _, found := s.cacheReader.GetFileSize(ctx, payloadID); found {
		return true, nil
	}

	// Fall back to block store
	return s.blockDownloader.Exists(ctx, payloadID)
}
