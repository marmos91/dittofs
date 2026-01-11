// Package fs implements filesystem-based content storage for DittoFS.
//
// This file contains read operations for the filesystem content store, including
// full content reads, seekable reads, size queries, and existence checks.
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

// ReadContent returns a reader for the content identified by the given ID.
//
// The caller is responsible for closing the returned ReadCloser when done.
// The returned reader does not have built-in context cancellation - callers
// should implement timeouts using context deadlines or manual checks.
//
// Context Cancellation:
// This operation checks the context before opening the file. Once the file
// is opened, the caller should monitor the context and close the reader if
// the context is cancelled.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier to read
//
// Returns:
//   - io.ReadCloser: Reader for the content (must be closed by caller)
//   - error: Returns error if content not found, open fails, or context is cancelled
func (r *FSContentStore) ReadContent(ctx context.Context, id metadata.ContentID) (io.ReadCloser, error) {
	// ========================================================================
	// Step 1: Check context before filesystem operation
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ========================================================================
	// Step 2: Open the content file
	// ========================================================================

	filePath := r.getFilePath(ctx, id)
	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("content %s: %w", id, content.ErrContentNotFound)
		}
		return nil, fmt.Errorf("failed to open content: %w", err)
	}

	return file, nil
}

// GetContentSize returns the size of the content in bytes.
//
// This performs a filesystem stat operation to retrieve the file size without
// reading the entire file content.
//
// Context Cancellation:
// This operation checks the context before performing the stat operation.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//
// Returns:
//   - uint64: Size of the content in bytes
//   - error: Returns error if content not found, stat fails, or context is cancelled
func (r *FSContentStore) GetContentSize(ctx context.Context, id metadata.ContentID) (uint64, error) {
	// ========================================================================
	// Step 1: Check context before filesystem operation
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return 0, err
	}

	// ========================================================================
	// Step 2: Stat the content file
	// ========================================================================

	filePath := r.getFilePath(ctx, id)
	info, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("content %s: %w", id, content.ErrContentNotFound)
		}
		return 0, fmt.Errorf("failed to stat content: %w", err)
	}

	return uint64(info.Size()), nil
}

// ContentExists checks if content with the given ID exists.
//
// This is a lightweight existence check that only performs a stat operation
// without reading file content.
//
// Context Cancellation:
// This operation checks the context before performing the stat operation.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier to check
//
// Returns:
//   - bool: True if content exists, false otherwise
//   - error: Returns error on filesystem errors (excluding not-exists) or
//     context cancellation
func (r *FSContentStore) ContentExists(ctx context.Context, id metadata.ContentID) (bool, error) {
	// ========================================================================
	// Step 1: Check context before filesystem operation
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return false, err
	}

	// ========================================================================
	// Step 2: Check file existence
	// ========================================================================

	filePath := r.getFilePath(ctx, id)
	_, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check content existence: %w", err)
	}

	return true, nil
}

// ============================================================================
// SeekableContentStore Interface Implementation
// ============================================================================

// ReadContentSeekable returns a seekable reader for the content.
//
// This implements the SeekableContentStore.ReadContentSeekable interface method.
//
// The filesystem implementation supports efficient seeking, so we return
// the same *os.File handle which implements io.ReadSeekCloser.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier to read
//
// Returns:
//   - io.ReadSeekCloser: Seekable reader (must be closed by caller)
//   - error: Returns error if content not found or context is cancelled
func (r *FSContentStore) ReadContentSeekable(ctx context.Context, id metadata.ContentID) (io.ReadSeekCloser, error) {
	// ========================================================================
	// Step 1: Check context before filesystem operation
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ========================================================================
	// Step 2: Open the content file
	// ========================================================================

	filePath := r.getFilePath(ctx, id)
	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("content %s: %w", id, content.ErrContentNotFound)
		}
		return nil, fmt.Errorf("failed to open content: %w", err)
	}

	return file, nil
}
