package memory

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// AddShare makes a filesystem path available to clients with specific access rules.
//
// This method creates a new share with a root directory. The share serves as the
// entry point for client access, with its own access control rules and configuration.
//
// Implementation Details:
//   - Creates a root directory with the provided attributes
//   - Initializes the root's link count to 2 ("." self-reference and share reference)
//   - Initializes an empty children map for the root directory
//   - Root directories have no parent (they are top-level entries)
//   - Root directories have no content (ContentID is empty)
//
// Attribute Completion:
// If rootAttr fields are zero/empty, they are filled with defaults:
//   - Type: Must be FileTypeDirectory (enforced)
//   - Mode: Defaults to 0755 if zero
//   - UID/GID: Kept as provided (0 is valid for root user)
//   - Size: Implementation-specific for directories
//   - Timestamps: Set to current time
//   - ContentID: Empty (directories don't have content)
//   - LinkTarget: Empty (not a symlink)
//
// Parameters:
//   - ctx: Context for cancellation
//   - name: Unique identifier for the share
//   - options: Access control and authentication settings
//   - rootAttr: Initial attributes for the share's root directory
//
// Returns:
//   - error: ErrAlreadyExists if share exists, ErrInvalidArgument if rootAttr.Type
//     is not Directory, or context cancellation error
//
// AddShare makes a filesystem path available to clients with specific access rules.
func (store *MemoryMetadataStore) AddShare(
	ctx context.Context,
	name string,
	options metadata.ShareOptions,
	rootAttr *metadata.FileAttr,
) error {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	// Check if share already exists
	if _, exists := store.shares[name]; exists {
		return &metadata.StoreError{
			Code:    metadata.ErrAlreadyExists,
			Message: "share already exists",
			Path:    name,
		}
	}

	// Validate root attributes
	if rootAttr.Type != metadata.FileTypeDirectory {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "share root must be a directory",
			Path:    name,
		}
	}

	// Complete root directory attributes with defaults
	if rootAttr.Mode == 0 {
		rootAttr.Mode = 0755
	}

	// Generate unique handle for root directory
	rootHandle := store.generateFileHandle()

	// Initialize root directory metadata
	// Note: We store a copy to avoid external modifications
	rootAttrCopy := *rootAttr
	key := handleToKey(rootHandle)

	// Create and store fileData for root directory
	store.files[key] = &fileData{
		Attr:      &rootAttrCopy,
		ShareName: name, // Set the share name for the root
	}

	// Initialize children map for root directory (empty initially)
	store.children[key] = make(map[string]metadata.FileHandle)

	// Set link count to 2:
	// - 1 for "." (self-reference)
	// - 1 for the share's reference to this root
	store.linkCounts[key] = 2

	// Root directories have no parent (they are top-level)
	// So we don't add an entry to store.parents

	// Store share configuration
	store.shares[name] = &shareData{
		Share: metadata.Share{
			Name:    name,
			Options: options,
		},
		RootHandle: rootHandle,
	}

	return nil
}

// GetShares returns all configured shares.
//
// This retrieves the complete list of shares available on the server,
// regardless of access restrictions or mount status. The shares are
// returned in arbitrary order (map iteration order is undefined in Go).
//
// Implementation Details:
//   - Returns copies of share configurations to prevent external modifications
//   - Empty slice is returned if no shares are configured
//   - Does not check access permissions (that's done by CheckShareAccess)
//
// Returns:
//   - []Share: List of all share configurations (may be empty)
//   - error: Only context cancellation errors
func (store *MemoryMetadataStore) GetShares(
	ctx context.Context,
) ([]metadata.Share, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	// Pre-allocate slice with exact capacity
	shares := make([]metadata.Share, 0, len(store.shares))

	// Copy all share configurations
	for _, sd := range store.shares {
		shares = append(shares, sd.Share)
	}

	return shares, nil
}

// FindShare retrieves a share configuration by name.
//
// This looks up a specific share and returns its configuration. The returned
// share is a copy to prevent external modifications.
//
// Implementation Details:
//   - Case-sensitive name lookup
//   - Returns copy of share configuration
//   - Does not check access permissions (that's done by CheckShareAccess)
//
// Parameters:
//   - ctx: Context for cancellation
//   - name: The share name to look up
//
// Returns:
//   - *Share: The share configuration (copy)
//   - error: ErrNotFound if share doesn't exist, or context cancellation error
func (store *MemoryMetadataStore) FindShare(
	ctx context.Context,
	name string,
) (*metadata.Share, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	// Look up share
	sd, exists := store.shares[name]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    name,
		}
	}

	// Return a copy to prevent external modifications
	shareCopy := sd.Share
	return &shareCopy, nil
}

// DeleteShare removes a share configuration.
//
// WARNING: This only removes the share configuration from the shares map.
// It does NOT:
//   - Remove the root directory or its files from the files map
//   - Delete any file content from the content repository
//   - Disconnect active client sessions
//   - Clean up session tracking records
//
// Rationale for Not Deleting Files:
// The root directory and its contents may be:
//   - Referenced by other shares (shared directory trees)
//   - Still being accessed by active clients
//   - Intended to persist beyond the share lifetime
//
// Callers should ensure proper cleanup before deleting shares if needed:
//  1. Disconnect all client sessions for this share
//  2. Optionally remove files if desired (via RemoveDirectory recursively)
//  3. Then delete the share
//
// Garbage Collection:
// A garbage collector could later identify and clean up orphaned files
// (files not reachable from any share root).
//
// Parameters:
//   - ctx: Context for cancellation
//   - name: The share name to delete
//
// Returns:
//   - error: ErrNotFound if share doesn't exist, or context cancellation error
func (store *MemoryMetadataStore) DeleteShare(
	ctx context.Context,
	name string,
) error {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	// Check if share exists
	if _, exists := store.shares[name]; !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    name,
		}
	}

	// Remove share configuration only
	// Note: Does NOT remove files or clean up root directory
	delete(store.shares, name)

	return nil
}

// GetShareRoot returns the root directory handle for a share.
//
// This is the entry point for all filesystem operations within a share.
// Clients use this handle as the starting point for path traversal and
// file operations.
//
// Implementation Details:
//   - Returns the handle stored in shareData
//   - Handle remains valid until the root directory is explicitly deleted
//   - The root directory exists as long as it's in the files map
//
// Parameters:
//   - ctx: Context for cancellation
//   - name: The name of the share
//
// Returns:
//   - FileHandle: Root directory handle for the share
//   - error: ErrNotFound if share doesn't exist, or context cancellation error
func (store *MemoryMetadataStore) GetShareRoot(ctx context.Context, name string) (metadata.FileHandle, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	// Look up share
	sd, exists := store.shares[name]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    name,
		}
	}

	// Return the root handle
	return sd.RootHandle, nil
}
