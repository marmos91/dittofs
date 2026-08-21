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
// Allows updating the static capabilities after store creation,
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

	stats := store.computeStatistics(shareNameOf(handle))
	return &stats, nil
}

// shareNameOf decodes the share a handle belongs to. An undecodable handle
// yields the empty share, whose usage buckets are empty — the same answer a
// share with no files gives.
func shareNameOf(handle metadata.FileHandle) string {
	shareName, _, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return ""
	}
	return shareName
}

// computeStatistics calculates current filesystem statistics for one share.
// The store instance is shared by every share naming the same config, so the
// usage figures are scoped to shareName; the capacity ceilings are store-wide.
//
// Only regular files contribute, matching the SQL backends' scoped aggregate:
// directories carry no logical bytes and the share root would otherwise inflate
// UsedFiles.
//
// Must be called with at least a read lock held.
func (store *MemoryMetadataStore) computeStatistics(shareName string) metadata.FilesystemStatistics {
	// O(1) read of the per-share bucket, no scan needed.
	store.quotaMu.Lock()
	usage := store.quota.Share(shareName)
	store.quotaMu.Unlock()
	totalSize := uint64(max(usage.Bytes, 0))
	fileCount := uint64(max(usage.Files, 0))

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
