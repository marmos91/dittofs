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

	stats := store.computeStatistics()
	return &stats, nil
}

// computeStatistics calculates current filesystem statistics.
// Must be called with at least a read lock held.
func (store *MemoryMetadataStore) computeStatistics() metadata.FilesystemStatistics {
	// Read usage from atomic counter (O(1), no scan needed).
	totalSize := uint64(store.usedBytes.Load())
	fileCount := uint64(len(store.files))

	// Report storage limits or defaults
	totalBytes := store.maxStorageBytes
	if totalBytes == 0 {
		totalBytes = 1 << 50 // 1 PiB (unlimited sentinel)
	}

	maxFiles := store.maxFiles
	if maxFiles == 0 {
		maxFiles = 1000000 // 1 million default
	}

	availableBytes := uint64(0)
	if totalBytes > totalSize {
		availableBytes = totalBytes - totalSize
	}

	availableFiles := uint64(0)
	if maxFiles > fileCount {
		availableFiles = maxFiles - fileCount
	}

	return metadata.FilesystemStatistics{
		TotalBytes:     totalBytes,
		UsedBytes:      totalSize,
		AvailableBytes: availableBytes,
		TotalFiles:     maxFiles,
		UsedFiles:      fileCount,
		AvailableFiles: availableFiles,
	}
}
