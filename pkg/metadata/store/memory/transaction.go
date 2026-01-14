package memory

import (
	"context"
	"time"

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

func (tx *memoryTransaction) GetFile(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
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

func (tx *memoryTransaction) PutFile(ctx context.Context, file *metadata.File) error {
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

func (tx *memoryTransaction) DeleteFile(ctx context.Context, handle metadata.FileHandle) error {
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

func (tx *memoryTransaction) PutFilesystemMeta(ctx context.Context, shareName string, metaSvc *metadata.FilesystemMeta) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// For memory store, update capabilities
	tx.store.capabilities = metaSvc.Capabilities
	return nil
}

func (tx *memoryTransaction) GenerateHandle(ctx context.Context, shareName string, path string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return metadata.GenerateNewHandle(shareName)
}

func (tx *memoryTransaction) GetFileByPayloadID(ctx context.Context, payloadID metadata.PayloadID) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Search through all files for matching content ID
	for key, fd := range tx.store.files {
		if fd.Attr == nil || fd.Attr.PayloadID == "" {
			continue
		}
		if fd.Attr.PayloadID == payloadID {
			handle := []byte(key)
			file, err := tx.store.buildFileWithNlink(handle, fd)
			if err != nil {
				continue
			}
			return file, nil
		}
	}

	return nil, &metadata.StoreError{
		Code:    metadata.ErrNotFound,
		Message: "file with content ID not found",
	}
}

// ============================================================================
// Transaction Shares Operations
// ============================================================================

func (tx *memoryTransaction) GetRootHandle(ctx context.Context, shareName string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareData, exists := tx.store.shares[shareName]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    shareName,
		}
	}

	return shareData.RootHandle, nil
}

func (tx *memoryTransaction) GetShareOptions(ctx context.Context, shareName string) (*metadata.ShareOptions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareData, exists := tx.store.shares[shareName]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    shareName,
		}
	}

	optsCopy := shareData.Share.Options
	return &optsCopy, nil
}

func (tx *memoryTransaction) CreateShare(ctx context.Context, share *metadata.Share) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if _, exists := tx.store.shares[share.Name]; exists {
		return &metadata.StoreError{
			Code:    metadata.ErrAlreadyExists,
			Message: "share already exists",
			Path:    share.Name,
		}
	}

	rootHandle := tx.store.generateFileHandle(share.Name, "/")
	tx.store.shares[share.Name] = &shareData{
		Share:      *share,
		RootHandle: rootHandle,
	}

	return nil
}

func (tx *memoryTransaction) UpdateShareOptions(ctx context.Context, shareName string, options *metadata.ShareOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	shareData, exists := tx.store.shares[shareName]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    shareName,
		}
	}

	shareData.Share.Options = *options
	return nil
}

func (tx *memoryTransaction) DeleteShare(ctx context.Context, shareName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if _, exists := tx.store.shares[shareName]; !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    shareName,
		}
	}

	// Remove all files belonging to this share
	for key, fd := range tx.store.files {
		if fd.ShareName == shareName {
			delete(tx.store.files, key)
			delete(tx.store.parents, key)
			delete(tx.store.children, key)
			delete(tx.store.linkCounts, key)
			delete(tx.store.deviceNumbers, key)
			delete(tx.store.sortedDirCache, key)
		}
	}

	delete(tx.store.shares, shareName)
	return nil
}

func (tx *memoryTransaction) ListShares(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(tx.store.shares))
	for name := range tx.store.shares {
		names = append(names, name)
	}

	return names, nil
}

func (tx *memoryTransaction) CreateRootDirectory(ctx context.Context, shareName string, attr *metadata.FileAttr) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Validate attributes
	if attr.Type != metadata.FileTypeDirectory {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "root must be a directory",
			Path:    shareName,
		}
	}

	// Generate deterministic handle for root directory based on share name
	rootHandle := tx.store.generateFileHandle(shareName, "/")
	key := handleToKey(rootHandle)

	// Check if root already exists - if so, just return success (idempotent)
	if existingData, exists := tx.store.files[key]; exists {
		_, id, err := metadata.DecodeFileHandle(rootHandle)
		if err != nil {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrIOError,
				Message: "failed to decode root handle",
			}
		}
		return &metadata.File{
			ID:        id,
			ShareName: shareName,
			Path:      "/",
			FileAttr:  *existingData.Attr,
		}, nil
	}

	// Complete root directory attributes with defaults
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

	// Create and store fileData for root directory
	tx.store.files[key] = &fileData{
		Attr:      &rootAttrCopy,
		ShareName: shareName,
	}

	// Initialize children map for root directory (empty initially)
	tx.store.children[key] = make(map[string]metadata.FileHandle)

	// Set link count to 2
	tx.store.linkCounts[key] = 2
	rootAttrCopy.Nlink = 2

	_, id, err := metadata.DecodeFileHandle(rootHandle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: "failed to decode root handle",
		}
	}

	return &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      "/",
		FileAttr:  rootAttrCopy,
	}, nil
}

// ============================================================================
// Transaction ServerConfig Operations
// ============================================================================

func (tx *memoryTransaction) SetServerConfig(ctx context.Context, config metadata.MetadataServerConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	tx.store.serverConfig = config
	return nil
}

func (tx *memoryTransaction) GetServerConfig(ctx context.Context) (metadata.MetadataServerConfig, error) {
	if err := ctx.Err(); err != nil {
		return metadata.MetadataServerConfig{}, err
	}

	return tx.store.serverConfig, nil
}

func (tx *memoryTransaction) GetFilesystemCapabilities(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	capsCopy := tx.store.capabilities
	return &capsCopy, nil
}

func (tx *memoryTransaction) SetFilesystemCapabilities(capabilities metadata.FilesystemCapabilities) {
	tx.store.capabilities = capabilities
}

func (tx *memoryTransaction) GetFilesystemStatistics(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemStatistics, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stats := tx.store.computeStatistics()
	return &stats, nil
}
