package badger

import (
	"context"
	"encoding/json"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Transaction Support
// ============================================================================

// badgerTransaction wraps a BadgerDB transaction for the Transaction interface.
type badgerTransaction struct {
	store *BadgerMetadataStore
	txn   *badgerdb.Txn
}

// WithTransaction executes fn within a BadgerDB transaction.
//
// If fn returns an error, the transaction is rolled back (discarded).
// If fn returns nil, the transaction is committed.
func (s *BadgerMetadataStore) WithTransaction(ctx context.Context, fn func(tx metadata.Transaction) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
		tx := &badgerTransaction{store: s, txn: txn}
		return fn(tx)
	})
}

// ============================================================================
// Transaction CRUD Operations
// ============================================================================

func (tx *badgerTransaction) GetEntry(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Decode handle to get UUID
	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	item, err := tx.txn.Get(keyFile(fileID))
	if err == badgerdb.ErrKeyNotFound {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}
	if err != nil {
		return nil, err
	}

	var file *metadata.File
	err = item.Value(func(val []byte) error {
		f, decErr := decodeFile(val)
		if decErr != nil {
			return decErr
		}
		file = f
		return nil
	})
	if err != nil {
		return nil, err
	}

	return file, nil
}

func (tx *badgerTransaction) PutEntry(ctx context.Context, file *metadata.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := encodeFile(file)
	if err != nil {
		return err
	}

	return tx.txn.Set(keyFile(file.ID), data)
}

func (tx *badgerTransaction) DeleteEntry(ctx context.Context, handle metadata.FileHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Decode handle to get UUID
	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	// Check if exists first
	_, err = tx.txn.Get(keyFile(fileID))
	if err == badgerdb.ErrKeyNotFound {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}
	if err != nil {
		return err
	}

	// Delete all related keys
	keys := [][]byte{
		keyFile(fileID),
		keyParent(fileID),
		keyLinkCount(fileID),
	}

	for _, key := range keys {
		if err := tx.txn.Delete(key); err != nil && err != badgerdb.ErrKeyNotFound {
			return err
		}
	}

	return nil
}

func (tx *badgerTransaction) GetChild(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Decode directory handle to get UUID
	shareName, dirID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	item, err := tx.txn.Get(keyChild(dirID, name))
	if err == badgerdb.ErrKeyNotFound {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "child not found",
		}
	}
	if err != nil {
		return nil, err
	}

	var childID uuid.UUID
	err = item.Value(func(val []byte) error {
		childID, err = uuid.FromBytes(val)
		return err
	})
	if err != nil {
		return nil, err
	}

	// Encode child handle with same share name
	return metadata.EncodeShareHandle(shareName, childID)
}

func (tx *badgerTransaction) SetChild(ctx context.Context, dirHandle metadata.FileHandle, name string, childHandle metadata.FileHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Decode directory handle to get UUID
	_, dirID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	// Decode child handle to get UUID
	_, childID, err := metadata.DecodeFileHandle(childHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid child handle",
		}
	}

	// Store child UUID bytes
	return tx.txn.Set(keyChild(dirID, name), childID[:])
}

func (tx *badgerTransaction) DeleteChild(ctx context.Context, dirHandle metadata.FileHandle, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Decode directory handle to get UUID
	_, dirID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	_, err = tx.txn.Get(keyChild(dirID, name))
	if err == badgerdb.ErrKeyNotFound {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "child not found",
		}
	}
	if err != nil {
		return err
	}

	return tx.txn.Delete(keyChild(dirID, name))
}

func (tx *badgerTransaction) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int) ([]metadata.DirEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	// Decode directory handle to get UUID and share name
	shareName, dirID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return nil, "", &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	prefix := keyChildPrefix(dirID)
	opts := badgerdb.DefaultIteratorOptions
	opts.Prefix = prefix

	it := tx.txn.NewIterator(opts)
	defer it.Close()

	if limit <= 0 {
		limit = 1000
	}

	var entries []metadata.DirEntry
	startKey := prefix
	if cursor != "" {
		startKey = keyChild(dirID, cursor)
	}

	for it.Seek(startKey); it.ValidForPrefix(prefix) && len(entries) < limit; it.Next() {
		item := it.Item()

		// Extract name from key
		name := extractNameFromChildKey(item.Key(), prefix)
		if name == "" || (cursor != "" && name == cursor) {
			continue
		}

		var childID uuid.UUID
		err := item.Value(func(val []byte) error {
			childID, err = uuid.FromBytes(val)
			return err
		})
		if err != nil {
			return nil, "", err
		}

		// Encode child handle with same share name
		childHandle, err := metadata.EncodeShareHandle(shareName, childID)
		if err != nil {
			return nil, "", err
		}

		entry := metadata.DirEntry{
			ID:     metadata.HandleToINode(childHandle),
			Name:   name,
			Handle: childHandle,
		}

		// Try to get attributes
		fileItem, err := tx.txn.Get(keyFile(childID))
		if err == nil {
			err = fileItem.Value(func(val []byte) error {
				file, decErr := decodeFile(val)
				if decErr != nil {
					return decErr
				}
				entry.Attr = &file.FileAttr
				return nil
			})
		}

		entries = append(entries, entry)
	}

	nextCursor := ""
	if len(entries) >= limit {
		nextCursor = entries[len(entries)-1].Name
	}

	return entries, nextCursor, nil
}

func (tx *badgerTransaction) GetParent(ctx context.Context, handle metadata.FileHandle) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Decode handle to get UUID and share name
	shareName, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	item, err := tx.txn.Get(keyParent(fileID))
	if err == badgerdb.ErrKeyNotFound {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "parent not found",
		}
	}
	if err != nil {
		return nil, err
	}

	var parentID uuid.UUID
	err = item.Value(func(val []byte) error {
		parentID, err = uuid.FromBytes(val)
		return err
	})
	if err != nil {
		return nil, err
	}

	// Encode parent handle with same share name
	return metadata.EncodeShareHandle(shareName, parentID)
}

func (tx *badgerTransaction) SetParent(ctx context.Context, handle metadata.FileHandle, parentHandle metadata.FileHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Decode child handle to get UUID
	_, childID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	// Decode parent handle to get UUID
	_, parentID, err := metadata.DecodeFileHandle(parentHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid parent handle",
		}
	}

	return tx.txn.Set(keyParent(childID), parentID[:])
}

func (tx *badgerTransaction) GetLinkCount(ctx context.Context, handle metadata.FileHandle) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	// Decode handle to get UUID
	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return 0, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	item, err := tx.txn.Get(keyLinkCount(fileID))
	if err == badgerdb.ErrKeyNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	var count uint32
	err = item.Value(func(val []byte) error {
		count, err = decodeUint32(val)
		return err
	})
	if err != nil {
		return 0, err
	}

	return count, nil
}

func (tx *badgerTransaction) SetLinkCount(ctx context.Context, handle metadata.FileHandle, count uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Decode handle to get UUID
	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	return tx.txn.Set(keyLinkCount(fileID), encodeUint32(count))
}

func (tx *badgerTransaction) GetFilesystemMeta(ctx context.Context, shareName string) (*metadata.FilesystemMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	item, err := tx.txn.Get(keyFilesystemMeta(shareName))
	if err == badgerdb.ErrKeyNotFound {
		// Return defaults
		return &metadata.FilesystemMeta{
			Capabilities: tx.store.capabilities,
		}, nil
	}
	if err != nil {
		return nil, err
	}

	var meta metadata.FilesystemMeta
	err = item.Value(func(val []byte) error {
		return json.Unmarshal(val, &meta)
	})
	if err != nil {
		return nil, err
	}

	return &meta, nil
}

func (tx *badgerTransaction) PutFilesystemMeta(ctx context.Context, shareName string, meta *metadata.FilesystemMeta) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	return tx.txn.Set(keyFilesystemMeta(shareName), data)
}

// ============================================================================
// Additional Store Methods
// ============================================================================

// ListChildren implements the Transaction interface for non-transactional calls.
func (s *BadgerMetadataStore) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int) ([]metadata.DirEntry, string, error) {
	var entries []metadata.DirEntry
	var nextCursor string

	err := s.db.View(func(txn *badgerdb.Txn) error {
		tx := &badgerTransaction{store: s, txn: txn}
		var err error
		entries, nextCursor, err = tx.ListChildren(ctx, dirHandle, cursor, limit)
		return err
	})

	return entries, nextCursor, err
}

// GetFilesystemMeta retrieves filesystem metadata for a share.
func (s *BadgerMetadataStore) GetFilesystemMeta(ctx context.Context, shareName string) (*metadata.FilesystemMeta, error) {
	var meta *metadata.FilesystemMeta

	err := s.db.View(func(txn *badgerdb.Txn) error {
		tx := &badgerTransaction{store: s, txn: txn}
		var err error
		meta, err = tx.GetFilesystemMeta(ctx, shareName)
		return err
	})

	return meta, err
}

// PutFilesystemMeta stores filesystem metadata for a share.
func (s *BadgerMetadataStore) PutFilesystemMeta(ctx context.Context, shareName string, meta *metadata.FilesystemMeta) error {
	return s.db.Update(func(txn *badgerdb.Txn) error {
		tx := &badgerTransaction{store: s, txn: txn}
		return tx.PutFilesystemMeta(ctx, shareName, meta)
	})
}

// CreateShare creates a new share with the given configuration.
func (s *BadgerMetadataStore) CreateShare(ctx context.Context, share *metadata.Share) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
		// Check if share already exists
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

		// Store share configuration
		data, err := json.Marshal(share)
		if err != nil {
			return err
		}

		return txn.Set(keyShare(share.Name), data)
	})
}

// DeleteShare removes a share and all its metadata.
func (s *BadgerMetadataStore) DeleteShare(ctx context.Context, shareName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
		// Check if share exists
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

		// Delete share key
		if err := txn.Delete(keyShare(shareName)); err != nil {
			return err
		}

		// Note: We don't delete all files here as that would be expensive.
		// The caller should handle file cleanup or use garbage collection.
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
			// Extract share name from key
			name := string(key[len(prefix):])
			names = append(names, name)
		}

		return nil
	})

	return names, err
}

// ============================================================================
// Key Helper Functions
// ============================================================================

func keyFilesystemMeta(shareName string) []byte {
	return []byte(prefixFilesystemMeta + shareName)
}

func extractNameFromChildKey(key, prefix []byte) string {
	if len(key) <= len(prefix) {
		return ""
	}
	return string(key[len(prefix):])
}

const (
	prefixFilesystemMeta = "fsmeta:"
)
