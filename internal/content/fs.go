package content

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// FSContentRepository implements ContentRepository using the local filesystem.
//
// This implementation stores file contents directly on the filesystem using
// content IDs as filenames. It provides basic CRUD operations for file content
// with context cancellation support for all I/O operations.
//
// Thread Safety:
// The underlying filesystem operations are thread-safe at the OS level, but
// concurrent writes to the same file may result in corruption. Callers should
// ensure proper synchronization for concurrent access to the same content ID.
type FSContentRepository struct {
	basePath string
}

// NewFSContentRepository creates a new filesystem-based content repository.
//
// This initializes the repository by creating the base directory if it doesn't
// exist. The base directory will be created with permissions 0755.
//
// Context Cancellation:
// This operation checks the context before creating the directory structure.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - basePath: Root directory for storing content files
//
// Returns:
//   - *FSContentRepository: Initialized repository
//   - error: Returns error if directory creation fails or context is cancelled
func NewFSContentRepository(ctx context.Context, basePath string) (*FSContentRepository, error) {
	// ========================================================================
	// Step 1: Check context before filesystem operation
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ========================================================================
	// Step 2: Create the base directory if it doesn't exist
	// ========================================================================

	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base directory: %w", err)
	}

	return &FSContentRepository{
		basePath: basePath,
	}, nil
}

// getFilePath returns the full path for a given content ID.
//
// This is a lightweight helper function that performs no I/O and does not
// need context cancellation checks.
//
// Parameters:
//   - ctx: Context (unused but kept for interface consistency)
//   - id: Content identifier
//
// Returns:
//   - string: Full filesystem path for the content
func (r *FSContentRepository) getFilePath(_ context.Context, id ContentID) string {
	return filepath.Join(r.basePath, string(id))
}

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
func (r *FSContentRepository) ReadContent(ctx context.Context, id ContentID) (io.ReadCloser, error) {
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
			return nil, fmt.Errorf("content not found: %s", id)
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
func (r *FSContentRepository) GetContentSize(ctx context.Context, id ContentID) (uint64, error) {
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
			return 0, fmt.Errorf("content not found: %s", id)
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
func (r *FSContentRepository) ContentExists(ctx context.Context, id ContentID) (bool, error) {
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
func (r *FSContentRepository) WriteContent(ctx context.Context, id ContentID, content []byte) error {
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
	defer file.Close()

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

// WriteAt writes data at the specified offset.
//
// This implements the WriteRepository interface for partial file updates.
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
func (r *FSContentRepository) WriteAt(ctx context.Context, id ContentID, data []byte, offset int64) error {
	// ========================================================================
	// Step 1: Check context before filesystem operation
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return err
	}

	filePath := r.getFilePath(ctx, id)

	// ========================================================================
	// Step 2: Open file for read/write, create if doesn't exist
	// ========================================================================

	file, err := os.OpenFile(filePath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return fmt.Errorf("failed to open file for writing: %w", err)
	}
	defer file.Close()

	// ========================================================================
	// Step 3: Seek to offset
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return err
	}

	_, err = file.Seek(offset, io.SeekStart)
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
