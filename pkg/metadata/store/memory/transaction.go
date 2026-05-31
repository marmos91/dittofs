package memory

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
	mderrors "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// ============================================================================
// Transaction Support
// ============================================================================

// memoryTransaction wraps the store for transactional operations.
// Since the memory store uses a global mutex, the transaction
// holds the lock for the duration of all operations.
//
// pendingDelta accumulates the usedBytes change made by mutations inside
// the closure. It is applied to the store's atomic counter exactly once
// after a successful commit (see WithTransaction), so a rolled-back
// closure never touches the counter and a retried closure cannot
// double-count.
type memoryTransaction struct {
	store        *MemoryMetadataStore
	pendingDelta int64
}

// txSnapshot captures the mutable state of the memory store so a failed
// transaction can be rolled back to all-or-nothing. The memory store mutates
// its maps directly under the global write lock; without a snapshot a closure
// that fails midway leaves partial mutations behind, violating the
// WithTransaction contract (interface.go: error → roll back).
//
// Map entries are cloned one level deep. fileData / FileBlock pointer values
// are copied because incrementRefCountLocked mutates *FileBlock in place;
// the rest are replaced (not mutated) by the tx methods, but copying keeps
// the snapshot robust against future in-place edits.
type txSnapshot struct {
	shares        map[string]*shareData
	files         map[string]*fileData
	parents       map[string]metadata.FileHandle
	children      map[string]map[string]metadata.FileHandle
	linkCounts    map[string]uint32
	deviceNumbers map[string]*deviceNumber
	sortedDirs    map[string][]string
	objectIndex   map[blockstore.ContentHash]string
	// hadFileBlockData records whether fileBlockData was allocated at snapshot
	// time. If the closure lazily allocated it (initFileBlockData) and then
	// failed, restore must reset the struct's maps to non-nil empties (or it
	// would leave fileBlockData non-nil with nil maps → a later Put panics on
	// a nil-map write).
	hadFileBlockData bool
	blocks           map[string]*metadata.FileBlock
	hashIndex        map[metadata.ContentHash]string
	serverConfig     metadata.MetadataServerConfig
	capabilities     metadata.FilesystemCapabilities
}

// snapshotLocked captures the store's mutable maps. Caller MUST hold the
// write lock. Top-level maps are shallow-cloned; nested children maps and the
// in-place-mutated FileBlock values are copied so a rolled-back closure cannot
// leak through a shared inner map or pointer.
func (store *MemoryMetadataStore) snapshotLocked() *txSnapshot {
	snap := &txSnapshot{
		shares:        maps.Clone(store.shares),
		files:         maps.Clone(store.files),
		parents:       maps.Clone(store.parents),
		children:      make(map[string]map[string]metadata.FileHandle, len(store.children)),
		linkCounts:    maps.Clone(store.linkCounts),
		deviceNumbers: maps.Clone(store.deviceNumbers),
		sortedDirs:    maps.Clone(store.sortedDirCache),
		objectIndex:   maps.Clone(store.objectIndex),
		serverConfig:  store.serverConfig,
		capabilities:  store.capabilities,
	}
	for k, inner := range store.children {
		snap.children[k] = maps.Clone(inner)
	}
	if store.fileBlockData != nil {
		snap.hadFileBlockData = true
		snap.blocks = make(map[string]*metadata.FileBlock, len(store.fileBlockData.blocks))
		snap.hashIndex = maps.Clone(store.fileBlockData.hashIndex)
		for k, v := range store.fileBlockData.blocks {
			// Copy the struct so an in-place RefCount mutation inside the
			// closure does not leak into the snapshot.
			bc := *v
			snap.blocks[k] = &bc
		}
	}
	return snap
}

// restoreLocked reverts the store's mutable maps to the snapshot. Caller MUST
// hold the write lock.
func (store *MemoryMetadataStore) restoreLocked(snap *txSnapshot) {
	store.shares = snap.shares
	store.files = snap.files
	store.parents = snap.parents
	store.children = snap.children
	store.linkCounts = snap.linkCounts
	store.deviceNumbers = snap.deviceNumbers
	store.sortedDirCache = snap.sortedDirs
	store.objectIndex = snap.objectIndex
	store.serverConfig = snap.serverConfig
	store.capabilities = snap.capabilities
	switch {
	case snap.hadFileBlockData:
		// fileBlockData existed at snapshot time — restore its maps to the
		// captured copies (snap.blocks/hashIndex are non-nil).
		store.fileBlockData.blocks = snap.blocks
		store.fileBlockData.hashIndex = snap.hashIndex
	case store.fileBlockData != nil:
		// The closure lazily allocated fileBlockData then failed. Drop it back
		// to its pre-tx (nil) state so it is re-initialized cleanly on the next
		// use — never left non-nil with nil maps.
		store.fileBlockData = nil
	}
}

// WithTransaction executes fn within a transaction.
//
// For the memory store, this acquires the write lock and holds it for the
// entire duration of fn. The store mutates its maps directly under the lock;
// to honor the all-or-nothing contract a snapshot of every mutable map is
// taken before fn runs and restored if fn returns an error, so a failed
// closure leaves no partial mutations behind.
//
// usedBytes is tracked as a pending delta on the transaction and applied to
// the atomic counter only after the closure succeeds, so a rollback never
// drifts the counter.
//
// The snapshot shallow-clones the top-level maps, so a transaction is O(store
// size) rather than O(keys touched). The memory store is the
// testing/development/ephemeral backend (badger and postgres are the
// persistent backends with native rollback), where correctness and simplicity
// outweigh the clone cost; a write-heavy production workload uses a durable
// backend.
//
// Scope: the snapshot covers the file/directory/share/fileblock metadata maps.
// The separately-mutexed lock store (memoryLockStore, area-5) is NOT snapshotted
// — lock persistence runs in its own transactions and is not mixed with
// file-metadata mutations in a single WithTransaction.
func (store *MemoryMetadataStore) WithTransaction(ctx context.Context, fn func(tx metadata.Transaction) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	snap := store.snapshotLocked()
	tx := &memoryTransaction{store: store}
	if err := fn(tx); err != nil {
		store.restoreLocked(snap)
		return err
	}

	// Commit succeeded — apply the accumulated usedBytes delta once.
	if tx.pendingDelta != 0 {
		store.usedBytes.Add(tx.pendingDelta)
	}
	return nil
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
	// Deep-copy reference-bearing fields (Blocks, ACL) so the stored view
	// cannot be mutated by a later caller-side in-place mutation of the input.
	attrCopy.Blocks = cloneBlocks(file.Blocks)
	attrCopy.ACL = cloneACL(file.ACL)

	// Track size delta for regular files.
	if file.Type == metadata.FileTypeRegular {
		var oldSize uint64
		if existing, exists := tx.store.files[key]; exists && existing.Attr.Type == metadata.FileTypeRegular {
			oldSize = existing.Attr.Size
		}
		// Accumulate into the tx-scoped pending delta; applied once after a
		// successful commit so a rollback/retry never drifts the counter.
		tx.pendingDelta += int64(file.Size) - int64(oldSize)
	}

	// Maintain ObjectID secondary index BEFORE overwriting
	// tx.store.files[key]. The caller (WithTransaction) holds the write
	// lock; tx.store.files is mutated directly without per-call locking.
	//
	// (review iteration 1): race detection MUST run before
	// stale-entry cleanup. memory's WithTransaction has no rollback (see
	// transaction.go WithTransaction docstring): "If fn returns an error,
	// no rollback is needed since operations are performed directly on the
	// maps." If we cleaned the old index entry first and then returned
	// ErrConflict on the race check, the file row would still hold the
	// old ObjectID but the index would no longer map it — a subsequent
	// FindByObjectID(oldObjectID) would return nil even though the file
	// persists with that ObjectID. Reorder so a failed PutFile leaves
	// every map untouched.
	//
	// Step 1: race detection (first-committer-wins). If someone
	// else's file already claims this ObjectID, reject before we mutate
	// any state.
	if !attrCopy.ObjectID.IsZero() {
		if otherKey, claimed := tx.store.objectIndex[attrCopy.ObjectID]; claimed && otherKey != key {
			return mderrors.NewConflictError(
				"memory PutFile",
				fmt.Sprintf("object_id already mapped to file key %s", otherKey),
			)
		}
	}

	// Step 2: stale-entry cleanup. If the existing record had a non-zero
	// ObjectID and we are now writing a different (or zero) ObjectID,
	// drop the old index entry. Only runs after the race check passes
	// so a rejected write never leaves orphaned-or-missing index state.
	if existing, exists := tx.store.files[key]; exists && existing.Attr != nil &&
		!existing.Attr.ObjectID.IsZero() && existing.Attr.ObjectID != attrCopy.ObjectID {
		delete(tx.store.objectIndex, existing.Attr.ObjectID)
	}

	// Step 3: install the new index entry.
	if !attrCopy.ObjectID.IsZero() {
		tx.store.objectIndex[attrCopy.ObjectID] = key
	}

	tx.store.files[key] = &fileData{
		Attr:      &attrCopy,
		ShareName: file.ShareName,
		Path:      file.Path,
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
	existing, exists := tx.store.files[key]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	// Subtract size from the pending delta for regular files.
	if existing.Attr.Type == metadata.FileTypeRegular && existing.Attr.Size > 0 {
		tx.pendingDelta -= int64(existing.Attr.Size)
	}

	// drop ObjectID secondary entry. The "only if mapped
	// to this same key" guard is defensive -- under the write lock that
	// guards both maps a divergence is impossible, but the guard cheaply
	// protects against future refactors.
	if existing.Attr != nil && !existing.Attr.ObjectID.IsZero() {
		if mapped, ok := tx.store.objectIndex[existing.Attr.ObjectID]; ok && mapped == key {
			delete(tx.store.objectIndex, existing.Attr.ObjectID)
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

		// Try to get attributes (deep-copy reference-bearing fields).
		childKey := handleToKey(childHandle)
		if fileData, exists := tx.store.files[childKey]; exists {
			attr := *fileData.Attr
			attr.Blocks = cloneBlocks(fileData.Attr.Blocks)
			attr.ACL = cloneACL(fileData.Attr.ACL)
			entry.Attr = &attr
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
			// Subtract size from the pending delta for regular files.
			if fd.Attr.Type == metadata.FileTypeRegular && fd.Attr.Size > 0 {
				tx.pendingDelta -= int64(fd.Attr.Size)
			}
			// drop ObjectID secondary entry too.
			if fd.Attr != nil && !fd.Attr.ObjectID.IsZero() {
				if mapped, ok := tx.store.objectIndex[fd.Attr.ObjectID]; ok && mapped == key {
					delete(tx.store.objectIndex, fd.Attr.ObjectID)
				}
			}
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
		Path:      "/",
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
