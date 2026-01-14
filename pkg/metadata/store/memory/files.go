package memory

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// CRUD Operations
// ============================================================================
//
// These methods delegate to transaction methods via WithTransaction.
// This ensures consistency and avoids duplicating implementation logic.

// GetFile retrieves file metadata by handle.
// Returns ErrNotFound if handle doesn't exist.
func (store *MemoryMetadataStore) GetFile(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Use read lock for this read-only operation
	store.mu.RLock()
	defer store.mu.RUnlock()

	key := handleToKey(handle)
	fileData, exists := store.files[key]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	return store.buildFileWithNlink(handle, fileData)
}

// PutFile stores or updates file metadata.
// Creates the entry if it doesn't exist.
func (store *MemoryMetadataStore) PutFile(ctx context.Context, file *metadata.File) error {
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutFile(ctx, file)
	})
}

// DeleteFile removes file metadata by handle.
// Returns ErrNotFound if handle doesn't exist.
func (store *MemoryMetadataStore) DeleteFile(ctx context.Context, handle metadata.FileHandle) error {
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteFile(ctx, handle)
	})
}

// GetChild resolves a name in a directory to a file handle.
// Returns ErrNotFound if name doesn't exist.
func (store *MemoryMetadataStore) GetChild(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Use read lock for this read-only operation
	store.mu.RLock()
	defer store.mu.RUnlock()

	dirKey := handleToKey(dirHandle)
	childrenMap, exists := store.children[dirKey]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "child not found",
		}
	}

	childHandle, exists := childrenMap[name]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "child not found",
		}
	}

	return childHandle, nil
}

// SetChild adds or updates a child entry in a directory.
func (store *MemoryMetadataStore) SetChild(ctx context.Context, dirHandle metadata.FileHandle, name string, childHandle metadata.FileHandle) error {
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetChild(ctx, dirHandle, name, childHandle)
	})
}

// DeleteChild removes a child entry from a directory.
// Returns ErrNotFound if name doesn't exist.
func (store *MemoryMetadataStore) DeleteChild(ctx context.Context, dirHandle metadata.FileHandle, name string) error {
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteChild(ctx, dirHandle, name)
	})
}

// GetParent returns the parent handle for a file/directory.
// Returns ErrNotFound for root directories (no parent).
func (store *MemoryMetadataStore) GetParent(ctx context.Context, handle metadata.FileHandle) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Use read lock for this read-only operation
	store.mu.RLock()
	defer store.mu.RUnlock()

	key := handleToKey(handle)
	parentHandle, exists := store.parents[key]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "parent not found",
		}
	}

	return parentHandle, nil
}

// SetParent sets the parent handle for a file/directory.
func (store *MemoryMetadataStore) SetParent(ctx context.Context, handle metadata.FileHandle, parentHandle metadata.FileHandle) error {
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetParent(ctx, handle, parentHandle)
	})
}

// GetLinkCount returns the hard link count for a file.
// Returns 0 if the file doesn't track link counts or doesn't exist.
func (store *MemoryMetadataStore) GetLinkCount(ctx context.Context, handle metadata.FileHandle) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	// Use read lock for this read-only operation
	store.mu.RLock()
	defer store.mu.RUnlock()

	key := handleToKey(handle)
	count, exists := store.linkCounts[key]
	if !exists {
		return 0, nil
	}

	return count, nil
}

// SetLinkCount sets the hard link count for a file.
func (store *MemoryMetadataStore) SetLinkCount(ctx context.Context, handle metadata.FileHandle, count uint32) error {
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetLinkCount(ctx, handle, count)
	})
}

// ListChildren returns directory entries with pagination support.
// This is a read-only operation and uses a read lock for better concurrency.
func (store *MemoryMetadataStore) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int) ([]metadata.DirEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	// Use read lock for better concurrency (this is a read-only operation)
	store.mu.RLock()
	defer store.mu.RUnlock()

	dirKey := handleToKey(dirHandle)
	childrenMap, exists := store.children[dirKey]
	if !exists {
		// Empty directory
		return []metadata.DirEntry{}, "", nil
	}

	// Get sorted entries (with caching)
	sortedNames := store.getSortedDirEntriesWithCache(dirHandle, childrenMap)

	// Find start position based on cursor
	startIdx := 0
	if cursor != "" {
		for i, name := range sortedNames {
			if name == cursor {
				startIdx = i + 1
				break
			}
		}
	}

	if limit <= 0 {
		limit = 1000 // Default limit
	}

	// Collect entries
	var entries []metadata.DirEntry
	for i := startIdx; i < len(sortedNames) && len(entries) < limit; i++ {
		name := sortedNames[i]
		childHandle := childrenMap[name]

		entry := metadata.DirEntry{
			ID:     metadata.HandleToINode(childHandle),
			Name:   name,
			Handle: childHandle,
		}

		// Try to get attributes
		childKey := handleToKey(childHandle)
		if fileData, exists := store.files[childKey]; exists {
			entry.Attr = fileData.Attr
		}

		entries = append(entries, entry)
	}

	// Determine next cursor
	nextCursor := ""
	if startIdx+len(entries) < len(sortedNames) {
		nextCursor = entries[len(entries)-1].Name
	}

	return entries, nextCursor, nil
}

// GetFilesystemMeta retrieves filesystem metadata for a share.
func (store *MemoryMetadataStore) GetFilesystemMeta(ctx context.Context, shareName string) (*metadata.FilesystemMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Use read lock for this read-only operation
	store.mu.RLock()
	defer store.mu.RUnlock()

	// For memory store, return capabilities and computed statistics
	return &metadata.FilesystemMeta{
		Capabilities: store.capabilities,
		Statistics:   store.computeStatistics(),
	}, nil
}

// PutFilesystemMeta stores filesystem metadata for a share.
func (store *MemoryMetadataStore) PutFilesystemMeta(ctx context.Context, shareName string, meta *metadata.FilesystemMeta) error {
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutFilesystemMeta(ctx, shareName, meta)
	})
}

// GetFileByPayloadID retrieves file metadata by its content identifier.
//
// This scans all files to find one matching the given PayloadID.
// Note: This is O(n) and may be slow for large filesystems.
func (store *MemoryMetadataStore) GetFileByPayloadID(
	ctx context.Context,
	payloadID metadata.PayloadID,
) (*metadata.File, error) {
	// Check context before acquiring lock
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	// Scan all files for matching PayloadID
	for _, fileData := range store.files {
		if fileData.Attr.PayloadID == payloadID {
			// Return File with just the attributes we need
			// ID and Path aren't needed by the flusher (only Size is used)
			return &metadata.File{
				ShareName: fileData.ShareName,
				FileAttr:  *fileData.Attr,
			}, nil
		}
	}

	return nil, &metadata.StoreError{
		Code:    metadata.ErrNotFound,
		Message: fmt.Sprintf("no file found with content ID: %s", payloadID),
	}
}
