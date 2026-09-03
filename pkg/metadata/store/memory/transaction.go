package memory

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	mderrors "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
)

// ============================================================================
// Transaction Support
// ============================================================================

// memoryTransaction wraps the store for transactional operations.
// Since the memory store uses a global mutex, the transaction
// holds the lock for the duration of all operations.
type memoryTransaction struct {
	store *MemoryMetadataStore
	// quota accumulates usage changes (bytes + file count) keyed by share and
	// owner identity. Applied to the store's quota cache exactly once after a
	// successful commit, so a rolled-back closure never touches it and a
	// retried closure cannot double-count.
	quota basestore.QuotaDelta
	// syncedOps buffers SyncedHashStore mutations made inside the closure.
	// The synced maps live under their own mutex (syncedMu), NOT store.mu, so
	// they cannot participate in the snapshot/restore rollback: instead the
	// tx methods overlay this map over the store (read-your-writes) and
	// WithTransaction applies it under syncedMu exactly once after a
	// successful commit. A rolled-back closure discards it untouched.
	syncedOps map[block.ContentHash]syncedTxState
}

// syncedTxState is one hash's pending synced-marker mutation inside a tx:
// either a pending delete, or a pending mark carrying the locator to record.
type syncedTxState struct {
	deleted bool
	loc     block.ChunkLocator
}

// txSnapshot captures the mutable state of the memory store so a failed
// transaction can be rolled back to all-or-nothing. The memory store mutates
// its maps directly under the global write lock; without a snapshot a closure
// that fails midway leaves partial mutations behind, violating the
// WithTransaction contract (interface.go: error → roll back).
//
// Map entries are cloned one level deep. fileData / FileChunk pointer values
// are copied because incrementRefCountLocked mutates *FileChunk in place;
// the rest are replaced (not mutated) by the tx methods, but copying keeps
// the snapshot robust against future in-place edits.
type txSnapshot struct {
	shares      map[string]*shareData
	files       map[string]*fileData
	parents     map[string]metadata.FileHandle
	children    map[string]map[string]metadata.FileHandle
	linkCounts  map[string]uint32
	objectIndex map[block.ContentHash]string
	// hadFileChunkData records whether fileChunkData was allocated at snapshot
	// time. If the closure lazily allocated it (initFileChunkData) and then
	// failed, restore must reset the struct's maps to non-nil empties (or it
	// would leave fileChunkData non-nil with nil maps → a later Put panics on
	// a nil-map write).
	hadFileChunkData bool
	blocks           map[string]*metadata.FileChunk
	hashIndex        map[metadata.ContentHash]string
	serverConfig     metadata.MetadataServerConfig
	capabilities     metadata.FilesystemCapabilities
	// Block packing snapshots.
	blockRecords map[string]*block.BlockRecord
}

// snapshotLocked captures the store's mutable maps. Caller MUST hold the
// write lock. Top-level maps are shallow-cloned; nested children maps and the
// in-place-mutated FileChunk values are copied so a rolled-back closure cannot
// leak through a shared inner map or pointer.
func (store *MemoryMetadataStore) snapshotLocked() *txSnapshot {
	snap := &txSnapshot{
		// shares holds *shareData mutated in place (UpdateShareOptions,
		// CreateRootDirectory set RootHandle) — a shallow map clone would share
		// those pointers and let the mutation leak into the snapshot, defeating
		// rollback. Copy each struct (same discipline as fileChunkData.blocks).
		shares:       make(map[string]*shareData, len(store.shares)),
		files:        maps.Clone(store.files),
		parents:      maps.Clone(store.parents),
		children:     make(map[string]map[string]metadata.FileHandle, len(store.children)),
		linkCounts:   maps.Clone(store.linkCounts),
		objectIndex:  maps.Clone(store.objectIndex),
		serverConfig: store.serverConfig,
		capabilities: store.capabilities,
		blockRecords: make(map[string]*block.BlockRecord, len(store.blockRecords)),
	}
	for k, v := range store.shares {
		sc := *v
		snap.shares[k] = &sc
	}
	for k, inner := range store.children {
		snap.children[k] = maps.Clone(inner)
	}
	if store.fileChunkData != nil {
		snap.hadFileChunkData = true
		snap.blocks = make(map[string]*metadata.FileChunk, len(store.fileChunkData.blocks))
		snap.hashIndex = maps.Clone(store.fileChunkData.hashIndex)
		for k, v := range store.fileChunkData.blocks {
			// Copy the struct so an in-place RefCount mutation inside the
			// closure does not leak into the snapshot.
			bc := *v
			snap.blocks[k] = &bc
		}
	}
	for k, v := range store.blockRecords {
		rc := *v
		snap.blockRecords[k] = &rc
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
	store.objectIndex = snap.objectIndex
	store.serverConfig = snap.serverConfig
	store.capabilities = snap.capabilities
	store.blockRecords = snap.blockRecords
	switch {
	case snap.hadFileChunkData:
		// fileChunkData existed at snapshot time — restore its maps to the
		// captured copies (snap.blocks/hashIndex are non-nil).
		store.fileChunkData.blocks = snap.blocks
		store.fileChunkData.hashIndex = snap.hashIndex
	case store.fileChunkData != nil:
		// The closure lazily allocated fileChunkData then failed. Drop it back
		// to its pre-tx (nil) state so it is re-initialized cleanly on the next
		// use — never left non-nil with nil maps.
		store.fileChunkData = nil
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
// Lock state (memoryLockStore) is NOT snapshotted — lock persistence runs in its
// own transactions and is not mixed with file-metadata mutations in a single
// WithTransaction. It is guarded by the same store-wide mutex taken here.
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

	// Commit succeeded — apply the accumulated usage deltas once, under quotaMu.
	if d := tx.quota.Map(); len(d) > 0 {
		store.quotaMu.Lock()
		store.quota.Apply(d)
		store.quotaMu.Unlock()
	}
	// Apply buffered synced-marker mutations once, under syncedMu. Map writes
	// cannot fail, so the commit stays all-or-nothing. A concurrent direct
	// MarkSynced racing the same hash between the closure's first-wins check
	// and this apply loses to the tx — an inherent (and acceptable) race for
	// the in-memory test backend.
	if len(tx.syncedOps) > 0 {
		store.syncedMu.Lock()
		for h, st := range tx.syncedOps {
			if st.deleted {
				delete(store.synced, h)
				delete(store.syncedLocators, h)
				continue
			}
			if store.synced == nil {
				store.synced = make(map[block.ContentHash]time.Time)
			}
			store.synced[h] = time.Now()
			if st.loc.IsStandalone() {
				delete(store.syncedLocators, h)
			} else {
				if store.syncedLocators == nil {
					store.syncedLocators = make(map[block.ContentHash]block.ChunkLocator)
				}
				store.syncedLocators[h] = st.loc
			}
		}
		store.syncedMu.Unlock()
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

	return tx.store.getFileLocked(handle)
}

// SetManifest is UpdateAttrs on this backend: the block list rides the stored
// FileAttr, so there is no separate manifest to rewrite.
func (tx *memoryTransaction) SetManifest(ctx context.Context, file *metadata.File) error {
	return tx.UpdateAttrs(ctx, file)
}

// UpdateAttrs stores or updates file metadata, block list included.
func (tx *memoryTransaction) UpdateAttrs(ctx context.Context, file *metadata.File) error {
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
	attrCopy.EAs = cloneEAs(file.EAs)

	// Track size delta for regular files (store-wide counter) and per-identity
	// usage. Handles three cases: new regular file (count +1, bytes +size),
	// in-place size change (same owner, bytes delta), and chown (move
	// bytes+count from old owner identity to new).
	//
	// This write never touches linkCounts, so the pre-write count is also the
	// post-write one.
	if tx.store.chargedLocked(key, file.Type) {
		var oldSize uint64
		var hadOldRegular bool
		var oldUID, oldGID uint32
		if existing, exists := tx.store.files[key]; exists && existing.Attr.Type == metadata.FileTypeRegular {
			oldSize = existing.Attr.Size
			oldUID = existing.Attr.UID
			oldGID = existing.Attr.GID
			hadOldRegular = true
		}
		switch {
		case !hadOldRegular:
			// New regular file (create or type change to regular): charge full
			// size + 1 inode to the new owner.
			tx.quota.Add(file.ShareName, file.UID, file.GID, int64(file.Size), 1)
		case oldUID == file.UID && oldGID == file.GID:
			// Same owner: only the byte delta moves.
			tx.quota.Add(file.ShareName, file.UID, file.GID, int64(file.Size)-int64(oldSize), 0)
		default:
			// Chown: remove old size + inode from the previous owner, add new
			// size + inode to the new owner.
			tx.quota.Add(file.ShareName, oldUID, oldGID, -int64(oldSize), -1)
			tx.quota.Add(file.ShareName, file.UID, file.GID, int64(file.Size), 1)
		}
	}

	// Maintain ObjectID secondary index BEFORE overwriting
	// tx.store.files[key]. The caller (WithTransaction) holds the write
	// lock; tx.store.files is mutated directly without per-call locking.
	//
	// race detection MUST run before stale-entry cleanup. Although
	// WithTransaction snapshots and restores every map on error, ordering the
	// race check first keeps the no-partial-mutation invariant obvious and
	// independent of the rollback mechanics: if we cleaned the old index entry
	// first and then returned ErrConflict on the race check, the file row would
	// still hold the old ObjectID but the index would no longer map it — a
	// subsequent FindByObjectID(oldObjectID) would return nil even though the
	// file persists with that ObjectID. Reorder so a failed UpdateAttrs leaves
	// every map untouched.
	//
	// Step 1: race detection (first-committer-wins). If someone
	// else's file already claims this ObjectID, reject before we mutate
	// any state.
	if !attrCopy.ObjectID.IsZero() {
		if otherKey, claimed := tx.store.objectIndex[attrCopy.ObjectID]; claimed && otherKey != key {
			return mderrors.NewConflictError(
				"memory UpdateAttrs",
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

	// Remove the inode + bytes from the owner's per-share, per-identity usage.
	// An inode whose last name went already gave them back, so removing the
	// record itself owes the counters nothing.
	if tx.store.chargedLocked(key, existing.Attr.Type) {
		tx.quota.Add(existing.ShareName, existing.Attr.UID, existing.Attr.GID, -int64(existing.Attr.Size), -1)
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

	return nil
}

func (tx *memoryTransaction) GetChild(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return tx.store.getChildLocked(dirHandle, name)
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

	return nil
}

func (tx *memoryTransaction) DeleteChild(ctx context.Context, dirHandle metadata.FileHandle, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	dirKey := handleToKey(dirHandle)
	childrenMap, exists := tx.store.children[dirKey]
	if !exists {
		// A directory with no children map has nothing under any name, which
		// is the state DeleteChild is asked to reach.
		return nil
	}

	delete(childrenMap, name)

	return nil
}

func (tx *memoryTransaction) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int, attrs metadata.ChildAttrs) ([]metadata.DirEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	return tx.store.listChildrenLocked(dirHandle, cursor, limit, attrs)
}

func (tx *memoryTransaction) GetParent(ctx context.Context, handle metadata.FileHandle) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return tx.store.getParentLocked(handle)
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

	return tx.store.getLinkCountLocked(handle), nil
}

func (tx *memoryTransaction) SetLinkCount(ctx context.Context, handle metadata.FileHandle, count uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	key := handleToKey(handle)

	// A link count crossing zero is what puts an inode's bytes into the share's
	// usage or takes them back out; any other change (a hard link added or
	// dropped alongside others) leaves it charged exactly once either way.
	if existing, exists := tx.store.files[key]; exists {
		was := tx.store.chargedLocked(key, existing.Attr.Type)
		now := basestore.Charged(existing.Attr.Type, count)
		switch {
		case was && !now:
			tx.quota.Add(existing.ShareName, existing.Attr.UID, existing.Attr.GID, -int64(existing.Attr.Size), -1)
		case !was && now:
			tx.quota.Add(existing.ShareName, existing.Attr.UID, existing.Attr.GID, int64(existing.Attr.Size), 1)
		}
	}

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
		Statistics:   tx.store.computeStatistics(shareName),
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
	return basestore.GenerateHandle(ctx, shareName)
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
			// Remove the inode + bytes from the owner's per-share,
			// per-identity usage. An unlinked-but-open inode released them
			// when its last name went, so the share has none left to give
			// back for it.
			if tx.store.chargedLocked(key, fd.Attr.Type) {
				tx.quota.Add(fd.ShareName, fd.Attr.UID, fd.Attr.GID, -int64(fd.Attr.Size), -1)
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

	rootHandle, err := tx.store.rootHandleLocked(shareName)
	if err != nil {
		return nil, err
	}
	key := handleToKey(rootHandle)

	// Register the share so it resolves by name (see store-level
	// CreateRootDirectory). Idempotent.
	if _, ok := tx.store.shares[shareName]; !ok {
		tx.store.shares[shareName] = &shareData{
			Share:      metadata.Share{Name: shareName},
			RootHandle: rootHandle,
		}
	}

	// An existing root is reconciled against the configured attributes rather
	// than returned as it stands (see reconcileRootAttrs).
	if existingData, exists := tx.store.files[key]; exists {
		_, id, err := metadata.DecodeFileHandle(rootHandle)
		if err != nil {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrIOError,
				Message: "failed to decode root handle",
			}
		}

		reconciled := reconcileRootAttrs(existingData.Attr, attr)
		tx.store.files[key] = &fileData{Attr: reconciled, ShareName: existingData.ShareName}

		return &metadata.File{
			ID:        id,
			ShareName: shareName,
			Path:      "/",
			FileAttr:  *reconciled,
		}, nil
	}

	// Complete root directory attributes with defaults
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

	return withSettings(tx.store.serverConfig), nil
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

	shareName, _, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, err
	}

	stats := tx.store.computeStatistics(shareName)
	return &stats, nil
}
