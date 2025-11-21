package memory

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

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
		rootAttrCopy.Mode = 0755
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
