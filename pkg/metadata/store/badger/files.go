package badger

import (
	"context"
	"fmt"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// errFound is used to signal iterator completion when we find a match
var errFound = fmt.Errorf("found")

// ============================================================================
// File Entry Operations
// ============================================================================

// GetFile retrieves file metadata by handle.
func (s *BadgerMetadataStore) GetFile(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	var result *metadata.File
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		result, err = tx.GetFile(ctx, handle)
		return err
	})
	return result, err
}

// PutFile stores or updates file metadata.
func (s *BadgerMetadataStore) PutFile(ctx context.Context, file *metadata.File) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutFile(ctx, file)
	})
}

// DeleteFile removes file metadata by handle.
func (s *BadgerMetadataStore) DeleteFile(ctx context.Context, handle metadata.FileHandle) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteFile(ctx, handle)
	})
}

// GetFileByContentID retrieves file metadata by its content identifier.
// Note: This is O(n) and may be slow for large filesystems.
func (s *BadgerMetadataStore) GetFileByContentID(ctx context.Context, contentID metadata.ContentID) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var result *metadata.File

	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.PrefetchSize = 100
		it := txn.NewIterator(opts)
		defer it.Close()

		prefix := []byte(prefixFile)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}

			item := it.Item()
			err := item.Value(func(val []byte) error {
				file, err := decodeFile(val)
				if err != nil {
					return nil // Skip corrupted entries
				}

				if file.ContentID == contentID {
					result = file
					return errFound
				}
				return nil
			})

			if err == errFound {
				return nil
			}
			if err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	if result == nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: fmt.Sprintf("no file found with content ID: %s", contentID),
		}
	}

	return result, nil
}

// ============================================================================
// Directory Operations
// ============================================================================

// GetChild resolves a name in a directory to a file handle.
func (s *BadgerMetadataStore) GetChild(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error) {
	var result metadata.FileHandle
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		result, err = tx.GetChild(ctx, dirHandle, name)
		return err
	})
	return result, err
}

// SetChild adds or updates a child entry in a directory.
func (s *BadgerMetadataStore) SetChild(ctx context.Context, dirHandle metadata.FileHandle, name string, childHandle metadata.FileHandle) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetChild(ctx, dirHandle, name, childHandle)
	})
}

// DeleteChild removes a child entry from a directory.
func (s *BadgerMetadataStore) DeleteChild(ctx context.Context, dirHandle metadata.FileHandle, name string) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteChild(ctx, dirHandle, name)
	})
}

// ListChildren returns directory entries with pagination support.
func (s *BadgerMetadataStore) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int) ([]metadata.DirEntry, string, error) {
	var entries []metadata.DirEntry
	var nextCursor string
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		entries, nextCursor, err = tx.ListChildren(ctx, dirHandle, cursor, limit)
		return err
	})
	return entries, nextCursor, err
}

// ============================================================================
// Parent/Link Operations
// ============================================================================

// GetParent returns the parent handle for a file/directory.
func (s *BadgerMetadataStore) GetParent(ctx context.Context, handle metadata.FileHandle) (metadata.FileHandle, error) {
	var result metadata.FileHandle
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		result, err = tx.GetParent(ctx, handle)
		return err
	})
	return result, err
}

// SetParent sets the parent handle for a file/directory.
func (s *BadgerMetadataStore) SetParent(ctx context.Context, handle metadata.FileHandle, parentHandle metadata.FileHandle) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetParent(ctx, handle, parentHandle)
	})
}

// GetLinkCount returns the hard link count for a file.
func (s *BadgerMetadataStore) GetLinkCount(ctx context.Context, handle metadata.FileHandle) (uint32, error) {
	var result uint32
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		result, err = tx.GetLinkCount(ctx, handle)
		return err
	})
	return result, err
}

// SetLinkCount sets the hard link count for a file.
func (s *BadgerMetadataStore) SetLinkCount(ctx context.Context, handle metadata.FileHandle, count uint32) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetLinkCount(ctx, handle, count)
	})
}

// ============================================================================
// Filesystem Metadata
// ============================================================================

// GetFilesystemMeta retrieves filesystem metadata for a share.
func (s *BadgerMetadataStore) GetFilesystemMeta(ctx context.Context, shareName string) (*metadata.FilesystemMeta, error) {
	var meta *metadata.FilesystemMeta
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		meta, err = tx.GetFilesystemMeta(ctx, shareName)
		return err
	})
	return meta, err
}

// PutFilesystemMeta stores filesystem metadata for a share.
func (s *BadgerMetadataStore) PutFilesystemMeta(ctx context.Context, shareName string, meta *metadata.FilesystemMeta) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutFilesystemMeta(ctx, shareName, meta)
	})
}
