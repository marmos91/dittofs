package content

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// FSContentStore implements ContentRepository using the local filesystem.
//
// This implementation stores file contents directly on the filesystem using
// content IDs as filenames. It provides basic CRUD operations for file content
// with context cancellation support for all I/O operations.
//
// Thread Safety:
// The underlying filesystem operations are thread-safe at the OS level, but
// concurrent writes to the same file may result in corruption. Callers should
// ensure proper synchronization for concurrent access to the same content ID.
type FSContentStore struct {
	basePath string
	fdCache  *FDCache
}

// NewFSContentStore creates a new filesystem-based content repository.
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
//   - *FSContentStore: Initialized repository
//   - error: Returns error if directory creation fails or context is cancelled
func NewFSContentStore(ctx context.Context, basePath string) (*FSContentStore, error) {
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

	const defaultFDCacheSize = 512

	return &FSContentStore{
		basePath: basePath,
		fdCache:  NewFDCache(defaultFDCacheSize),
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
func (r *FSContentStore) getFilePath(_ context.Context, id metadata.ContentID) string {
	// Hex-encode the content ID to make it filesystem-safe
	// Binary data like SHA-256 hashes contain illegal byte sequences
	return filepath.Join(r.basePath, hex.EncodeToString([]byte(id)))
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
			file.Close()
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

	if err := r.fdCache.Remove(id); err != nil {
		// Cache removal error is non-fatal
	}

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

// GetStorageStats returns statistics about the filesystem storage.
//
// ⚠️  IMPORTANT: This is currently a placeholder implementation that returns
// zeros for all fields. It is NOT suitable for production use cases requiring
// accurate capacity planning or quota enforcement.
//
// Implementation Status:
// This method requires platform-specific system calls (syscall.Statfs on Unix,
// GetDiskFreeSpaceEx on Windows) to retrieve filesystem statistics, and
// directory scanning to count content items. Given DittoFS's experimental
// status, this was deprioritized in favor of core NFS functionality.
//
// To implement this properly:
//  1. Use build tags for platform-specific implementations (fs_unix.go, fs_windows.go)
//  2. Call syscall.Statfs (Unix) or GetDiskFreeSpaceEx (Windows) for disk stats
//  3. Scan r.basePath to count files and calculate total size
//  4. Consider caching results with TTL (expensive operation)
//
// For now, callers should check for zero values and handle gracefully.
// The memory implementation (MemoryContentStore) provides a reference for
// complete stats functionality.
//
// This implements the ContentStore.GetStorageStats interface method.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//
// Returns:
//   - *content.StorageStats: Placeholder statistics (all zeros)
//   - error: Returns error for context cancellation only
func (r *FSContentStore) GetStorageStats(ctx context.Context) (*content.StorageStats, error) {
	// ========================================================================
	// Step 1: Check context before filesystem operation
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ========================================================================
	// Step 2: Get filesystem statistics
	// ========================================================================
	// Note: This is a platform-specific operation. For now, we return
	// placeholder values. A full implementation would use syscall.Statfs
	// on Unix systems or GetDiskFreeSpaceEx on Windows.

	return &content.StorageStats{
		TotalSize:     0, // Would need platform-specific syscall
		UsedSize:      0, // Would need to scan directory
		AvailableSize: 0, // Would need platform-specific syscall
		ContentCount:  0, // Would need to scan directory
		AverageSize:   0, // Would need to scan directory
	}, nil
}

// ListAllContent returns all content IDs stored in the filesystem.
//
// This implements the GarbageCollectableStore.ListAllContent interface method.
//
// This scans the base directory and returns all file names as ContentIDs.
// For large stores with many files, this may be slow and consume memory.
//
// Context Cancellation:
// This operation checks context periodically during directory scanning.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//
// Returns:
//   - []metadata.ContentID: List of all content IDs
//   - error: Returns error for context cancellation or filesystem failures
func (r *FSContentStore) ListAllContent(ctx context.Context) ([]metadata.ContentID, error) {
	// ========================================================================
	// Step 1: Check context before filesystem operation
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ========================================================================
	// Step 2: Read directory entries
	// ========================================================================

	entries, err := os.ReadDir(r.basePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read content directory: %w", err)
	}

	// ========================================================================
	// Step 3: Build list of content IDs
	// ========================================================================

	contentIDs := make([]metadata.ContentID, 0, len(entries))

	for _, entry := range entries {
		// Check context periodically (every 100 entries)
		if len(contentIDs)%100 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}

		// Skip directories, only include regular files
		if !entry.IsDir() {
			contentIDs = append(contentIDs, metadata.ContentID(entry.Name()))
		}
	}

	return contentIDs, nil
}

// DeleteBatch removes multiple content items in one operation.
//
// This implements the GarbageCollectableStore.DeleteBatch interface method.
//
// For filesystem storage, this performs deletions sequentially. The operation
// is best-effort - partial failures are allowed and returned in the map.
//
// Context Cancellation:
// This operation checks context periodically during batch deletion.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - ids: Content identifiers to delete
//
// Returns:
//   - map[metadata.ContentID]error: Map of failed deletions (empty = all succeeded)
//   - error: Only returns error for context cancellation
func (r *FSContentStore) DeleteBatch(ctx context.Context, ids []metadata.ContentID) (map[metadata.ContentID]error, error) {
	failures := make(map[metadata.ContentID]error)

	for i, id := range ids {
		// Check context periodically (every 10 deletions)
		if i%10 == 0 {
			if err := ctx.Err(); err != nil {
				// Context cancelled - mark remaining as failed
				for j := i; j < len(ids); j++ {
					failures[ids[j]] = ctx.Err()
				}
				return failures, ctx.Err()
			}
		}

		// Attempt to delete
		if err := r.Delete(ctx, id); err != nil {
			failures[id] = err
		}
	}

	return failures, nil
}

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

// Close closes all cached file descriptors and cleans up resources
func (r *FSContentStore) Close() error {
	return r.fdCache.Close()
}
