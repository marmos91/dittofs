package memory

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Server Configuration
// ============================================================================

// SetServerConfig sets the server-wide configuration.
//
// This stores global server settings that apply across all shares and operations.
// Configuration changes are applied atomically - concurrent operations see either
// the old or new configuration, never a partial update.
func (s *MemoryMetadataStore) SetServerConfig(ctx context.Context, config metadata.MetadataServerConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.serverConfig = config
	return nil
}

// GetServerConfig returns the current server configuration.
//
// This retrieves the global server settings for use by protocol handlers,
// management tools, and monitoring systems.
func (s *MemoryMetadataStore) GetServerConfig(ctx context.Context) (metadata.MetadataServerConfig, error) {
	if err := ctx.Err(); err != nil {
		return metadata.MetadataServerConfig{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.serverConfig, nil
}

// ============================================================================
// Filesystem Capabilities
// ============================================================================

// GetFilesystemCapabilities returns static filesystem capabilities and limits.
//
// This provides information about what the in-memory filesystem supports and
// its limits. The information is relatively static (changes only on configuration
// updates or server restart).
func (store *MemoryMetadataStore) GetFilesystemCapabilities(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemCapabilities, error) {
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
func (store *MemoryMetadataStore) SetFilesystemCapabilities(capabilities metadata.FilesystemCapabilities) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.capabilities = capabilities
}

// ============================================================================
// Filesystem Statistics
// ============================================================================

// GetFilesystemStatistics returns dynamic filesystem statistics.
//
// This provides current information about filesystem usage and availability.
// For the in-memory implementation, statistics are calculated from the current
// state of the files map.
func (store *MemoryMetadataStore) GetFilesystemStatistics(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemStatistics, error) {
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
		TotalBytes:     totalBytes,
		UsedBytes:      usedBytes,
		AvailableBytes: availableBytes,
		TotalFiles:     totalFiles,
		UsedFiles:      usedFiles,
		AvailableFiles: availableFiles,
		ValidFor:       0, // Statistics may change at any time
	}, nil
}

// computeStatistics calculates current filesystem statistics.
// Must be called with at least a read lock held.
func (store *MemoryMetadataStore) computeStatistics() metadata.FilesystemStatistics {
	var totalSize uint64
	fileCount := uint64(len(store.files))

	for _, fd := range store.files {
		totalSize += fd.Attr.Size
	}

	// Report storage limits or defaults
	totalBytes := store.maxStorageBytes
	if totalBytes == 0 {
		totalBytes = 1099511627776 // 1TB default
	}

	maxFiles := store.maxFiles
	if maxFiles == 0 {
		maxFiles = 1000000 // 1 million default
	}

	return metadata.FilesystemStatistics{
		TotalBytes:     totalBytes,
		UsedBytes:      totalSize,
		AvailableBytes: totalBytes - totalSize,
		TotalFiles:     maxFiles,
		UsedFiles:      fileCount,
		AvailableFiles: maxFiles - fileCount,
	}
}
