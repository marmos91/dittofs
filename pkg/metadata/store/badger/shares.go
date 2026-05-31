package badger

import (
	"context"
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
			// D-A6: empty BlockLayout (share blob without
			// the field) coerces to `legacy`. Unknown values are
			// fail-loud — matches Postgres backend + ErrInvalidBlockLayout.
			normalized, perr := metadata.ParseBlockLayout(string(optsCopy.BlockLayout))
			if perr != nil {
				return fmt.Errorf("share %q: %w", shareName, perr)
			}
			optsCopy.BlockLayout = normalized
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

		// Store as shareData for consistency with GetRootHandle and CreateRootDirectory
		shareDataValue := &shareData{
			Share: *share,
			// RootHandle will be set by CreateRootDirectory
		}

		encoded, err := encodeShareData(shareDataValue)
		if err != nil {
			return err
		}

		return txn.Set(keyShare(share.Name), encoded)
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

		if err := s.deleteShareFiles(txn, shareName); err != nil {
			return err
		}

		return txn.Delete(keyShare(shareName))
	})
}

// deleteShareFiles removes every file inode and its dependent keys (parent,
// link-count, child-map, objectID index) for a share, decrementing usedBytes
// for regular files. Shared by the pool-path (BadgerMetadataStore.DeleteShare)
// and the transaction-path (badgerTransaction.DeleteShare) so both honor the
// store.go:161 contract ("removes a share and all its metadata") identically;
// dropping only the share key would orphan every file/parent/linkcount/child/
// objectID entry. Files are collected first, then mutated, so keys are never
// deleted out from under the active iterator.
//
// usedBytes is adjusted inline; like PutFile/DeleteFile this shares the known
// tx-retry double-count class (a conflict/serialization retry re-runs the
// enclosing Update and re-applies the delta), tracked for the
// metadata-tx-integrity fix PR. The counter is statfs-only, not quota-enforcing.
func (s *BadgerMetadataStore) deleteShareFiles(txn *badgerdb.Txn, shareName string) error {
	type doomed struct {
		id       uuid.UUID
		objectID metadata.ContentHash
		isDir    bool
		size     uint64
		isReg    bool
	}
	var victims []doomed

	opts := badgerdb.DefaultIteratorOptions
	opts.PrefetchValues = true
	it := txn.NewIterator(opts)
	filePrefix := []byte(prefixFile)
	for it.Seek(filePrefix); it.ValidForPrefix(filePrefix); it.Next() {
		item := it.Item()
		val, vErr := item.ValueCopy(nil)
		if vErr != nil {
			it.Close()
			return fmt.Errorf("badger DeleteShare: copy file value: %w", vErr)
		}
		file, decErr := decodeFile(val)
		if decErr != nil {
			// Skip undecodable rows rather than wedge the whole delete;
			// they cannot be attributed to this share anyway.
			continue
		}
		if file.ShareName != shareName {
			continue
		}
		victims = append(victims, doomed{
			id:       file.ID,
			objectID: file.ObjectID,
			isDir:    file.Type == metadata.FileTypeDirectory,
			size:     file.Size,
			isReg:    file.Type == metadata.FileTypeRegular,
		})
	}
	it.Close()

	for _, v := range victims {
		if delErr := deleteFileKeys(txn, v.id, v.objectID); delErr != nil {
			return delErr
		}
		// Directories own c:<uuid>:<name> child entries; prefix-scan and
		// delete them so no dangling mapping survives the share.
		if v.isDir {
			if delErr := deleteChildEntries(txn, v.id); delErr != nil {
				return delErr
			}
		}
		if v.isReg && v.size > 0 {
			s.usedBytes.Add(-int64(v.size))
		}
	}

	return nil
}

// deleteFileKeys removes the primary file row plus its parent, link-count, and
// (when present) ObjectID secondary-index keys. Shared by DeleteFile and
// DeleteShare so the per-file teardown lives in one place. Missing keys are
// tolerated; the caller owns child-entry and usedBytes cleanup.
func deleteFileKeys(txn *badgerdb.Txn, id uuid.UUID, objectID metadata.ContentHash) error {
	keys := [][]byte{
		keyFile(id),
		keyParent(id),
		keyLinkCount(id),
	}
	for _, key := range keys {
		if err := txn.Delete(key); err != nil && err != badgerdb.ErrKeyNotFound {
			return err
		}
	}
	if !objectID.IsZero() {
		if err := txn.Delete(keyObjectID(objectID)); err != nil && err != badgerdb.ErrKeyNotFound {
			return fmt.Errorf("badger: delete obj index: %w", err)
		}
	}
	return nil
}

// deleteChildEntries removes every c:<parentID>:<name> mapping under a
// directory. Collects keys under the held txn iterator first, then deletes,
// to avoid mutating keys out from under the iterator.
func deleteChildEntries(txn *badgerdb.Txn, parentID uuid.UUID) error {
	prefix := keyChildPrefix(parentID)
	opts := badgerdb.DefaultIteratorOptions
	opts.PrefetchValues = false
	it := txn.NewIterator(opts)
	var keys [][]byte
	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
		keys = append(keys, it.Item().KeyCopy(nil))
	}
	it.Close()
	for _, k := range keys {
		if err := txn.Delete(k); err != nil && err != badgerdb.ErrKeyNotFound {
			return err
		}
	}
	return nil
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

	// If share exists but has no root handle yet (e.g., CreateShare was called
	// separately before CreateRootDirectory), create a new root directory.
	if len(existingShareData.RootHandle) == 0 {
		return s.createNewRoot(txn, shareName, attr, rootFile)
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

	// Look up link count for the root file
	linkItem, linkErr := txn.Get(keyLinkCount(rootID))
	switch linkErr {
	case nil:
		_ = linkItem.Value(func(linkVal []byte) error {
			count, countErr := decodeUint32(linkVal)
			if countErr == nil {
				(*rootFile).Nlink = count
			}
			return nil
		})
	case badgerdb.ErrKeyNotFound:
		// Root directories always have at least 2 links
		(*rootFile).Nlink = 2
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

	// Preserve existing share configuration (e.g. ShareOptions written
	// by a prior CreateShare call) when materializing the root row.
	// (Rule 2 deviation): the original code wrote a
	// fresh `metadata.Share{Name: shareName}` here, silently wiping
	// any Options the caller had set via CreateShare. That's
	// correctness-critical now that ShareOptions.BlockLayout is the
	// per-share dual-read shim gate (D-A6) — losing the field means
	// the engine can't tell migrated shares from unmigrated ones.
	preservedShare := metadata.Share{Name: shareName}
	if existingItem, getErr := txn.Get(keyShare(shareName)); getErr == nil {
		if vErr := existingItem.Value(func(val []byte) error {
			existing, dErr := decodeShareData(val)
			if dErr != nil {
				return dErr
			}
			preservedShare = existing.Share
			// Defensive: ensure Name stays canonical even if a buggy
			// caller stored it as "" via CreateShare.
			preservedShare.Name = shareName
			return nil
		}); vErr != nil {
			return fmt.Errorf("failed to read existing share for option preservation: %w", vErr)
		}
	} else if getErr != badgerdb.ErrKeyNotFound {
		return fmt.Errorf("failed to probe existing share: %w", getErr)
	}

	shareDataObj := &shareData{
		Share:      preservedShare,
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
