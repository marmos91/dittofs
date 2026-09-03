package memory

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
)

// ============================================================================
// Handle/Share Operations
// ============================================================================

// GenerateHandle creates a new unique file handle for a path in a share.
func (store *MemoryMetadataStore) GenerateHandle(ctx context.Context, shareName string, path string) (metadata.FileHandle, error) {
	return basestore.GenerateHandle(ctx, shareName)
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

	// Return a copy to avoid external mutation.
	optsCopy := shareData.Share.Options
	return &optsCopy, nil
}

// ============================================================================
// Share Lifecycle Operations
// ============================================================================

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
	// Delegate to the transaction path: tx.DeleteShare handles objectIndex
	// cleanup and pendingDelta accounting, and WithTransaction commits the
	// delta to usedBytes atomically on success (or restores the snapshot on
	// failure). The store-level body previously skipped both, leaking
	// usedBytes and stale objectIndex entries.
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteShare(ctx, shareName)
	})
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

	store.mu.Lock()
	defer store.mu.Unlock()

	rootHandle, err := store.rootHandleLocked(shareName)
	if err != nil {
		return nil, err
	}
	key := handleToKey(rootHandle)

	// Register the share so GetRootHandle and GetShareOptions resolve it by
	// name; this is the only entry point that records one. Idempotent: an
	// existing entry keeps the options already set on it.
	if _, ok := store.shares[shareName]; !ok {
		store.shares[shareName] = &shareData{
			Share:      metadata.Share{Name: shareName},
			RootHandle: rootHandle,
		}
	}

	// An existing root is reconciled against the configured attributes rather
	// than returned as it stands (see reconcileRootAttrs).
	if existingData, exists := store.files[key]; exists {
		_, id, err := metadata.DecodeFileHandle(rootHandle)
		if err != nil {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrIOError,
				Message: "failed to decode root handle",
			}
		}
		reconciled := reconcileRootAttrs(existingData.Attr, attr)
		store.files[key] = &fileData{Attr: reconciled, ShareName: existingData.ShareName}

		return &metadata.File{
			ID:        id,
			ShareName: shareName,
			Path:      "/",
			FileAttr:  *reconciled,
		}, nil
	}

	// Root doesn't exist, create it
	// Complete root directory attributes with defaults
	rootAttrCopy := *attr
	if rootAttrCopy.Mode == 0 {
		rootAttrCopy.Mode = metadata.DefaultRootMode
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
//
// For the memory store there are no OS resources to release, but Close is the
// graceful-shutdown site for the lock subsystem: it records the clean-shutdown
// marker so the lock-recovery boot path treats this drain as clean. The marker
// is in-process only (memory is non-durable across restarts), which is correct
// — a new process always starts with a fresh, unclean-by-default store.
func (store *MemoryMetadataStore) Close() error {
	store.mu.Lock()
	store.initLockStore()
	store.lockStore.cleanShutdown = true
	store.mu.Unlock()
	return nil
}

// reconcileRootAttrs brings a stored root directory in line with the attributes
// a share was configured with, so re-attaching a share with changed root
// ownership or mode takes effect rather than silently keeping the old values.
// The configuration is the intent; the stored root records a previous run's.
//
// Both entry points call this, because a backend that reconciles on one and not
// the other makes whether an operator's change lands depend on which call site
// reached it.
func reconcileRootAttrs(stored *metadata.FileAttr, configured *metadata.FileAttr) *metadata.FileAttr {
	mode := configured.Mode
	if mode == 0 {
		mode = metadata.DefaultRootMode
	}
	if stored.Mode == mode && stored.UID == configured.UID && stored.GID == configured.GID {
		return stored
	}

	// A copy rather than an edit in place: a transaction's rollback snapshot
	// clones the file map only one level deep, so the *FileAttr behind an entry
	// is shared with the snapshot and an edit through it would survive the
	// rollback it is supposed to be undone by. Callers replace the entry.
	updated := *stored
	updated.Mode = mode
	updated.UID = configured.UID
	updated.GID = configured.GID
	updated.Ctime = time.Now()
	return &updated
}
