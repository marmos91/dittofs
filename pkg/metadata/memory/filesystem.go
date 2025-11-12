package memory

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
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
func (s *MemoryMetadataStore) GetFilesystemCapabilities(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemCapabilities, error) {
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

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Verify the handle exists
	key := handleToKey(handle)
	if _, exists := s.files[key]; !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	// Return static capabilities for the in-memory filesystem
	// These values are based on typical NFS/SMB recommendations and
	// standard Unix filesystem limits
	return &metadata.FilesystemCapabilities{
		// Transfer Sizes
		// Maximum read size: 1MB (balance between efficiency and memory)
		MaxReadSize: 1048576,

		// Preferred read size: 64KB (good balance for most protocols)
		PreferredReadSize: 65536,

		// Maximum write size: 1MB (matches read size)
		MaxWriteSize: 1048576,

		// Preferred write size: 64KB (matches preferred read size)
		PreferredWriteSize: 65536,

		// Limits
		// Maximum file size: 2^63-1 (practically unlimited, constrained by memory)
		MaxFileSize: 9223372036854775807,

		// Maximum filename length: 255 bytes (standard Unix limit)
		MaxFilenameLen: 255,

		// Maximum path length: 4096 bytes (standard Unix limit)
		MaxPathLen: 4096,

		// Maximum hard link count: 32767 (reasonable limit, similar to ext4)
		MaxHardLinkCount: 32767,

		// Features
		// Supports hard links (we track link counts)
		SupportsHardLinks: true,

		// Supports symbolic links (we store symlink targets in FileAttr)
		SupportsSymlinks: true,

		// Case-sensitive filenames (Go map keys are case-sensitive)
		CaseSensitive: true,

		// Case-preserving (we store exact filenames as provided)
		CasePreserving: true,

		// ACLs not supported in basic implementation (could be added via Extended attrs)
		SupportsACLs: false,

		// Extended attributes not supported in basic implementation
		SupportsExtendedAttrs: false,

		// Reject long names with error (don't silently truncate)
		TruncatesLongNames: true,

		// Timestamp resolution: 1 nanosecond (Go time.Time has nanosecond precision)
		TimestampResolution: 1,
	}, nil
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
func (s *MemoryMetadataStore) GetFilesystemStatistics(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemStatistics, error) {
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

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Verify the handle exists
	key := handleToKey(handle)
	if _, exists := s.files[key]; !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	// Calculate current usage by summing all file sizes
	var usedBytes uint64
	for _, attr := range s.files {
		// Only count regular files (directories, symlinks have no real content)
		if attr.Type == metadata.FileTypeRegular {
			usedBytes += attr.Size
		}
	}

	// Count total files (including directories)
	usedFiles := uint64(len(s.files))

	// For in-memory implementation, use large default values to indicate "unlimited"
	// In practice, limits are determined by available system memory
	// These could be made configurable via CustomSettings if needed, e.g.:
	//   - s.serverConfig.CustomSettings["memory.max_storage_bytes"]
	//   - s.serverConfig.CustomSettings["memory.max_files"]

	// Default to 1TB total storage (effectively unlimited for in-memory)
	totalBytes := uint64(1024 * 1024 * 1024 * 1024)

	// Default to 1 million files
	totalFiles := uint64(1000000)

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
		// Total space in bytes (default limit)
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
