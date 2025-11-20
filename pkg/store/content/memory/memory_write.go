// Package memory implements in-memory content storage for DittoFS.
//
// This file contains write operations for the memory content store, including
// full content writes, partial writes, truncation, and deletion.
package memory

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// ============================================================================
// WritableContentStore Interface Implementation
// ============================================================================

// WriteAt writes data at the specified offset.
//
// This implements sparse file semantics:
//   - If content doesn't exist: create with zeros up to offset, then data
//   - If offset > current size: extend with zeros, then write data
//   - If offset < current size: overwrite existing data
//
// Context Cancellation:
// Checked before acquiring the lock. The write itself is atomic.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier (created if doesn't exist)
//   - data: Data to write
//   - offset: Byte offset where writing begins
//
// Returns:
//   - error: Returns error if offset is negative or context is cancelled
func (s *MemoryContentStore) WriteAt(ctx context.Context, id metadata.ContentID, data []byte, offset int64) error {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return err
	}

	// ========================================================================
	// Step 2: Validate offset
	// ========================================================================

	if offset < 0 {
		return fmt.Errorf("offset %d: %w", offset, content.ErrInvalidOffset)
	}

	// ========================================================================
	// Step 3: Acquire write lock and perform write
	// ========================================================================

	s.mu.Lock()
	defer s.mu.Unlock()

	// Get existing content or create new
	existing, exists := s.data[id]

	// Calculate new size needed
	newSize := int(offset) + len(data)

	// Create new buffer
	var result []byte
	if exists && len(existing) > newSize {
		// Existing content is larger, preserve it
		result = make([]byte, len(existing))
		copy(result, existing)
	} else {
		// Need to expand
		result = make([]byte, newSize)
		if exists {
			// Copy existing data
			copy(result, existing)
		}
		// Gap between old size and offset is already zeros (from make())
	}

	// Write new data at offset
	copy(result[offset:], data)

	// Store updated content
	s.data[id] = result

	return nil
}

// Truncate changes the size of the content.
//
// Truncate Semantics:
//   - If newSize < currentSize: Content is truncated (trailing data removed)
//   - If newSize > currentSize: Content is extended with zeros
//   - If newSize == currentSize: No-op (succeeds immediately)
//
// Context Cancellation:
// Checked before acquiring the lock. The truncate itself is atomic.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//   - newSize: New size in bytes
//
// Returns:
//   - error: ErrContentNotFound if content doesn't exist, or context errors
func (s *MemoryContentStore) Truncate(ctx context.Context, id metadata.ContentID, newSize uint64) error {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return err
	}

	// ========================================================================
	// Step 2: Acquire write lock and perform truncate
	// ========================================================================

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if content exists
	existing, exists := s.data[id]
	if !exists {
		return fmt.Errorf("truncate failed for %s: %w", id, content.ErrContentNotFound)
	}

	currentSize := uint64(len(existing))

	// No-op if size is the same
	if newSize == currentSize {
		return nil
	}

	// Create new buffer with new size
	newData := make([]byte, newSize)

	// Copy existing data (up to newSize)
	if newSize < currentSize {
		// Truncating: copy only newSize bytes
		copy(newData, existing[:newSize])
	} else {
		// Extending: copy all existing data, rest is zeros
		copy(newData, existing)
	}

	s.data[id] = newData

	return nil
}

// Delete removes content from the store.
//
// The operation is idempotent - deleting non-existent content returns nil.
// Memory is reclaimed immediately by the Go garbage collector.
//
// Context Cancellation:
// Checked before acquiring the lock. The delete itself is atomic.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier to delete
//
// Returns:
//   - error: Only returns error for context cancellation
func (s *MemoryContentStore) Delete(ctx context.Context, id metadata.ContentID) error {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return err
	}

	// ========================================================================
	// Step 2: Acquire write lock and delete
	// ========================================================================

	s.mu.Lock()
	defer s.mu.Unlock()

	// Delete is idempotent - no error if content doesn't exist
	delete(s.data, id)

	return nil
}

// WriteContent writes the entire content in one operation.
//
// This is a convenience method that replaces any existing content with
// the new data.
//
// Context Cancellation:
// Checked before acquiring the lock. The write itself is atomic.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier (created if doesn't exist, replaced if exists)
//   - data: Complete content data
//
// Returns:
//   - error: Returns error if context is cancelled
func (s *MemoryContentStore) WriteContent(ctx context.Context, id metadata.ContentID, data []byte) error {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return err
	}

	// ========================================================================
	// Step 2: Acquire write lock and write content
	// ========================================================================

	s.mu.Lock()
	defer s.mu.Unlock()

	// Create a copy to prevent external modifications
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	s.data[id] = dataCopy

	return nil
}
