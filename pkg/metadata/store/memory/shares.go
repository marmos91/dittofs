package memory

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Handle/Share Operations
// ============================================================================

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

// ============================================================================
// Share Lifecycle Operations
// ============================================================================

// CreateShare creates a new share with the given configuration.
func (store *MemoryMetadataStore) CreateShare(ctx context.Context, share *metadata.Share) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if _, exists := store.shares[share.Name]; exists {
		return &metadata.StoreError{
			Code:    metadata.ErrAlreadyExists,
			Message: "share already exists",
			Path:    share.Name,
		}
	}

	// Generate root handle
	rootHandle := store.generateFileHandle(share.Name, "/")

	store.shares[share.Name] = &shareData{
		Share:      *share,
		RootHandle: rootHandle,
	}

	return nil
}

// UpdateShareOptions updates the share configuration options.
func (store *MemoryMetadataStore) UpdateShareOptions(ctx context.Context, shareName string, options *metadata.ShareOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	shareData, exists := store.shares[shareName]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    shareName,
		}
	}

	shareData.Share.Options = *options
	return nil
}

// DeleteShare removes a share and all its metadata.
func (store *MemoryMetadataStore) DeleteShare(ctx context.Context, shareName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if _, exists := store.shares[shareName]; !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    shareName,
		}
	}

	// Remove all files belonging to this share
	for key, fd := range store.files {
		if fd.ShareName == shareName {
			delete(store.files, key)
			delete(store.parents, key)
			delete(store.children, key)
			delete(store.linkCounts, key)
			delete(store.deviceNumbers, key)
			delete(store.sortedDirCache, key)
		}
	}

	delete(store.shares, shareName)
	return nil
}

// ListShares returns the names of all shares.
func (store *MemoryMetadataStore) ListShares(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	names := make([]string, 0, len(store.shares))
	for name := range store.shares {
		names = append(names, name)
	}

	return names, nil
}

// ============================================================================
// Root Directory Operations
// ============================================================================

// CreateRootDirectory creates a root directory for a share without a parent.
//
// This is a special operation used during share initialization. The root directory
// is created with a handle in the format "shareName:/" and has no parent.
//
// Parameters:
//   - ctx: Context for cancellation
//   - shareName: Name of the share (used to generate root handle)
//   - attr: Directory attributes (Type must be FileTypeDirectory)
//
// Returns:
//   - *File: Complete file information for the newly created root directory
//   - error: ErrAlreadyExists if root exists, ErrInvalidArgument if not a directory
func (store *MemoryMetadataStore) CreateRootDirectory(
	ctx context.Context,
	shareName string,
	attr *metadata.FileAttr,
) (*metadata.File, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Validate attributes
	if attr.Type != metadata.FileTypeDirectory {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "root must be a directory",
			Path:    shareName,
		}
	}

	// Generate deterministic handle for root directory based on share name
	rootHandle := store.generateFileHandle(shareName, "/")
	key := handleToKey(rootHandle)

	store.mu.Lock()
	defer store.mu.Unlock()

	// Check if root already exists - if so, just return success (idempotent)
	if existingData, exists := store.files[key]; exists {
		// Root already exists, this is OK (idempotent operation)
		// Decode handle to get ID
		_, id, err := metadata.DecodeFileHandle(rootHandle)
		if err != nil {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrIOError,
				Message: "failed to decode root handle",
			}
		}
		return &metadata.File{
			ID:        id,
			ShareName: shareName,
			Path:      "/",
			FileAttr:  *existingData.Attr,
		}, nil
	}

	// Root doesn't exist, create it
	// Complete root directory attributes with defaults
	rootAttrCopy := *attr
	if rootAttrCopy.Mode == 0 {
		rootAttrCopy.Mode = 0777
	}
	now := time.Now()
	if rootAttrCopy.Atime.IsZero() {
		rootAttrCopy.Atime = now
	}
	if rootAttrCopy.Mtime.IsZero() {
		rootAttrCopy.Mtime = now
	}
	if rootAttrCopy.Ctime.IsZero() {
		rootAttrCopy.Ctime = now
	}
	if rootAttrCopy.CreationTime.IsZero() {
		rootAttrCopy.CreationTime = now
	}

	// Create and store fileData for root directory
	store.files[key] = &fileData{
		Attr:      &rootAttrCopy,
		ShareName: shareName,
	}

	// Initialize children map for root directory (empty initially)
	store.children[key] = make(map[string]metadata.FileHandle)

	// Set link count to 2:
	// - 1 for "." (self-reference)
	// - 1 for the share's reference to this root
	store.linkCounts[key] = 2
	rootAttrCopy.Nlink = 2

	// Root directories have no parent (they are top-level)
	// So we don't add an entry to store.parents

	// Decode handle to get ID
	_, id, err := metadata.DecodeFileHandle(rootHandle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: "failed to decode root handle",
		}
	}

	// Return full File information
	return &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      "/",
		FileAttr:  rootAttrCopy,
	}, nil
}

// Close releases any resources held by the store.
// For memory store, this is a no-op.
func (store *MemoryMetadataStore) Close() error {
	return nil
}
