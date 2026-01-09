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
	var result *metadata.File
	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		result, err = tx.GetFile(ctx, handle)
		return err
	})
	return result, err
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
	var result metadata.FileHandle
	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		result, err = tx.GetChild(ctx, dirHandle, name)
		return err
	})
	return result, err
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
	var result metadata.FileHandle
	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		result, err = tx.GetParent(ctx, handle)
		return err
	})
	return result, err
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
	var result uint32
	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		result, err = tx.GetLinkCount(ctx, handle)
		return err
	})
	return result, err
}

// SetLinkCount sets the hard link count for a file.
func (store *MemoryMetadataStore) SetLinkCount(ctx context.Context, handle metadata.FileHandle, count uint32) error {
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetLinkCount(ctx, handle, count)
	})
}

// ListChildren returns directory entries with pagination support.
func (store *MemoryMetadataStore) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int) ([]metadata.DirEntry, string, error) {
	var entries []metadata.DirEntry
	var nextCursor string
	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		entries, nextCursor, err = tx.ListChildren(ctx, dirHandle, cursor, limit)
		return err
	})
	return entries, nextCursor, err
}

// GetFilesystemMeta retrieves filesystem metadata for a share.
func (store *MemoryMetadataStore) GetFilesystemMeta(ctx context.Context, shareName string) (*metadata.FilesystemMeta, error) {
	var meta *metadata.FilesystemMeta
	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		meta, err = tx.GetFilesystemMeta(ctx, shareName)
		return err
	})
	return meta, err
}

// PutFilesystemMeta stores filesystem metadata for a share.
func (store *MemoryMetadataStore) PutFilesystemMeta(ctx context.Context, shareName string, meta *metadata.FilesystemMeta) error {
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutFilesystemMeta(ctx, shareName, meta)
	})
}

// GetFileByContentID retrieves file metadata by its content identifier.
//
// This scans all files to find one matching the given ContentID.
// Note: This is O(n) and may be slow for large filesystems.
func (store *MemoryMetadataStore) GetFileByContentID(
	ctx context.Context,
	contentID metadata.ContentID,
) (*metadata.File, error) {
	// Check context before acquiring lock
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	// Scan all files for matching ContentID
	for _, fileData := range store.files {
		if fileData.Attr.ContentID == contentID {
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
		Message: fmt.Sprintf("no file found with content ID: %s", contentID),
	}
}
