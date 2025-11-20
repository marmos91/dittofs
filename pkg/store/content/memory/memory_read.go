// Package memory implements in-memory content storage for DittoFS.
//
// This file contains read operations for the memory content store, including
// full content reads, seekable reads, size queries, and existence checks.
package memory

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// ============================================================================
// ContentStore Interface Implementation
// ============================================================================

// ReadContent returns a reader for the content identified by the given ID.
//
// The returned reader reads from a copy of the content, so modifications
// to the store after this call won't affect the reader.
//
// Context Cancellation:
// Only checked before acquiring the lock. Once the reader is returned,
// it's independent of the context.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier to read
//
// Returns:
//   - io.ReadCloser: Reader for the content (closing is a no-op)
//   - error: ErrContentNotFound if content doesn't exist, or context errors
func (s *MemoryContentStore) ReadContent(ctx context.Context, id metadata.ContentID) (io.ReadCloser, error) {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ========================================================================
	// Step 2: Acquire read lock and get content
	// ========================================================================

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Check if content exists
	data, exists := s.data[id]
	if !exists {
		return nil, fmt.Errorf("content %s: %w", id, content.ErrContentNotFound)
	}

	// ========================================================================
	// Step 3: Return a reader over a copy of the data
	// ========================================================================
	// This prevents data races if the content is later modified

	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	return io.NopCloser(bytes.NewReader(dataCopy)), nil
}

// GetContentSize returns the size of the content in bytes.
//
// This is a lightweight operation that just returns the length of the
// stored byte slice.
//
// Context Cancellation:
// Only checked before acquiring the lock.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//
// Returns:
//   - uint64: Size of the content in bytes
//   - error: ErrContentNotFound if content doesn't exist, or context errors
func (s *MemoryContentStore) GetContentSize(ctx context.Context, id metadata.ContentID) (uint64, error) {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return 0, err
	}

	// ========================================================================
	// Step 2: Acquire read lock and get size
	// ========================================================================

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Check if content exists
	data, exists := s.data[id]
	if !exists {
		return 0, fmt.Errorf("content %s: %w", id, content.ErrContentNotFound)
	}

	return uint64(len(data)), nil
}

// ContentExists checks if content with the given ID exists.
//
// This is a lightweight existence check that just checks the map.
//
// Context Cancellation:
// Only checked before acquiring the lock.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier to check
//
// Returns:
//   - bool: True if content exists, false otherwise
//   - error: Only returns error for context cancellation
func (s *MemoryContentStore) ContentExists(ctx context.Context, id metadata.ContentID) (bool, error) {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return false, err
	}

	// ========================================================================
	// Step 2: Acquire read lock and check existence
	// ========================================================================

	s.mu.RLock()
	defer s.mu.RUnlock()

	_, exists := s.data[id]
	return exists, nil
}

// ============================================================================
// SeekableContentStore Interface Implementation
// ============================================================================

// ReadContentSeekable returns a seekable reader for the content.
//
// The in-memory implementation supports efficient seeking. The reader
// operates on a copy of the content to prevent data races.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier to read
//
// Returns:
//   - io.ReadSeekCloser: Seekable reader (closing is a no-op)
//   - error: ErrContentNotFound if content doesn't exist, or context errors
func (s *MemoryContentStore) ReadContentSeekable(ctx context.Context, id metadata.ContentID) (io.ReadSeekCloser, error) {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ========================================================================
	// Step 2: Acquire read lock and get content
	// ========================================================================

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Check if content exists
	data, exists := s.data[id]
	if !exists {
		return nil, fmt.Errorf("content %s: %w", id, content.ErrContentNotFound)
	}

	// ========================================================================
	// Step 3: Return a seekable reader over a copy of the data
	// ========================================================================

	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	return &seekableReader{
		Reader: bytes.NewReader(dataCopy),
	}, nil
}

// seekableReader wraps bytes.Reader to add a Close method.
// bytes.Reader implements Read and Seek, we just add Close.
type seekableReader struct {
	*bytes.Reader
}

// Close implements io.Closer. This is a no-op for in-memory readers.
func (r *seekableReader) Close() error {
	return nil
}
