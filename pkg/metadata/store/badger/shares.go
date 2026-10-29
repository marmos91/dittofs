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
	return basestore.GenerateHandle(ctx, shareName)
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
		tx := &badgerTransaction{store: s, txn: txn}
		var err error
		rootHandle, err = tx.GetRootHandle(ctx, shareName)
		return err
	})
	return rootHandle, err
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
	// that races this read cannot leave a stale value cached (Store checks it).
	gen := s.shareCache.Generation()

	var opts *metadata.ShareOptions
	err := s.db.View(func(txn *badgerdb.Txn) error {
		tx := &badgerTransaction{store: s, txn: txn}
		var err error
		opts, err = tx.GetShareOptions(ctx, shareName)
		return err
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

// UpdateShareOptions updates the share configuration options.
func (s *BadgerMetadataStore) UpdateShareOptions(ctx context.Context, shareName string, options *metadata.ShareOptions) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.UpdateShareOptions(ctx, shareName, options)
	})
}

// shareDeleteBatch bounds the keys one transaction of DeleteShare deletes.
// Badger refuses a transaction past 15% of its memtable: about 13,000 entries
// or 1.2 MiB at the 8 MiB of an in-memory store, the smallest a store opens
// with, and about 105,000 at the default 64 MiB. A third of the smaller leaves
// room for the usage counters the same commit writes and for long names.
const shareDeleteBatch = 4096

// DeleteShare removes a share and all its metadata.
//
// A share can hold more rows than one transaction may delete, so its files go
// in transactions of shareDeleteBatch keys and the share record goes with the
// last. A removal cut short leaves the record, and calling DeleteShare again
// finishes it.
func (s *BadgerMetadataStore) DeleteShare(ctx context.Context, shareName string) error {
	var from []byte
	for {
		var next []byte
		err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
			var err error
			next, err = tx.(*badgerTransaction).deleteShare(shareName, from, shareDeleteBatch)
			return err
		})
		if err != nil || next == nil {
			return err
		}
		from = next
	}
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

// deleteShare deletes the share's files whose keys sort at or after from, up to
// budget keys of them (0: no bound), and the share record once none are left.
// It returns the file key to resume from, or nil once the share is gone.
func (tx *badgerTransaction) deleteShare(shareName string, from []byte, budget int) ([]byte, error) {
	if _, err := tx.txn.Get(keyShare(shareName)); err != nil {
		return nil, mapBadgerError(err, "share", shareName)
	}
	next, err := tx.deleteShareFiles(shareName, from, budget)
	if err != nil || next != nil {
		return next, err
	}
	if err := tx.txn.Delete(keyShare(shareName)); err != nil {
		return nil, err
	}
	tx.dirtyShares = append(tx.dirtyShares, shareName)
	return nil, nil
}

// deleteShareFiles removes the share's file inodes and their dependent keys
// (parent, link-count, child-map, payload index) in key order from the file key
// from, until it has deleted budget keys (0: no bound). It returns the key of
// the first file it left, or nil once it reached the last. Dropping only the
// share key would orphan every one of those entries. Files are collected first,
// then mutated, so keys are never deleted out from under the active iterator.
//
// The freed usage and the deleted file IDs go to the transaction, which applies
// them once it commits, so a conflict retry never counts them twice and a
// deleted file stops resolving through the read caches.
func (tx *badgerTransaction) deleteShareFiles(shareName string, from []byte, budget int) ([]byte, error) {
	type doomed struct {
		key       []byte
		id        uuid.UUID
		payloadID metadata.PayloadID
		isDir     bool
		size      uint64
		isReg     bool
		uid       uint32
		gid       uint32
	}
	var victims []doomed
	var next []byte

	// A file deletes at least four keys of its own, so a call never reaches
	// more than budget/4 of them.
	limit := budget / 4

	opts := badgerdb.DefaultIteratorOptions
	opts.PrefetchValues = true
	it := tx.txn.NewIterator(opts)
	filePrefix := []byte(prefixFile)
	start := filePrefix
	if from != nil {
		start = from
	}
	for it.Seek(start); it.ValidForPrefix(filePrefix); it.Next() {
		item := it.Item()
		val, vErr := item.ValueCopy(nil)
		if vErr != nil {
			it.Close()
			return nil, fmt.Errorf("badger DeleteShare: copy file value: %w", vErr)
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
		if limit > 0 && len(victims) == limit {
			next = item.KeyCopy(nil)
			break
		}
		victims = append(victims, doomed{
			key:       item.KeyCopy(nil),
			id:        file.ID,
			payloadID: file.PayloadID,
			isDir:     file.Type == metadata.FileTypeDirectory,
			size:      file.Size,
			// An unlinked-but-open inode released its bytes when its last name
			// went, so the share has none of them left to give back here.
			isReg: basestore.Charged(file.Type, fileLinkCountTxn(tx.txn, file)),
			uid:   file.UID,
			gid:   file.GID,
		})
	}
	it.Close()

	deleted := 0
	for _, v := range victims {
		if budget > 0 && deleted >= budget {
			return v.key, nil
		}
		// Directories own c:<uuid>:<name> child entries; prefix-scan and
		// delete them so no dangling mapping survives the share. A directory
		// with more of them than the budget has left keeps its own row, so the
		// next call resumes at it.
		if v.isDir {
			n, more, err := deleteChildEntries(tx.txn, v.id, max(budget-deleted, 0))
			if err != nil {
				return nil, err
			}
			deleted += n
			if more {
				return v.key, nil
			}
		}
		n, err := deleteFileKeys(tx.txn, v.id, v.payloadID)
		if err != nil {
			return nil, err
		}
		deleted += n
		tx.dirtyFiles = append(tx.dirtyFiles, v.id.String())
		if v.isReg {
			tx.quota.Add(shareName, v.uid, v.gid, -int64(v.size), -1)
		}
	}
	return next, nil
}

// deleteFileKeys removes the primary file row plus its parent, link-count, and
// (when present) PayloadID secondary-index keys, and returns how many keys it
// deleted. Shared by DeleteFile and DeleteShare so the per-file teardown lives
// in one place. Missing keys are tolerated; the caller owns child-entry and
// usage cleanup.
func deleteFileKeys(txn *badgerdb.Txn, id uuid.UUID, payloadID metadata.PayloadID) (int, error) {
	keys := [][]byte{
		keyFile(id),
		keyFileManifest(id),
		keyParent(id),
		keyLinkCount(id),
	}
	// The manifest is segmented across fm:<uuid>:<seq>, so the whole-list key
	// above clears only a store that predates segmentation. The segments are
	// walked rather than scanned by prefix: DeleteShare runs many files of the
	// share in one transaction, and an iterator per file re-sorts that
	// transaction's whole pending-write set, which is quadratic in the file
	// count.
	segments, err := deleteManifestSegmentsFrom(txn, id, 0)
	if err != nil {
		return 0, err
	}

	for _, key := range keys {
		if err := txn.Delete(key); err != nil && err != badgerdb.ErrKeyNotFound {
			return 0, err
		}
	}
	deleted := segments + len(keys)
	if payloadID != "" {
		if err := txn.Delete(keyPayloadID(payloadID)); err != nil && err != badgerdb.ErrKeyNotFound {
			return 0, fmt.Errorf("badger: delete payload index: %w", err)
		}
		deleted++
	}
	return deleted, nil
}

// deleteChildEntries removes the c:<parentID>:<name> forward mappings and the
// matching cn:<parentID>:<child> reverse mappings under a directory, at most
// limit of them (0: no bound), and reports how many it deleted and whether any
// are left. Collects keys under the held txn iterator first, then deletes, to
// avoid mutating keys out from under the iterator.
func deleteChildEntries(txn *badgerdb.Txn, parentID uuid.UUID, limit int) (int, bool, error) {
	prefixes := [][]byte{
		keyChildPrefix(parentID),
		keyChildNamePrefix(parentID),
	}
	var keys [][]byte
	more := false
	for _, prefix := range prefixes {
		opts := badgerdb.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if limit > 0 && len(keys) == limit {
				more = true
				break
			}
			keys = append(keys, it.Item().KeyCopy(nil))
		}
		it.Close()
		if more {
			break
		}
	}
	for _, k := range keys {
		if err := txn.Delete(k); err != nil && err != badgerdb.ErrKeyNotFound {
			return 0, false, err
		}
	}
	return len(keys), more, nil
}

// ListShares returns the names of all shares.
func (s *BadgerMetadataStore) ListShares(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var names []string
	err := s.db.View(func(txn *badgerdb.Txn) error {
		tx := &badgerTransaction{store: s, txn: txn}
		var err error
		names, err = tx.ListShares(ctx)
		return err
	})
	return names, err
}

// CreateRootDirectory creates a share's root directory, or reconciles an
// existing one (from a previous server run) against the configured attrs, so
// metadata persists across restarts and a changed config still lands.
//
// This one does not delegate to the transaction path the way the rest of this
// file does: the two share loadExistingRoot, but only this path drops the cache
// entries a reconciliation invalidates (see below).
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

	// createNewRoot writes the share record, so drop any cached options for it
	// after the commit rather than tracking which branch ran. loadExistingRoot may
	// also rewrite the root inode (mode/UID/GID reconciliation against the
	// configured attrs), a write that bypasses WithTransaction's dirty-file
	// tracking — so drop the root's own cache entries too, or a re-attach with
	// changed ownership keeps serving the previous UID/GID.
	s.shareCache.Invalidate(shareName)
	if rootFile != nil {
		id := rootFile.ID.String()
		s.readCache.Invalidate(id)
		s.parentCache.Invalidate(id)
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

	// Every writer of a share record sets its root handle, so an empty one is
	// a corrupt record rather than a state to repair: decoding it fails below.
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

	// A zero configured mode means "use the default", the same as it does when
	// the root is first created. Comparing against the raw zero instead would
	// reconcile a defaulted root down to mode 0 on the next call.
	mode := attr.Mode
	if mode == 0 {
		mode = metadata.DefaultRootMode
	}

	// Update attributes if config changed
	needsUpdate := false
	if (*rootFile).Mode != mode {
		logger.Info("Updating root directory mode from config",
			"share", shareName,
			"oldMode", fmt.Sprintf("%o", (*rootFile).Mode),
			"newMode", fmt.Sprintf("%o", mode))
		(*rootFile).Mode = mode
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

	// The caller reached here only because the share record is absent, so
	// there are no recorded options to carry over.
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
