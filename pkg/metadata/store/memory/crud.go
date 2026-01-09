package memory

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// CRUD Operations
// ============================================================================
//
// These methods provide low-level data operations for metadata storage.
// They are thin wrappers around the internal maps with NO business logic.
// Business logic is handled by shared functions in the metadata package.

// GetEntry retrieves file metadata by handle.
// Returns ErrNotFound if handle doesn't exist.
func (store *MemoryMetadataStore) GetEntry(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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

// PutEntry stores or updates file metadata.
// Creates the entry if it doesn't exist.
func (store *MemoryMetadataStore) PutEntry(ctx context.Context, file *metadata.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	// Build handle from file info
	handle, err := metadata.EncodeShareHandle(file.ShareName, file.ID)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "failed to encode file handle",
		}
	}

	key := handleToKey(handle)

	// Copy attributes to avoid external mutation
	attrCopy := file.FileAttr

	store.files[key] = &fileData{
		Attr:      &attrCopy,
		ShareName: file.ShareName,
	}

	// Initialize link count if not set
	if _, exists := store.linkCounts[key]; !exists {
		if file.Type == metadata.FileTypeDirectory {
			store.linkCounts[key] = 2 // . and parent entry
		} else {
			store.linkCounts[key] = 1
		}
	}

	return nil
}

// DeleteEntry removes file metadata by handle.
// Returns ErrNotFound if handle doesn't exist.
func (store *MemoryMetadataStore) DeleteEntry(ctx context.Context, handle metadata.FileHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	key := handleToKey(handle)
	if _, exists := store.files[key]; !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	// Remove from all maps
	delete(store.files, key)
	delete(store.parents, key)
	delete(store.children, key)
	delete(store.linkCounts, key)
	delete(store.deviceNumbers, key)
	delete(store.sortedDirCache, key)

	return nil
}

// GetChild resolves a name in a directory to a file handle.
// Returns ErrNotFound if name doesn't exist.
func (store *MemoryMetadataStore) GetChild(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	dirKey := handleToKey(dirHandle)

	// Initialize children map if needed
	if store.children[dirKey] == nil {
		store.children[dirKey] = make(map[string]metadata.FileHandle)
	}

	store.children[dirKey][name] = childHandle

	// Invalidate sorted cache
	store.invalidateDirCache(dirHandle)

	return nil
}

// DeleteChild removes a child entry from a directory.
// Returns ErrNotFound if name doesn't exist.
func (store *MemoryMetadataStore) DeleteChild(ctx context.Context, dirHandle metadata.FileHandle, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	dirKey := handleToKey(dirHandle)
	childrenMap, exists := store.children[dirKey]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "child not found",
		}
	}

	if _, exists := childrenMap[name]; !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "child not found",
		}
	}

	delete(childrenMap, name)

	// Invalidate sorted cache
	store.invalidateDirCache(dirHandle)

	return nil
}

// GetParent returns the parent handle for a file/directory.
// Returns ErrNotFound for root directories (no parent).
func (store *MemoryMetadataStore) GetParent(ctx context.Context, handle metadata.FileHandle) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	key := handleToKey(handle)
	store.parents[key] = parentHandle

	return nil
}

// GetLinkCount returns the hard link count for a file.
// Returns 0 if the file doesn't track link counts or doesn't exist.
func (store *MemoryMetadataStore) GetLinkCount(ctx context.Context, handle metadata.FileHandle) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

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
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	key := handleToKey(handle)
	store.linkCounts[key] = count

	return nil
}

// GenerateHandle creates a new unique file handle for a path in a share.
func (store *MemoryMetadataStore) GenerateHandle(ctx context.Context, shareName string, path string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Memory store uses UUID-based handles, path is for compatibility
	return store.generateFileHandle(shareName, path), nil
}

// GetRootHandle returns the root handle for a share.
// Returns ErrNotFound if the share doesn't exist.
func (store *MemoryMetadataStore) GetRootHandle(ctx context.Context, shareName string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	shareData, exists := store.shares[shareName]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    shareName,
		}
	}

	return shareData.RootHandle, nil
}

// GetShareOptions returns the share configuration options.
// Returns ErrNotFound if the share doesn't exist.
func (store *MemoryMetadataStore) GetShareOptions(ctx context.Context, shareName string) (*metadata.ShareOptions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	shareData, exists := store.shares[shareName]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    shareName,
		}
	}

	// Return a copy to avoid external mutation
	optsCopy := shareData.Share.Options
	return &optsCopy, nil
}
