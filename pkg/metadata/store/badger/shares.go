package badger

import (
	"context"
	"fmt"
	"time"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sharecache"
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
		if err != nil {
			return mapBadgerError(err, "share", shareName)
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

	// Cache fast path: skip the badger View txn + share-record decode on a hit.
	// Return a deep copy so callers can never mutate the shared cache entry.
	if cached, ok := s.shareCache.Get(shareName); ok {
		return sharecache.Clone(cached), nil
	}

	// Snapshot the invalidation generation BEFORE the backing read so a write
	// that races this read cannot leave a stale value cached (store() checks it).
	gen := s.shareCache.Generation()

	var opts *metadata.ShareOptions
	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyShare(shareName))
		if err != nil {
			return mapBadgerError(err, "share", shareName)
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

	s.shareCache.Store(shareName, opts, gen)
	return sharecache.Clone(opts), nil
}

// ============================================================================
// Share Lifecycle Operations
// ============================================================================

// CreateShare creates a new share with the given configuration.
func (s *BadgerMetadataStore) CreateShare(ctx context.Context, share *metadata.Share) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	err := s.updateWithConflictRetry(ctx, func(txn *badgerdb.Txn) error {
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
	if err == nil {
		s.shareCache.Invalidate(share.Name)
	}
	return err
}

// UpdateShareOptions updates the share configuration options.
func (s *BadgerMetadataStore) UpdateShareOptions(ctx context.Context, shareName string, options *metadata.ShareOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	err := s.updateWithConflictRetry(ctx, func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyShare(shareName))
		if err != nil {
			return mapBadgerError(err, "share", shareName)
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
	if err == nil {
		s.shareCache.Invalidate(shareName)
	}
	return err
}

// DeleteShare removes a share and all its metadata.
func (s *BadgerMetadataStore) DeleteShare(ctx context.Context, shareName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	var freedBytes int64
	var quotaFreed map[basestore.QuotaKey]metadata.UsageStat
	err := s.updateWithConflictRetry(ctx, func(txn *badgerdb.Txn) error {
		_, err := txn.Get(keyShare(shareName))
		if err != nil {
			return mapBadgerError(err, "share", shareName)
		}

		freed, quota, err := s.deleteShareFiles(txn, shareName)
		if err != nil {
			return err
		}
		freedBytes = freed
		quotaFreed = quota

		return txn.Delete(keyShare(shareName))
	})
	if err != nil {
		return err
	}
	// Drop any cached options for the removed share, after a successful commit.
	s.shareCache.Invalidate(shareName)
	// Apply the usedBytes decrement once, after a successful commit.
	if freedBytes > 0 {
		s.usedBytes.Add(-freedBytes)
	}
	s.applyQuotaDelta(quotaFreed)
	return nil
}

// applyQuotaDelta folds a per-identity usage delta into the in-memory usage
// cache. Called post-commit (matching usedBytes). Buckets that drop to zero or
// below are removed.
func (s *BadgerMetadataStore) applyQuotaDelta(delta map[basestore.QuotaKey]metadata.UsageStat) {
	if len(delta) == 0 {
		return
	}
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	s.quota.Apply(delta)
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
// deleteShareFiles returns the total regular-file bytes it removed (freedBytes)
// rather than mutating usedBytes inline. The caller applies the decrement once
// after a successful commit — the tx-path accumulates it into the transaction's
// pendingDelta and the pool-path applies it only once updateWithConflictRetry
// returns nil — so a conflict retry that re-runs the enclosing Update never
// double-counts. The counter is statfs-only, not quota-enforcing.
func (s *BadgerMetadataStore) deleteShareFiles(txn *badgerdb.Txn, shareName string) (freedBytes int64, quotaFreed map[basestore.QuotaKey]metadata.UsageStat, err error) {
	type doomed struct {
		id        uuid.UUID
		objectID  metadata.ContentHash
		payloadID metadata.PayloadID
		isDir     bool
		size      uint64
		isReg     bool
		uid       uint32
		gid       uint32
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
			return 0, nil, fmt.Errorf("badger DeleteShare: copy file value: %w", vErr)
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
			id:        file.ID,
			objectID:  file.ObjectID,
			payloadID: file.PayloadID,
			isDir:     file.Type == metadata.FileTypeDirectory,
			size:      file.Size,
			isReg:     file.Type == metadata.FileTypeRegular,
			uid:       file.UID,
			gid:       file.GID,
		})
	}
	it.Close()

	quotaFreed = make(map[basestore.QuotaKey]metadata.UsageStat)
	for _, v := range victims {
		if delErr := deleteFileKeys(txn, v.id, v.objectID, v.payloadID); delErr != nil {
			return 0, nil, delErr
		}
		// Directories own c:<uuid>:<name> child entries; prefix-scan and
		// delete them so no dangling mapping survives the share.
		if v.isDir {
			if delErr := deleteChildEntries(txn, v.id); delErr != nil {
				return 0, nil, delErr
			}
		}
		if v.isReg {
			if v.size > 0 {
				freedBytes += int64(v.size)
			}
			uk := basestore.QuotaKey{Scope: metadata.QuotaScopeUser, ID: v.uid}
			us := quotaFreed[uk]
			us.Bytes -= int64(v.size)
			us.Files--
			quotaFreed[uk] = us
			gk := basestore.QuotaKey{Scope: metadata.QuotaScopeGroup, ID: v.gid}
			gs := quotaFreed[gk]
			gs.Bytes -= int64(v.size)
			gs.Files--
			quotaFreed[gk] = gs
		}
	}

	return freedBytes, quotaFreed, nil
}

// deleteFileKeys removes the primary file row plus its parent, link-count, and
// (when present) ObjectID and PayloadID secondary-index keys. Shared by
// DeleteFile and DeleteShare so the per-file teardown lives in one place.
// Missing keys are tolerated; the caller owns child-entry and usedBytes cleanup.
func deleteFileKeys(txn *badgerdb.Txn, id uuid.UUID, objectID metadata.ContentHash, payloadID metadata.PayloadID) error {
	keys := [][]byte{
		keyFile(id),
		keyFileManifest(id),
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
	if payloadID != "" {
		if err := txn.Delete(keyPayloadID(payloadID)); err != nil && err != badgerdb.ErrKeyNotFound {
			return fmt.Errorf("badger: delete payload index: %w", err)
		}
	}
	return nil
}

// deleteChildEntries removes every c:<parentID>:<name> forward mapping and the
// matching cn:<parentID>:<child> reverse mapping under a directory. Collects
// keys under the held txn iterator first, then deletes, to avoid mutating keys
// out from under the iterator.
func deleteChildEntries(txn *badgerdb.Txn, parentID uuid.UUID) error {
	prefixes := [][]byte{
		keyChildPrefix(parentID),
		keyChildNamePrefix(parentID),
	}
	var keys [][]byte
	for _, prefix := range prefixes {
		opts := badgerdb.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			keys = append(keys, it.Item().KeyCopy(nil))
		}
		it.Close()
	}
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

	err := s.updateWithConflictRetry(ctx, func(txn *badgerdb.Txn) error {
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

	// Both branches (createNewRoot / loadExistingRoot) rewrite the share record,
	// so drop any cached options for it after the commit. loadExistingRoot may
	// also rewrite the root inode (mode/UID/GID reconciliation against the
	// configured attrs), a write that bypasses WithTransaction's dirty-file
	// tracking — so drop the root's own cache entries too, or a re-attach with
	// changed ownership keeps serving the previous UID/GID.
	s.shareCache.Invalidate(shareName)
	if rootFile != nil {
		id := rootFile.ID.String()
		s.readCache.invalidate(id)
		s.parentCache.invalidate(id)
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

	// A root directory with no stored link count falls back to the directory
	// default of 2.
	(*rootFile).Nlink = fileLinkCountTxn(txn, *rootFile)

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
	// by a prior CreateShare call) when materializing the root row:
	// writing a fresh metadata.Share{Name: shareName} here would wipe
	// any Options the caller already set.
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
