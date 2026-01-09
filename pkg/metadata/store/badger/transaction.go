package badger

import (
	"context"
	"encoding/json"
	"time"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Transaction Support
// ============================================================================

// badgerTransaction wraps a BadgerDB transaction for the Base interface.
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

func (tx *badgerTransaction) GetFile(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
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

func (tx *badgerTransaction) PutFile(ctx context.Context, file *metadata.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := encodeFile(file)
	if err != nil {
		return err
	}

	return tx.txn.Set(keyFile(file.ID), data)
}

func (tx *badgerTransaction) DeleteFile(ctx context.Context, handle metadata.FileHandle) error {
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

	var metaSvc metadata.FilesystemMeta
	err = item.Value(func(val []byte) error {
		return json.Unmarshal(val, &metaSvc)
	})
	if err != nil {
		return nil, err
	}

	return &metaSvc, nil
}

func (tx *badgerTransaction) PutFilesystemMeta(ctx context.Context, shareName string, metaSvc *metadata.FilesystemMeta) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := json.Marshal(metaSvc)
	if err != nil {
		return err
	}

	return tx.txn.Set(keyFilesystemMeta(shareName), data)
}

func (tx *badgerTransaction) GenerateHandle(ctx context.Context, shareName string, path string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return metadata.GenerateNewHandle(shareName)
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

// ============================================================================
// Transaction Shares Operations
// ============================================================================

func (tx *badgerTransaction) GetRootHandle(ctx context.Context, shareName string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	item, err := tx.txn.Get(keyShare(shareName))
	if err == badgerdb.ErrKeyNotFound {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    shareName,
		}
	}
	if err != nil {
		return nil, err
	}

	var rootHandle metadata.FileHandle
	err = item.Value(func(val []byte) error {
		data, err := decodeShareData(val)
		if err != nil {
			return err
		}
		rootHandle = data.RootHandle
		return nil
	})
	if err != nil {
		return nil, err
	}

	return rootHandle, nil
}

func (tx *badgerTransaction) GetShareOptions(ctx context.Context, shareName string) (*metadata.ShareOptions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	item, err := tx.txn.Get(keyShare(shareName))
	if err == badgerdb.ErrKeyNotFound {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    shareName,
		}
	}
	if err != nil {
		return nil, err
	}

	var opts *metadata.ShareOptions
	err = item.Value(func(val []byte) error {
		data, err := decodeShareData(val)
		if err != nil {
			return err
		}
		optsCopy := data.Share.Options
		opts = &optsCopy
		return nil
	})
	if err != nil {
		return nil, err
	}

	return opts, nil
}

func (tx *badgerTransaction) CreateShare(ctx context.Context, share *metadata.Share) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	_, err := tx.txn.Get(keyShare(share.Name))
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

	return tx.txn.Set(keyShare(share.Name), data)
}

func (tx *badgerTransaction) UpdateShareOptions(ctx context.Context, shareName string, options *metadata.ShareOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	item, err := tx.txn.Get(keyShare(shareName))
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

	data.Share.Options = *options

	updatedData, err := encodeShareData(data)
	if err != nil {
		return err
	}

	return tx.txn.Set(keyShare(shareName), updatedData)
}

func (tx *badgerTransaction) DeleteShare(ctx context.Context, shareName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	_, err := tx.txn.Get(keyShare(shareName))
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

	return tx.txn.Delete(keyShare(shareName))
}

func (tx *badgerTransaction) ListShares(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var names []string

	prefix := []byte(prefixShare)
	opts := badgerdb.DefaultIteratorOptions
	opts.Prefix = prefix
	opts.PrefetchValues = false

	it := tx.txn.NewIterator(opts)
	defer it.Close()

	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
		key := it.Item().Key()
		name := string(key[len(prefix):])
		names = append(names, name)
	}

	return names, nil
}

func (tx *badgerTransaction) CreateRootDirectory(ctx context.Context, shareName string, attr *metadata.FileAttr) (*metadata.File, error) {
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

	// Check if share already exists
	item, err := tx.txn.Get(keyShare(shareName))
	if err == nil {
		// Share exists - load and return existing root
		var existingShareData *shareData
		err := item.Value(func(val []byte) error {
			sd, decErr := decodeShareData(val)
			if decErr != nil {
				return decErr
			}
			existingShareData = sd
			return nil
		})
		if err != nil {
			return nil, err
		}

		_, rootID, err := metadata.DecodeFileHandle(existingShareData.RootHandle)
		if err != nil {
			return nil, err
		}

		rootItem, err := tx.txn.Get(keyFile(rootID))
		if err != nil {
			return nil, err
		}

		var rootFile *metadata.File
		err = rootItem.Value(func(val []byte) error {
			rf, decErr := decodeFile(val)
			if decErr != nil {
				return decErr
			}
			rootFile = rf
			return nil
		})
		if err != nil {
			return nil, err
		}

		return rootFile, nil
	} else if err != badgerdb.ErrKeyNotFound {
		return nil, err
	}

	// Create new root directory
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

	rootFile := &metadata.File{
		ID:        uuid.New(),
		ShareName: shareName,
		Path:      "/",
		FileAttr:  rootAttrCopy,
	}

	fileBytes, err := encodeFile(rootFile)
	if err != nil {
		return nil, err
	}
	if err := tx.txn.Set(keyFile(rootFile.ID), fileBytes); err != nil {
		return nil, err
	}

	if err := tx.txn.Set(keyLinkCount(rootFile.ID), encodeUint32(2)); err != nil {
		return nil, err
	}

	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		return nil, err
	}

	shareDataObj := &shareData{
		Share:      metadata.Share{Name: shareName},
		RootHandle: rootHandle,
	}
	shareBytes, err := encodeShareData(shareDataObj)
	if err != nil {
		return nil, err
	}
	if err := tx.txn.Set(keyShare(shareName), shareBytes); err != nil {
		return nil, err
	}

	return rootFile, nil
}

// ============================================================================
// Transaction ServerConfig Operations
// ============================================================================

func (tx *badgerTransaction) SetServerConfig(ctx context.Context, config metadata.MetadataServerConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	configBytes, err := encodeServerConfig(&config)
	if err != nil {
		return err
	}

	return tx.txn.Set(keyServerConfig(), configBytes)
}

func (tx *badgerTransaction) GetServerConfig(ctx context.Context) (metadata.MetadataServerConfig, error) {
	if err := ctx.Err(); err != nil {
		return metadata.MetadataServerConfig{}, err
	}

	item, err := tx.txn.Get(keyServerConfig())
	if err == badgerdb.ErrKeyNotFound {
		return metadata.MetadataServerConfig{
			CustomSettings: make(map[string]any),
		}, nil
	}
	if err != nil {
		return metadata.MetadataServerConfig{}, err
	}

	var config metadata.MetadataServerConfig
	err = item.Value(func(val []byte) error {
		cfg, decErr := decodeServerConfig(val)
		if decErr != nil {
			return decErr
		}
		config = *cfg
		return nil
	})
	if err != nil {
		return metadata.MetadataServerConfig{}, err
	}

	return config, nil
}

func (tx *badgerTransaction) GetFilesystemCapabilities(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	item, err := tx.txn.Get(keyFilesystemCapabilities())
	if err == badgerdb.ErrKeyNotFound {
		caps := tx.store.capabilities
		return &caps, nil
	}
	if err != nil {
		return nil, err
	}

	var caps *metadata.FilesystemCapabilities
	err = item.Value(func(val []byte) error {
		c, decErr := decodeFilesystemCapabilities(val)
		if decErr != nil {
			return decErr
		}
		caps = c
		return nil
	})
	if err != nil {
		return nil, err
	}

	return caps, nil
}

func (tx *badgerTransaction) SetFilesystemCapabilities(capabilities metadata.FilesystemCapabilities) {
	tx.store.capabilities = capabilities

	data, err := encodeFilesystemCapabilities(&capabilities)
	if err != nil {
		return
	}
	_ = tx.txn.Set(keyFilesystemCapabilities(), data)
}

func (tx *badgerTransaction) GetFilesystemStatistics(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemStatistics, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Compute stats by iterating files
	var fileCount uint64
	var usedSize uint64

	opts := badgerdb.DefaultIteratorOptions
	opts.Prefix = []byte(prefixFile)
	opts.PrefetchValues = true

	it := tx.txn.NewIterator(opts)
	defer it.Close()

	for it.Rewind(); it.Valid(); it.Next() {
		if fileCount%100 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}

		item := it.Item()
		err := item.Value(func(val []byte) error {
			file, decErr := decodeFile(val)
			if decErr != nil {
				return decErr
			}
			fileCount++
			usedSize += file.Size
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	stats := metadata.FilesystemStatistics{
		UsedFiles: fileCount,
		UsedBytes: usedSize,
	}

	if tx.store.maxStorageBytes > 0 {
		stats.TotalBytes = tx.store.maxStorageBytes
		if usedSize < tx.store.maxStorageBytes {
			stats.AvailableBytes = tx.store.maxStorageBytes - usedSize
		}
	} else {
		const reportedSize = 1024 * 1024 * 1024 * 1024 // 1TB
		stats.TotalBytes = reportedSize
		stats.AvailableBytes = reportedSize - usedSize
	}

	if tx.store.maxFiles > 0 {
		stats.TotalFiles = tx.store.maxFiles
		if fileCount < tx.store.maxFiles {
			stats.AvailableFiles = tx.store.maxFiles - fileCount
		}
	} else {
		const reportedFiles = 1000000
		stats.TotalFiles = reportedFiles
		stats.AvailableFiles = reportedFiles - fileCount
	}

	return &stats, nil
}

// ============================================================================
// Transaction Files Operations (additional)
// ============================================================================

func (tx *badgerTransaction) GetFileByContentID(ctx context.Context, contentID metadata.ContentID) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	opts := badgerdb.DefaultIteratorOptions
	opts.PrefetchSize = 100
	it := tx.txn.NewIterator(opts)
	defer it.Close()

	prefix := []byte(prefixFile)
	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		item := it.Item()
		var result *metadata.File
		err := item.Value(func(val []byte) error {
			file, decErr := decodeFile(val)
			if decErr != nil {
				return nil // Skip corrupted entries
			}

			if file.ContentID == contentID {
				result = file
				return errFound
			}
			return nil
		})

		if err == errFound {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
	}

	return nil, &metadata.StoreError{
		Code:    metadata.ErrNotFound,
		Message: "file with content ID not found",
	}
}
