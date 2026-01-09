package memory

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Transaction Support
// ============================================================================

// memoryTransaction wraps the store for transactional operations.
// Since the memory store uses a global mutex, the transaction simply
// holds the lock for the duration of all operations.
type memoryTransaction struct {
	store *MemoryMetadataStore
}

// WithTransaction executes fn within a transaction.
//
// For the memory store, this acquires the write lock and holds it for the
// entire duration of fn. If fn returns an error, no rollback is needed since
// operations are performed directly on the maps (no separate transaction buffer).
//
// Note: The memory store doesn't support true transaction rollback. If fn
// performs multiple operations and fails midway, partial changes will persist.
// This is acceptable for testing but not for production use with strict
// atomicity requirements.
func (store *MemoryMetadataStore) WithTransaction(ctx context.Context, fn func(tx metadata.Transaction) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	tx := &memoryTransaction{store: store}
	return fn(tx)
}

// ============================================================================
// Transaction CRUD Operations
// ============================================================================
// These methods operate on the store while the lock is held by WithTransaction.

func (tx *memoryTransaction) GetEntry(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	key := handleToKey(handle)
	fileData, exists := tx.store.files[key]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	return tx.store.buildFileWithNlink(handle, fileData)
}

func (tx *memoryTransaction) PutEntry(ctx context.Context, file *metadata.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	handle, err := metadata.EncodeShareHandle(file.ShareName, file.ID)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "failed to encode file handle",
		}
	}

	key := handleToKey(handle)
	attrCopy := file.FileAttr

	tx.store.files[key] = &fileData{
		Attr:      &attrCopy,
		ShareName: file.ShareName,
	}

	if _, exists := tx.store.linkCounts[key]; !exists {
		if file.Type == metadata.FileTypeDirectory {
			tx.store.linkCounts[key] = 2
		} else {
			tx.store.linkCounts[key] = 1
		}
	}

	return nil
}

func (tx *memoryTransaction) DeleteEntry(ctx context.Context, handle metadata.FileHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	key := handleToKey(handle)
	if _, exists := tx.store.files[key]; !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	delete(tx.store.files, key)
	delete(tx.store.parents, key)
	delete(tx.store.children, key)
	delete(tx.store.linkCounts, key)
	delete(tx.store.deviceNumbers, key)
	delete(tx.store.sortedDirCache, key)

	return nil
}

func (tx *memoryTransaction) GetChild(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dirKey := handleToKey(dirHandle)
	childrenMap, exists := tx.store.children[dirKey]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "child not found",
		}
	}

	childHandle, exists := childrenMap[name]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "child not found",
		}
	}

	return childHandle, nil
}

func (tx *memoryTransaction) SetChild(ctx context.Context, dirHandle metadata.FileHandle, name string, childHandle metadata.FileHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	dirKey := handleToKey(dirHandle)
	if tx.store.children[dirKey] == nil {
		tx.store.children[dirKey] = make(map[string]metadata.FileHandle)
	}

	tx.store.children[dirKey][name] = childHandle
	tx.store.invalidateDirCache(dirHandle)

	return nil
}

func (tx *memoryTransaction) DeleteChild(ctx context.Context, dirHandle metadata.FileHandle, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	dirKey := handleToKey(dirHandle)
	childrenMap, exists := tx.store.children[dirKey]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "child not found",
		}
	}

	if _, exists := childrenMap[name]; !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "child not found",
		}
	}

	delete(childrenMap, name)
	tx.store.invalidateDirCache(dirHandle)

	return nil
}

func (tx *memoryTransaction) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int) ([]metadata.DirEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	dirKey := handleToKey(dirHandle)
	childrenMap, exists := tx.store.children[dirKey]
	if !exists {
		// Empty directory
		return []metadata.DirEntry{}, "", nil
	}

	// Get sorted entries
	sortedNames := tx.store.getSortedDirEntries(dirHandle, childrenMap)

	// Find start position based on cursor
	startIdx := 0
	if cursor != "" {
		for i, name := range sortedNames {
			if name == cursor {
				startIdx = i + 1
				break
			}
		}
	}

	if limit <= 0 {
		limit = 1000 // Default limit
	}

	// Collect entries
	var entries []metadata.DirEntry
	for i := startIdx; i < len(sortedNames) && len(entries) < limit; i++ {
		name := sortedNames[i]
		childHandle := childrenMap[name]

		entry := metadata.DirEntry{
			ID:     metadata.HandleToINode(childHandle),
			Name:   name,
			Handle: childHandle,
		}

		// Try to get attributes
		childKey := handleToKey(childHandle)
		if fileData, exists := tx.store.files[childKey]; exists {
			entry.Attr = fileData.Attr
		}

		entries = append(entries, entry)
	}

	// Determine next cursor
	nextCursor := ""
	if startIdx+len(entries) < len(sortedNames) {
		nextCursor = entries[len(entries)-1].Name
	}

	return entries, nextCursor, nil
}

func (tx *memoryTransaction) GetParent(ctx context.Context, handle metadata.FileHandle) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	key := handleToKey(handle)
	parentHandle, exists := tx.store.parents[key]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "parent not found",
		}
	}

	return parentHandle, nil
}

func (tx *memoryTransaction) SetParent(ctx context.Context, handle metadata.FileHandle, parentHandle metadata.FileHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	key := handleToKey(handle)
	tx.store.parents[key] = parentHandle
	return nil
}

func (tx *memoryTransaction) GetLinkCount(ctx context.Context, handle metadata.FileHandle) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	key := handleToKey(handle)
	count, exists := tx.store.linkCounts[key]
	if !exists {
		return 0, nil
	}

	return count, nil
}

func (tx *memoryTransaction) SetLinkCount(ctx context.Context, handle metadata.FileHandle, count uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	key := handleToKey(handle)
	tx.store.linkCounts[key] = count
	return nil
}

func (tx *memoryTransaction) GetFilesystemMeta(ctx context.Context, shareName string) (*metadata.FilesystemMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// For memory store, return capabilities and computed statistics
	return &metadata.FilesystemMeta{
		Capabilities: tx.store.capabilities,
		Statistics:   tx.store.computeStatistics(),
	}, nil
}

func (tx *memoryTransaction) PutFilesystemMeta(ctx context.Context, shareName string, meta *metadata.FilesystemMeta) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// For memory store, update capabilities
	tx.store.capabilities = meta.Capabilities
	return nil
}

// ============================================================================
// Additional Store Methods
// ============================================================================

// ListChildren implements the Transaction interface for non-transactional calls.
func (store *MemoryMetadataStore) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int) ([]metadata.DirEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	dirKey := handleToKey(dirHandle)
	childrenMap, exists := store.children[dirKey]
	if !exists {
		return []metadata.DirEntry{}, "", nil
	}

	sortedNames := store.getSortedDirEntries(dirHandle, childrenMap)

	startIdx := 0
	if cursor != "" {
		for i, name := range sortedNames {
			if name == cursor {
				startIdx = i + 1
				break
			}
		}
	}

	if limit <= 0 {
		limit = 1000
	}

	var entries []metadata.DirEntry
	for i := startIdx; i < len(sortedNames) && len(entries) < limit; i++ {
		name := sortedNames[i]
		childHandle := childrenMap[name]

		entry := metadata.DirEntry{
			ID:     metadata.HandleToINode(childHandle),
			Name:   name,
			Handle: childHandle,
		}

		childKey := handleToKey(childHandle)
		if fileData, exists := store.files[childKey]; exists {
			entry.Attr = fileData.Attr
		}

		entries = append(entries, entry)
	}

	nextCursor := ""
	if startIdx+len(entries) < len(sortedNames) {
		nextCursor = entries[len(entries)-1].Name
	}

	return entries, nextCursor, nil
}

// GetFilesystemMeta retrieves filesystem metadata for a share.
func (store *MemoryMetadataStore) GetFilesystemMeta(ctx context.Context, shareName string) (*metadata.FilesystemMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	return &metadata.FilesystemMeta{
		Capabilities: store.capabilities,
		Statistics:   store.computeStatistics(),
	}, nil
}

// PutFilesystemMeta stores filesystem metadata for a share.
func (store *MemoryMetadataStore) PutFilesystemMeta(ctx context.Context, shareName string, meta *metadata.FilesystemMeta) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	store.capabilities = meta.Capabilities
	return nil
}

// computeStatistics calculates current filesystem statistics.
// Must be called with at least a read lock held.
func (store *MemoryMetadataStore) computeStatistics() metadata.FilesystemStatistics {
	var totalSize uint64
	fileCount := uint64(len(store.files))

	for _, fd := range store.files {
		totalSize += fd.Attr.Size
	}

	// Report storage limits or defaults
	totalBytes := store.maxStorageBytes
	if totalBytes == 0 {
		totalBytes = 1099511627776 // 1TB default
	}

	maxFiles := store.maxFiles
	if maxFiles == 0 {
		maxFiles = 1000000 // 1 million default
	}

	return metadata.FilesystemStatistics{
		TotalBytes:     totalBytes,
		UsedBytes:      totalSize,
		AvailableBytes: totalBytes - totalSize,
		TotalFiles:     maxFiles,
		UsedFiles:      fileCount,
		AvailableFiles: maxFiles - fileCount,
	}
}

// Close releases any resources held by the store.
// For memory store, this is a no-op.
func (store *MemoryMetadataStore) Close() error {
	return nil
}

// CreateShare creates a new share with the given configuration.
func (store *MemoryMetadataStore) CreateShare(ctx context.Context, share *metadata.Share) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if _, exists := store.shares[share.Name]; exists {
		return &metadata.StoreError{
			Code:    metadata.ErrAlreadyExists,
			Message: "share already exists",
			Path:    share.Name,
		}
	}

	// Generate root handle
	rootHandle := store.generateFileHandle(share.Name, "/")

	store.shares[share.Name] = &shareData{
		Share:      *share,
		RootHandle: rootHandle,
	}

	return nil
}

// DeleteShare removes a share and all its metadata.
func (store *MemoryMetadataStore) DeleteShare(ctx context.Context, shareName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if _, exists := store.shares[shareName]; !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    shareName,
		}
	}

	// Remove all files belonging to this share
	for key, fd := range store.files {
		if fd.ShareName == shareName {
			delete(store.files, key)
			delete(store.parents, key)
			delete(store.children, key)
			delete(store.linkCounts, key)
			delete(store.deviceNumbers, key)
			delete(store.sortedDirCache, key)
		}
	}

	delete(store.shares, shareName)
	return nil
}

// ListShares returns the names of all shares.
func (store *MemoryMetadataStore) ListShares(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	names := make([]string, 0, len(store.shares))
	for name := range store.shares {
		names = append(names, name)
	}

	return names, nil
}
