package memory

import (
	"context"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// GetFilesystemCapabilities returns static filesystem capabilities and limits.
//
// This provides information about what the in-memory filesystem supports and
// its limits. The information is relatively static (changes only on configuration
// updates or server restart).
//
// In-Memory Implementation Capabilities:
//   - Supports all file types (regular, directory, symlink, special files)
//   - Supports hard links
//   - Case-sensitive filenames
//   - Maximum name length: 255 bytes (standard Unix limit)
//   - Maximum file size: Limited by available memory and uint64 size field
//   - Recommended transfer sizes: Based on typical NFS/SMB defaults
//
// The handle parameter is used to verify the file exists, but all files in
// the in-memory store share the same capabilities (there's only one filesystem).
//
// Thread Safety:
// This method acquires a read lock to verify the handle exists.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: A file handle within the filesystem to query
//
// Returns:
//   - *FilesystemCapabilities: Static filesystem capabilities and limits
//   - error: ErrNotFound if handle doesn't exist, ErrInvalidHandle if handle
//     is malformed, or context cancellation error
func (store *MemoryMetadataStore) GetFilesystemCapabilities(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemCapabilities, error) {
	// Check context before acquiring lock
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Validate handle
	if len(handle) == 0 {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "file handle cannot be empty",
		}
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	// Verify the handle exists
	key := handleToKey(handle)
	if _, exists := store.files[key]; !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	// Return the capabilities that were configured at store creation
	// Make a copy to prevent external modifications
	capsCopy := store.capabilities
	return &capsCopy, nil
}

// SetFilesystemCapabilities updates the filesystem capabilities for this store.
//
// This method allows updating the static capabilities after store creation,
// which is useful during initialization when capabilities are loaded from
// global configuration.
//
// This is typically called once during server initialization before the store
// is used by any clients.
//
// Thread Safety:
// This method acquires a write lock to update the capabilities.
//
// Parameters:
//   - capabilities: The new filesystem capabilities to use
func (store *MemoryMetadataStore) SetFilesystemCapabilities(capabilities metadata.FilesystemCapabilities) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.capabilities = capabilities
}

// GetFilesystemStatistics returns dynamic filesystem statistics.
//
// This provides current information about filesystem usage and availability.
// For the in-memory implementation, statistics are calculated from the current
// state of the files map.
//
// Space Calculation:
//   - Total space: Configured limit (or unlimited if not set)
//   - Used space: Sum of all file sizes in the store
//   - Available space: Total - Used
//   - Total files: Count of all entries in files map
//   - Available files: Unlimited (or limit - used if configured)
//
// Note: The in-memory implementation doesn't enforce hard limits by default.
// Space "limits" are informational and can be configured via ServerConfig.
// Actual limits depend on available system memory.
//
// Thread Safety:
// This method acquires a read lock to calculate statistics from current state.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: A file handle within the filesystem to query
//
// Returns:
//   - *FilesystemStatistics: Dynamic filesystem usage statistics
//   - error: ErrNotFound if handle doesn't exist, ErrInvalidHandle if handle
//     is malformed, or context cancellation error
func (store *MemoryMetadataStore) GetFilesystemStatistics(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemStatistics, error) {
	// Check context before acquiring lock
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Validate handle
	if len(handle) == 0 {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "file handle cannot be empty",
		}
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	// Verify the handle exists
	key := handleToKey(handle)
	if _, exists := store.files[key]; !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	// Calculate current usage by summing all file sizes
	var usedBytes uint64
	for _, attr := range store.files {
		// Only count regular files (directories, symlinks have no real content)
		if attr.Attr.Type == metadata.FileTypeRegular {
			usedBytes += attr.Attr.Size
		}
	}

	// Count total files (including directories)
	usedFiles := uint64(len(store.files))

	// Get configured limits from store configuration
	// If not configured (0), use large default values to indicate "unlimited"
	totalBytes := store.maxStorageBytes
	if totalBytes == 0 {
		// Default to 1TB (effectively unlimited for in-memory)
		totalBytes = 1024 * 1024 * 1024 * 1024
	}

	totalFiles := store.maxFiles
	if totalFiles == 0 {
		// Default to 1 million files
		totalFiles = 1000000
	}

	// Calculate available space
	availableBytes := uint64(0)
	if totalBytes > usedBytes {
		availableBytes = totalBytes - usedBytes
	}

	availableFiles := uint64(0)
	if totalFiles > usedFiles {
		availableFiles = totalFiles - usedFiles
	}

	return &metadata.FilesystemStatistics{
		// Total space in bytes (from configuration)
		TotalBytes: totalBytes,

		// Used space in bytes (sum of all file sizes)
		UsedBytes: usedBytes,

		// Available space in bytes (available to users)
		AvailableBytes: availableBytes,

		// Total number of file slots (inodes)
		TotalFiles: totalFiles,

		// Used file slots (current file count)
		UsedFiles: usedFiles,

		// Available file slots (to users)
		AvailableFiles: availableFiles,

		// Statistics are real-time and may change at any moment
		// 0 means statistics may change at any time
		ValidFor: 0,
	}, nil
}
