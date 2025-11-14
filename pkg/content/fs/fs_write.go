// Package fs implements filesystem-based content storage for DittoFS.
//
// This file contains write operations for the filesystem content store, including
// full content writes, partial writes at offsets, truncation, and deletion.
package fs

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// ContentStore Interface Implementation
// ============================================================================

// WriteContent writes content to the repository and returns the content ID.
//
// This is a helper method for testing and initial setup. It writes the entire
// content in one operation, which may be slow for large files. For large files
// or partial updates, use WriteAt instead.
//
// Context Cancellation:
// This operation checks the context before writing. For very large content
// (>10MB), it performs chunked writes with periodic context checks.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier for the new content
//   - content: Data to write
//
// Returns:
//   - error: Returns error if write fails or context is cancelled
func (r *FSContentStore) WriteContent(ctx context.Context, id metadata.ContentID, content []byte) error {
	// ========================================================================
	// Step 1: Check context before filesystem operation
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return err
	}

	filePath := r.getFilePath(ctx, id)

	// ========================================================================
	// Step 2: Write content with chunking for large files
	// ========================================================================

	// For small files (<10MB), write directly
	if len(content) < 10*1024*1024 {
		if err := os.WriteFile(filePath, content, 0644); err != nil {
			return fmt.Errorf("failed to write content: %w", err)
		}
		return nil
	}

	// For large files, use chunked writes with context checks
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to open file for writing: %w", err)
	}
	defer func() { _ = file.Close() }()

	const chunkSize = 1 * 1024 * 1024 // 1MB chunks
	for offset := 0; offset < len(content); offset += chunkSize {
		// Check context before each chunk
		if err := ctx.Err(); err != nil {
			return err
		}

		end := min(offset+chunkSize, len(content))

		if _, err := file.Write(content[offset:end]); err != nil {
			return fmt.Errorf("failed to write content chunk: %w", err)
		}
	}

	return nil
}

// ============================================================================
// WritableContentStore Interface Implementation
// ============================================================================

// WriteAt writes data at the specified offset.
//
// This implements the WritableContentStore interface for partial file updates.
// The file will be created if it doesn't exist. If the offset is beyond the
// current file size, the gap will be filled with zeros.
//
// Context Cancellation:
// This operation checks the context before opening and seeking, and periodically
// during writes for large data (>1MB).
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//   - data: Data to write
//   - offset: Byte offset where writing should begin
//
// Returns:
//   - error: Returns error if operation fails or context is cancelled
func (r *FSContentStore) WriteAt(ctx context.Context, id metadata.ContentID, data []byte, offset int64) error {
	// ========================================================================
	// Step 1: Check context before filesystem operation
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return err
	}

	filePath := r.getFilePath(ctx, id)

	// ========================================================================
	// Step 2: Per-file locking and FD cache lookup
	// ========================================================================

	r.fdCache.LockFile(id)
	defer r.fdCache.UnlockFile(id)

	file, cacheHit := r.fdCache.Get(id)

	if !cacheHit {
		var err error
		file, err = os.OpenFile(filePath, os.O_RDWR|os.O_CREATE, 0644)
		if err != nil {
			return fmt.Errorf("failed to open file for writing: %w", err)
		}

		if err := r.fdCache.Put(id, file, filePath); err != nil {
			if closeErr := file.Close(); closeErr != nil {
				return fmt.Errorf("failed to cache file descriptor: %w (close error: %v)", err, closeErr)
			}
			return fmt.Errorf("failed to cache file descriptor: %w", err)
		}
	}

	// ========================================================================
	// Step 3: Seek to offset
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return err
	}

	_, err := file.Seek(offset, io.SeekStart)
	if err != nil {
		return fmt.Errorf("failed to seek to offset: %w", err)
	}

	// ========================================================================
	// Step 4: Write data with chunking for large writes
	// ========================================================================

	// For small writes (<1MB), write directly
	if len(data) < 1*1024*1024 {
		if _, err := file.Write(data); err != nil {
			return fmt.Errorf("failed to write data: %w", err)
		}
		return nil
	}

	// For large writes, use chunked writes with context checks
	const chunkSize = 256 * 1024 // 256KB chunks
	for offset := 0; offset < len(data); offset += chunkSize {
		// Check context before each chunk
		if err := ctx.Err(); err != nil {
			return err
		}

		end := min(offset+chunkSize, len(data))

		if _, err := file.Write(data[offset:end]); err != nil {
			return fmt.Errorf("failed to write data chunk: %w", err)
		}
	}

	return nil
}

// Truncate changes the size of the content to the specified size.
//
// This implements the WritableContentStore.Truncate interface method.
//
// Truncate Semantics:
//   - If newSize < currentSize: Content is truncated (trailing data removed)
//   - If newSize > currentSize: Content is extended with zeros
//   - If newSize == currentSize: No-op (succeeds immediately)
//
// Context Cancellation:
// This operation checks the context before performing the truncate operation.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//   - newSize: New size in bytes
//
// Returns:
//   - error: Returns error if truncate fails or context is cancelled
func (r *FSContentStore) Truncate(ctx context.Context, id metadata.ContentID, newSize uint64) error {
	// ========================================================================
	// Step 1: Check context before filesystem operation
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return err
	}

	filePath := r.getFilePath(ctx, id)

	// ========================================================================
	// Step 2: Check if file exists
	// ========================================================================

	_, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("truncate failed for %s: %w", id, content.ErrContentNotFound)
		}
		return fmt.Errorf("failed to stat content for truncate: %w", err)
	}

	// ========================================================================
	// Step 3: Truncate the file
	// ========================================================================

	if err := os.Truncate(filePath, int64(newSize)); err != nil {
		return fmt.Errorf("failed to truncate content: %w", err)
	}

	return nil
}

// Delete removes content from the filesystem.
//
// This implements the WritableContentStore.Delete interface method.
//
// The operation is idempotent - deleting non-existent content returns nil.
// Storage space is reclaimed immediately by the operating system.
//
// Context Cancellation:
// This operation checks the context before performing deletion.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier to delete
//
// Returns:
//   - error: Only returns error for context cancellation or filesystem failures,
//     NOT for non-existent content (returns nil in that case)
func (r *FSContentStore) Delete(ctx context.Context, id metadata.ContentID) error {
	// ========================================================================
	// Step 1: Check context before filesystem operation
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return err
	}

	filePath := r.getFilePath(ctx, id)

	// ========================================================================
	// Step 2: Remove from FD cache if present
	// ========================================================================

	_ = r.fdCache.Remove(id) // Ignore cache removal errors

	// ========================================================================
	// Step 3: Remove the file
	// ========================================================================

	err := os.Remove(filePath)
	if err != nil {
		// Check if file doesn't exist - this is OK (idempotent)
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to delete content: %w", err)
	}

	return nil
}
