package memory

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// cloneBlocks returns a deep copy of a []blockstore.BlockRef. BlockRef is a
// value type (Hash is a [32]byte array, Offset and Size are scalars), so a
// flat element-wise copy is sufficient — there are no shared pointer fields.
//
// Used by PutFile/GetFile in the Memory backend to prevent slice aliasing
// between the caller's view and the stored view (Phase 12 D-05, T-12-09).
//
// Returns nil if the input is nil or empty so the round-trip preserves
// the omitempty wire form (json:"blocks,omitempty" on FileAttr.Blocks).
func cloneBlocks(in []blockstore.BlockRef) []blockstore.BlockRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]blockstore.BlockRef, len(in))
	copy(out, in)
	return out
}

// ============================================================================
// FindByObjectID
// ============================================================================

// FindByObjectID looks up a file by its Merkle-root ObjectID via the
// in-memory secondary index (objectIndex). Returns (nil, nil) on miss
// (zero-valued input, missing index entry, or index drift where the
// indexed handle key no longer points at a valid fileData). Phase 13
// META-02 / D-12.
//
// The objectIndex value is the handle-key string (the same key used in
// store.files); fileData has no separate UUID identifier. Block list is
// returned via cloneBlocks to enforce slice-aliasing discipline
// (Phase 12 D-05).
func (store *MemoryMetadataStore) FindByObjectID(ctx context.Context, objectID blockstore.ObjectID) ([]blockstore.BlockRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if objectID.IsZero() {
		return nil, nil
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	key, ok := store.objectIndex[objectID]
	if !ok {
		return nil, nil
	}
	fd, exists := store.files[key]
	if !exists || fd.Attr == nil {
		// Index drift — the secondary entry points at a removed file.
		// Treat as miss; INV-02 audit reconciles drift.
		return nil, nil
	}
	return cloneBlocks(fd.Attr.Blocks), nil
}

// CountObjectIDIndexRows implements the storetest.ObjectIDIndexAccessor
// optional capability. Returns 1 if the in-memory objectIndex maps the
// given objectID to a handle key, 0 otherwise.
//
// Test-only — never call from production code. Used by the Phase 13
// Plan 05 ConcurrentQuiesceRace scenario to assert exactly one row
// survives the D-14 first-committer-wins resolution.
//
// Zero-valued objectID inputs short-circuit to (0, nil) without map
// access, mirroring FindByObjectID's partial/skip-zero discipline.
func (store *MemoryMetadataStore) CountObjectIDIndexRows(ctx context.Context, objectID blockstore.ObjectID) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if objectID.IsZero() {
		return 0, nil
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if _, ok := store.objectIndex[objectID]; ok {
		return 1, nil
	}
	return 0, nil
}

// ============================================================================
// CRUD Operations
// ============================================================================
//
// These methods delegate to transaction methods via WithTransaction.
// This ensures consistency and avoids duplicating implementation logic.

// GetFile retrieves file metadata by handle.
// Returns ErrNotFound if handle doesn't exist.
func (store *MemoryMetadataStore) GetFile(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Use read lock for this read-only operation
	store.mu.RLock()
	defer store.mu.RUnlock()

	key := handleToKey(handle)
	fileData, exists := store.files[key]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	return store.buildFileWithNlink(handle, fileData)
}

// PutFile stores or updates file metadata.
// Creates the entry if it doesn't exist.
func (store *MemoryMetadataStore) PutFile(ctx context.Context, file *metadata.File) error {
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutFile(ctx, file)
	})
}

// DeleteFile removes file metadata by handle.
// Returns ErrNotFound if handle doesn't exist.
func (store *MemoryMetadataStore) DeleteFile(ctx context.Context, handle metadata.FileHandle) error {
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteFile(ctx, handle)
	})
}

// GetChild resolves a name in a directory to a file handle.
// Returns ErrNotFound if name doesn't exist.
func (store *MemoryMetadataStore) GetChild(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Use read lock for this read-only operation
	store.mu.RLock()
	defer store.mu.RUnlock()

	dirKey := handleToKey(dirHandle)
	childrenMap, exists := store.children[dirKey]
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

// SetChild adds or updates a child entry in a directory.
func (store *MemoryMetadataStore) SetChild(ctx context.Context, dirHandle metadata.FileHandle, name string, childHandle metadata.FileHandle) error {
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetChild(ctx, dirHandle, name, childHandle)
	})
}

// DeleteChild removes a child entry from a directory.
// Returns ErrNotFound if name doesn't exist.
func (store *MemoryMetadataStore) DeleteChild(ctx context.Context, dirHandle metadata.FileHandle, name string) error {
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteChild(ctx, dirHandle, name)
	})
}

// GetParent returns the parent handle for a file/directory.
// Returns ErrNotFound for root directories (no parent).
func (store *MemoryMetadataStore) GetParent(ctx context.Context, handle metadata.FileHandle) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Use read lock for this read-only operation
	store.mu.RLock()
	defer store.mu.RUnlock()

	key := handleToKey(handle)
	parentHandle, exists := store.parents[key]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "parent not found",
		}
	}

	return parentHandle, nil
}

// SetParent sets the parent handle for a file/directory.
func (store *MemoryMetadataStore) SetParent(ctx context.Context, handle metadata.FileHandle, parentHandle metadata.FileHandle) error {
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetParent(ctx, handle, parentHandle)
	})
}

// GetLinkCount returns the hard link count for a file.
// Returns 0 if the file doesn't track link counts or doesn't exist.
func (store *MemoryMetadataStore) GetLinkCount(ctx context.Context, handle metadata.FileHandle) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	// Use read lock for this read-only operation
	store.mu.RLock()
	defer store.mu.RUnlock()

	key := handleToKey(handle)
	count, exists := store.linkCounts[key]
	if !exists {
		return 0, nil
	}

	return count, nil
}

// SetLinkCount sets the hard link count for a file.
func (store *MemoryMetadataStore) SetLinkCount(ctx context.Context, handle metadata.FileHandle, count uint32) error {
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetLinkCount(ctx, handle, count)
	})
}

// ListChildren returns directory entries with pagination support.
// This is a read-only operation and uses a read lock for better concurrency.
func (store *MemoryMetadataStore) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int) ([]metadata.DirEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	// Use read lock for better concurrency (this is a read-only operation)
	store.mu.RLock()
	defer store.mu.RUnlock()

	dirKey := handleToKey(dirHandle)
	childrenMap, exists := store.children[dirKey]
	if !exists {
		// Empty directory
		return []metadata.DirEntry{}, "", nil
	}

	// Get sorted entries (with caching)
	sortedNames := store.getSortedDirEntriesWithCache(dirHandle, childrenMap)

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

		// Try to get attributes with correct nlink
		childKey := handleToKey(childHandle)
		if fileData, exists := store.files[childKey]; exists {
			attr := *fileData.Attr
			if nlink, ok := store.linkCounts[childKey]; ok {
				attr.Nlink = nlink
			} else if attr.Type == metadata.FileTypeDirectory {
				attr.Nlink = 2
			} else {
				attr.Nlink = 1
			}
			// Deep-copy slice fields (Phase 12 D-05, T-12-09).
			attr.Blocks = cloneBlocks(fileData.Attr.Blocks)
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

// GetFilesystemMeta retrieves filesystem metadata for a share.
func (store *MemoryMetadataStore) GetFilesystemMeta(ctx context.Context, shareName string) (*metadata.FilesystemMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Use read lock for this read-only operation
	store.mu.RLock()
	defer store.mu.RUnlock()

	// For memory store, return capabilities and computed statistics
	return &metadata.FilesystemMeta{
		Capabilities: store.capabilities,
		Statistics:   store.computeStatistics(),
	}, nil
}

// PutFilesystemMeta stores filesystem metadata for a share.
func (store *MemoryMetadataStore) PutFilesystemMeta(ctx context.Context, shareName string, meta *metadata.FilesystemMeta) error {
	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutFilesystemMeta(ctx, shareName, meta)
	})
}

// GetFileByPayloadID retrieves file metadata by its content identifier.
//
// This scans all files to find one matching the given PayloadID.
// Note: This is O(n) and may be slow for large filesystems.
func (store *MemoryMetadataStore) GetFileByPayloadID(
	ctx context.Context,
	payloadID metadata.PayloadID,
) (*metadata.File, error) {
	// Check context before acquiring lock
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	// Scan all files for matching PayloadID
	for _, fileData := range store.files {
		if fileData.Attr.PayloadID == payloadID {
			// Return File with just the attributes we need
			// ID and Path aren't needed by the flusher (only Size is used)
			attr := *fileData.Attr
			attr.Blocks = cloneBlocks(fileData.Attr.Blocks)
			return &metadata.File{
				ShareName: fileData.ShareName,
				FileAttr:  attr,
			}, nil
		}
	}

	return nil, &metadata.StoreError{
		Code:    metadata.ErrNotFound,
		Message: fmt.Sprintf("no file found with content ID: %s", payloadID),
	}
}
