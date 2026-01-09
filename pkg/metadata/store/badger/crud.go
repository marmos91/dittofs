package badger

import (
	"context"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// CRUD Operations
// ============================================================================
//
// These methods provide low-level data operations for metadata storage.
// They are thin wrappers around BadgerDB with NO business logic.
// Business logic is handled by shared functions in the metadata package.

// GetEntry retrieves file metadata by handle.
// Returns ErrNotFound if handle doesn't exist.
func (s *BadgerMetadataStore) GetEntry(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Decode handle to get UUID
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "failed to decode file handle",
		}
	}

	var file *metadata.File
	err = s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(keyFile(id))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "file not found",
			}
		}
		if err != nil {
			return err
		}

		return item.Value(func(val []byte) error {
			var decErr error
			file, decErr = decodeFile(val)
			if decErr != nil {
				return decErr
			}

			// Get link count
			linkItem, err := txn.Get(keyLinkCount(id))
			if err == nil {
				err = linkItem.Value(func(val []byte) error {
					count, err := decodeUint32(val)
					if err != nil {
						return err
					}
					file.Nlink = count
					return nil
				})
				if err != nil {
					return err
				}
			} else if err != badger.ErrKeyNotFound {
				return err
			}

			return nil
		})
	})

	if err != nil {
		return nil, err
	}

	return file, nil
}

// PutEntry stores or updates file metadata.
// Creates the entry if it doesn't exist.
func (s *BadgerMetadataStore) PutEntry(ctx context.Context, file *metadata.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	fileBytes, err := encodeFile(file)
	if err != nil {
		return err
	}

	return s.db.Update(func(txn *badger.Txn) error {
		if err := txn.Set(keyFile(file.ID), fileBytes); err != nil {
			return err
		}

		// Initialize link count if not exists
		_, err := txn.Get(keyLinkCount(file.ID))
		if err == badger.ErrKeyNotFound {
			var initialCount uint32 = 1
			if file.Type == metadata.FileTypeDirectory {
				initialCount = 2 // . and parent entry
			}
			if err := txn.Set(keyLinkCount(file.ID), encodeUint32(initialCount)); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}

		return nil
	})
}

// DeleteEntry removes file metadata by handle.
// Returns ErrNotFound if handle doesn't exist.
func (s *BadgerMetadataStore) DeleteEntry(ctx context.Context, handle metadata.FileHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "failed to decode file handle",
		}
	}

	return s.db.Update(func(txn *badger.Txn) error {
		// Check if file exists
		_, err := txn.Get(keyFile(id))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "file not found",
			}
		}
		if err != nil {
			return err
		}

		// Delete file data
		if err := txn.Delete(keyFile(id)); err != nil {
			return err
		}

		// Delete parent relationship
		_ = txn.Delete(keyParent(id))

		// Delete link count
		_ = txn.Delete(keyLinkCount(id))

		// Delete device numbers if any
		_ = txn.Delete(keyDeviceNumber(id))

		// Delete all children mappings (if it was a directory)
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = keyChildPrefix(id)
		it := txn.NewIterator(opts)
		defer it.Close()

		var keysToDelete [][]byte
		for it.Seek(opts.Prefix); it.ValidForPrefix(opts.Prefix); it.Next() {
			keysToDelete = append(keysToDelete, append([]byte{}, it.Item().Key()...))
		}
		for _, key := range keysToDelete {
			if err := txn.Delete(key); err != nil {
				return err
			}
		}

		return nil
	})
}

// GetChild resolves a name in a directory to a file handle.
// Returns ErrNotFound if name doesn't exist.
func (s *BadgerMetadataStore) GetChild(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	_, parentID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "failed to decode directory handle",
		}
	}

	var childHandle metadata.FileHandle
	err = s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(keyChild(parentID, name))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "child not found",
			}
		}
		if err != nil {
			return err
		}

		return item.Value(func(val []byte) error {
			childHandle = append([]byte{}, val...)
			return nil
		})
	})

	if err != nil {
		return nil, err
	}

	return childHandle, nil
}

// SetChild adds or updates a child entry in a directory.
func (s *BadgerMetadataStore) SetChild(ctx context.Context, dirHandle metadata.FileHandle, name string, childHandle metadata.FileHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	_, parentID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "failed to decode directory handle",
		}
	}

	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(keyChild(parentID, name), childHandle)
	})
}

// DeleteChild removes a child entry from a directory.
// Returns ErrNotFound if name doesn't exist.
func (s *BadgerMetadataStore) DeleteChild(ctx context.Context, dirHandle metadata.FileHandle, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	_, parentID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "failed to decode directory handle",
		}
	}

	return s.db.Update(func(txn *badger.Txn) error {
		key := keyChild(parentID, name)

		// Check if child exists
		_, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "child not found",
			}
		}
		if err != nil {
			return err
		}

		return txn.Delete(key)
	})
}

// GetParent returns the parent handle for a file/directory.
// Returns ErrNotFound for root directories (no parent).
func (s *BadgerMetadataStore) GetParent(ctx context.Context, handle metadata.FileHandle) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "failed to decode file handle",
		}
	}

	var parentHandle metadata.FileHandle
	err = s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(keyParent(id))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "parent not found",
			}
		}
		if err != nil {
			return err
		}

		return item.Value(func(val []byte) error {
			parentHandle = append([]byte{}, val...)
			return nil
		})
	})

	if err != nil {
		return nil, err
	}

	return parentHandle, nil
}

// SetParent sets the parent handle for a file/directory.
func (s *BadgerMetadataStore) SetParent(ctx context.Context, handle metadata.FileHandle, parentHandle metadata.FileHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "failed to decode file handle",
		}
	}

	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(keyParent(id), parentHandle)
	})
}

// GetLinkCount returns the hard link count for a file.
// Returns 0 if the file doesn't track link counts or doesn't exist.
func (s *BadgerMetadataStore) GetLinkCount(ctx context.Context, handle metadata.FileHandle) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return 0, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "failed to decode file handle",
		}
	}

	var count uint32
	err = s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(keyLinkCount(id))
		if err == badger.ErrKeyNotFound {
			count = 0
			return nil
		}
		if err != nil {
			return err
		}

		return item.Value(func(val []byte) error {
			var decErr error
			count, decErr = decodeUint32(val)
			return decErr
		})
	})

	if err != nil {
		return 0, err
	}

	return count, nil
}

// SetLinkCount sets the hard link count for a file.
func (s *BadgerMetadataStore) SetLinkCount(ctx context.Context, handle metadata.FileHandle, count uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "failed to decode file handle",
		}
	}

	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(keyLinkCount(id), encodeUint32(count))
	})
}

// GenerateHandle creates a new unique file handle for a path in a share.
func (s *BadgerMetadataStore) GenerateHandle(ctx context.Context, shareName string, path string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// BadgerDB uses UUID-based handles, path is stored in File struct
	return metadata.GenerateNewHandle(shareName)
}

// GetRootHandle returns the root handle for a share.
// Returns ErrNotFound if the share doesn't exist.
func (s *BadgerMetadataStore) GetRootHandle(ctx context.Context, shareName string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var rootHandle metadata.FileHandle
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(keyShare(shareName))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "share not found",
				Path:    shareName,
			}
		}
		if err != nil {
			return err
		}

		return item.Value(func(val []byte) error {
			data, err := decodeShareData(val)
			if err != nil {
				return err
			}
			rootHandle = data.RootHandle
			return nil
		})
	})

	if err != nil {
		return nil, err
	}

	return rootHandle, nil
}

// GetShareOptions returns the share configuration options.
// Returns ErrNotFound if the share doesn't exist.
func (s *BadgerMetadataStore) GetShareOptions(ctx context.Context, shareName string) (*metadata.ShareOptions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var opts *metadata.ShareOptions
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(keyShare(shareName))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "share not found",
				Path:    shareName,
			}
		}
		if err != nil {
			return err
		}

		return item.Value(func(val []byte) error {
			data, err := decodeShareData(val)
			if err != nil {
				return err
			}
			optsCopy := data.Share.Options
			opts = &optsCopy
			return nil
		})
	})

	if err != nil {
		return nil, err
	}

	return opts, nil
}
