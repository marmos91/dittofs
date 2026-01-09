package badger

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Handle Generation
// ============================================================================

// GenerateHandle creates a new unique file handle for a path in a share.
func (s *BadgerMetadataStore) GenerateHandle(ctx context.Context, shareName string, path string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return metadata.GenerateNewHandle(shareName)
}

// ============================================================================
// Share Query Operations
// ============================================================================

// GetRootHandle returns the root handle for a share.
func (s *BadgerMetadataStore) GetRootHandle(ctx context.Context, shareName string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var rootHandle metadata.FileHandle
	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyShare(shareName))
		if err == badgerdb.ErrKeyNotFound {
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
func (s *BadgerMetadataStore) GetShareOptions(ctx context.Context, shareName string) (*metadata.ShareOptions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var opts *metadata.ShareOptions
	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyShare(shareName))
		if err == badgerdb.ErrKeyNotFound {
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

// ============================================================================
// Share Lifecycle Operations
// ============================================================================

// CreateShare creates a new share with the given configuration.
func (s *BadgerMetadataStore) CreateShare(ctx context.Context, share *metadata.Share) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
		_, err := txn.Get(keyShare(share.Name))
		if err == nil {
			return &metadata.StoreError{
				Code:    metadata.ErrAlreadyExists,
				Message: "share already exists",
				Path:    share.Name,
			}
		}
		if err != badgerdb.ErrKeyNotFound {
			return err
		}

		data, err := json.Marshal(share)
		if err != nil {
			return err
		}

		return txn.Set(keyShare(share.Name), data)
	})
}

// UpdateShareOptions updates the share configuration options.
func (s *BadgerMetadataStore) UpdateShareOptions(ctx context.Context, shareName string, options *metadata.ShareOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyShare(shareName))
		if err == badgerdb.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "share not found",
				Path:    shareName,
			}
		}
		if err != nil {
			return err
		}

		var data *shareData
		err = item.Value(func(val []byte) error {
			d, err := decodeShareData(val)
			if err != nil {
				return err
			}
			data = d
			return nil
		})
		if err != nil {
			return err
		}

		// Update options
		data.Share.Options = *options

		updatedData, err := encodeShareData(data)
		if err != nil {
			return err
		}

		return txn.Set(keyShare(shareName), updatedData)
	})
}

// DeleteShare removes a share and all its metadata.
func (s *BadgerMetadataStore) DeleteShare(ctx context.Context, shareName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
		_, err := txn.Get(keyShare(shareName))
		if err == badgerdb.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "share not found",
				Path:    shareName,
			}
		}
		if err != nil {
			return err
		}

		if err := txn.Delete(keyShare(shareName)); err != nil {
			return err
		}

		return nil
	})
}

// ListShares returns the names of all shares.
func (s *BadgerMetadataStore) ListShares(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var names []string

	err := s.db.View(func(txn *badgerdb.Txn) error {
		prefix := []byte(prefixShare)
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			key := it.Item().Key()
			name := string(key[len(prefix):])
			names = append(names, name)
		}

		return nil
	})

	return names, err
}

// ============================================================================
// Root Directory Operations
// ============================================================================

// CreateRootDirectory creates or retrieves the root directory for a share.
//
// If a root directory already exists (from a previous server run), it is returned.
// Otherwise, a new root directory is created. This idempotent behavior ensures
// metadata persists across server restarts.
func (s *BadgerMetadataStore) CreateRootDirectory(ctx context.Context, shareName string, attr *metadata.FileAttr) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if attr.Type != metadata.FileTypeDirectory {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "root must be a directory",
			Path:    shareName,
		}
	}

	var rootFile *metadata.File

	err := s.db.Update(func(txn *badgerdb.Txn) error {
		// Check if share already exists
		item, err := txn.Get(keyShare(shareName))
		if err == nil {
			return s.loadExistingRoot(txn, item, shareName, attr, &rootFile)
		} else if err != badgerdb.ErrKeyNotFound {
			return fmt.Errorf("failed to check for existing share: %w", err)
		}

		return s.createNewRoot(txn, shareName, attr, &rootFile)
	})

	if err != nil {
		return nil, err
	}

	return rootFile, nil
}

func (s *BadgerMetadataStore) loadExistingRoot(txn *badgerdb.Txn, item *badgerdb.Item, shareName string, attr *metadata.FileAttr, rootFile **metadata.File) error {
	var existingShareData *shareData
	err := item.Value(func(val []byte) error {
		sd, err := decodeShareData(val)
		if err != nil {
			return err
		}
		existingShareData = sd
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to decode existing share data: %w", err)
	}

	_, rootID, err := metadata.DecodeFileHandle(existingShareData.RootHandle)
	if err != nil {
		return fmt.Errorf("failed to decode existing root handle: %w", err)
	}

	rootItem, err := txn.Get(keyFile(rootID))
	if err != nil {
		return fmt.Errorf("failed to get existing root file: %w", err)
	}

	err = rootItem.Value(func(val []byte) error {
		rf, err := decodeFile(val)
		if err != nil {
			return err
		}
		*rootFile = rf
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to decode existing root file: %w", err)
	}

	// Update attributes if config changed
	needsUpdate := false
	if (*rootFile).Mode != attr.Mode {
		logger.Info("Updating root directory mode from config",
			"share", shareName,
			"oldMode", fmt.Sprintf("%o", (*rootFile).Mode),
			"newMode", fmt.Sprintf("%o", attr.Mode))
		(*rootFile).Mode = attr.Mode
		needsUpdate = true
	}
	if (*rootFile).UID != attr.UID {
		logger.Info("Updating root directory UID from config",
			"share", shareName, "oldUID", (*rootFile).UID, "newUID", attr.UID)
		(*rootFile).UID = attr.UID
		needsUpdate = true
	}
	if (*rootFile).GID != attr.GID {
		logger.Info("Updating root directory GID from config",
			"share", shareName, "oldGID", (*rootFile).GID, "newGID", attr.GID)
		(*rootFile).GID = attr.GID
		needsUpdate = true
	}

	if needsUpdate {
		(*rootFile).Ctime = time.Now()
		fileBytes, err := encodeFile(*rootFile)
		if err != nil {
			return fmt.Errorf("failed to encode updated root file: %w", err)
		}
		if err := txn.Set(keyFile(rootID), fileBytes); err != nil {
			return fmt.Errorf("failed to update root file: %w", err)
		}
		logger.Info("Root directory attributes updated from config",
			"share", shareName, "rootID", (*rootFile).ID)
	} else {
		logger.Debug("Reusing existing root directory for share",
			"share", shareName, "rootID", (*rootFile).ID)
	}

	return nil
}

func (s *BadgerMetadataStore) createNewRoot(txn *badgerdb.Txn, shareName string, attr *metadata.FileAttr, rootFile **metadata.File) error {
	logger.Debug("Creating new root directory for share", "share", shareName)

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
	if rootAttrCopy.CreationTime.IsZero() {
		rootAttrCopy.CreationTime = now
	}
	rootAttrCopy.Nlink = 2

	*rootFile = &metadata.File{
		ID:        uuid.New(),
		ShareName: shareName,
		Path:      "/",
		FileAttr:  rootAttrCopy,
	}

	fileBytes, err := encodeFile(*rootFile)
	if err != nil {
		return err
	}
	if err := txn.Set(keyFile((*rootFile).ID), fileBytes); err != nil {
		return fmt.Errorf("failed to store root file data: %w", err)
	}

	if err := txn.Set(keyLinkCount((*rootFile).ID), encodeUint32(2)); err != nil {
		return fmt.Errorf("failed to store link count: %w", err)
	}

	rootHandle, err := metadata.EncodeFileHandle(*rootFile)
	if err != nil {
		return fmt.Errorf("failed to encode root handle: %w", err)
	}

	shareDataObj := &shareData{
		Share:      metadata.Share{Name: shareName},
		RootHandle: rootHandle,
	}
	shareBytes, err := encodeShareData(shareDataObj)
	if err != nil {
		return fmt.Errorf("failed to encode share data: %w", err)
	}
	if err := txn.Set(keyShare(shareName), shareBytes); err != nil {
		return fmt.Errorf("failed to store share data: %w", err)
	}

	return nil
}
