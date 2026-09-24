package badger

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// errFound is used to signal iterator completion when we find a match
var errFound = fmt.Errorf("found")

// ============================================================================
// File Entry Operations
// ============================================================================

// GetFile retrieves file metadata by handle.
// Uses a read-only transaction for better concurrency.
func (s *BadgerMetadataStore) GetFile(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var result *metadata.File
	err := s.db.View(func(txn *badgerdb.Txn) error {
		tx := &badgerTransaction{store: s, txn: txn}
		var err error
		result, err = tx.GetFile(ctx, handle)
		return err
	})
	return result, err
}

// GetFileForRead is GetFile without deriving File.Path — it skips the
// parent-edge walk (a per-directory-level pair of badger gets) for the
// handle-addressed hot paths (NFS READ/WRITE/GETATTR) that never read Path.
// Implements the optional metadata read-fast-path interface; other backends
// fall back to GetFile.
func (s *BadgerMetadataStore) GetFileForRead(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Read-cache fast path: skip the badger View txn + File JSON decode for a
	// hot file. Keyed by fileID; invalidated after each committed write.
	_, fileID, decErr := metadata.DecodeFileHandle(handle)
	var key string
	if decErr == nil {
		key = fileID.String()
		if cached, ok := s.readCache.Get(key); ok {
			return copyForRead(cached), nil
		}
	}

	// Snapshot the invalidation generation BEFORE the backing read so a write
	// that races this read cannot leave a stale value cached (store() checks it).
	gen := s.readCache.Generation()
	var result *metadata.File
	err := s.db.View(func(txn *badgerdb.Txn) error {
		tx := &badgerTransaction{store: s, txn: txn}
		var err error
		result, err = tx.getFile(ctx, handle, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	if key != "" {
		s.readCache.Store(key, result, gen)
		return copyForRead(result), nil
	}
	return result, nil
}

// copyForRead returns a caller-owned copy of a cached File: the struct is
// copied and the reference-bearing fields (Blocks, ACL, EAs) are deep-copied so
// neither the caller nor a concurrent reader can mutate the shared cache entry.
// This preserves badger's no-alias invariant — before the read cache, every
// GetFileForRead JSON-decoded a fresh File, so callers never aliased stored
// state. The clones are cheap relative to the decode the cache skips.
func copyForRead(f *metadata.File) *metadata.File {
	cp := *f
	cp.Blocks = metadata.CloneBlocks(f.Blocks)
	cp.ACL = metadata.CloneACL(f.ACL)
	cp.EAs = metadata.CloneEAs(f.EAs)
	return &cp
}

// loadManifest populates file.Blocks from the manifest keys. A legacy f: blob
// that still embeds the manifest arrives with Blocks already set and is left
// untouched (the next write migrates it out); new-format blobs carry no inline
// manifest, so the chunk list is read from its sibling keys. Absent keys mean
// an empty manifest (directory, symlink, or empty regular file).
//
// Two on-disk shapes are read. A store written before segmentation holds the
// whole list under fm:<uuid>; a segmented one holds it across fm:<uuid>:<seq>.
// The whole-list key is checked first and wins, because putManifest retires it
// only when it rewrites the file, so both can exist for exactly as long as it
// takes that file to be written once.
func loadManifest(txn *badgerdb.Txn, file *metadata.File) error {
	if len(file.Blocks) > 0 {
		return nil // legacy embedded manifest — authoritative for this row
	}

	item, err := txn.Get(keyFileManifest(file.ID))
	if err == nil {
		return item.Value(func(val []byte) error {
			blocks, derr := decodeManifest(val)
			if derr != nil {
				return derr
			}
			file.Blocks = blocks
			return nil
		})
	}
	if !errors.Is(err, badgerdb.ErrKeyNotFound) {
		return err
	}

	// Segmented form: a prefix scan returns segments in sequence order, and
	// concatenating their values rebuilds the list in offset order.
	prefix := keyFileManifestPrefix(file.ID)
	opts := badgerdb.DefaultIteratorOptions
	opts.Prefix = prefix
	it := txn.NewIterator(opts)
	defer it.Close()

	var blocks []block.ChunkRef
	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
		if err := it.Item().Value(func(val []byte) error {
			seg, derr := decodeManifest(val)
			if derr != nil {
				return derr
			}
			blocks = append(blocks, seg...)
			return nil
		}); err != nil {
			return err
		}
	}
	file.Blocks = blocks
	return nil
}

// putManifest persists (or, when empty, removes) the block manifest, split
// across fm:<uuid>:<seq> segments of at most manifestSegmentRefs refs each.
//
// Segmenting is what keeps each value below Badger's ValueThreshold, so the
// manifest lives in the LSM where compaction reclaims superseded copies rather
// than in the value log where they accumulate (see manifestSegmentRefs).
//
// A segment whose bytes are unchanged is not rewritten. That is what makes an
// append at EOF cost one segment instead of the whole list: without it, the
// manifest would still be rewritten in full on every commit, merely into
// reclaimable space instead of unreclaimable space.
//
// An empty manifest — a truncated/empty regular file, a directory, or a symlink
// — carries no keys, so loadManifest reads their absence as "no blocks". This
// keeps the manifest coherent when a truncate prunes every chunk.
func (tx *badgerTransaction) putManifest(id uuid.UUID, blocks []block.ChunkRef) error {
	// Retire the legacy whole-list key on any write, so a file migrates to the
	// segmented form the first time it is written and never carries both.
	if err := tx.txn.Delete(keyFileManifest(id)); err != nil && !errors.Is(err, badgerdb.ErrKeyNotFound) {
		return err
	}

	segments := 0
	for start := 0; start < len(blocks); start += manifestSegmentRefs {
		end := min(start+manifestSegmentRefs, len(blocks))

		data, err := encodeManifest(blocks[start:end])
		if err != nil {
			return err
		}
		key := keyFileManifestSegment(id, segments)
		segments++

		if unchanged, err := tx.segmentUnchanged(key, data); err != nil {
			return err
		} else if unchanged {
			continue
		}
		if err := tx.txn.Set(key, data); err != nil {
			return err
		}
	}

	// Drop segments the list no longer reaches — a truncate, a deallocate, or
	// any rewrite that shortened it. Scanning forward from the first surplus
	// sequence stops at the first gap, and there are none: segments are always
	// written from zero without holes.
	for seq := segments; ; seq++ {
		key := keyFileManifestSegment(id, seq)
		if _, err := tx.txn.Get(key); errors.Is(err, badgerdb.ErrKeyNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		if err := tx.txn.Delete(key); err != nil && !errors.Is(err, badgerdb.ErrKeyNotFound) {
			return err
		}
	}
}

// manifestMaterialized reports whether this file already has a manifest on
// disk, in either shape. It answers "does an attr-only write still need to
// materialize the manifest" — true for a file whose blocks are already stored,
// false for a fresh create or a legacy f: blob that still embeds them.
//
// Checking segment zero is sufficient: putManifest writes segments from zero
// upward with no holes, so a manifest exists if and only if that key does.
func (tx *badgerTransaction) manifestMaterialized(id uuid.UUID) (bool, error) {
	for _, key := range [][]byte{keyFileManifest(id), keyFileManifestSegment(id, 0)} {
		_, err := tx.txn.Get(key)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, badgerdb.ErrKeyNotFound) {
			return false, err
		}
	}
	return false, nil
}

// segmentUnchanged reports whether the stored segment already holds exactly
// these bytes. Skipping an identical write is what bounds a commit's cost to
// the segments it actually changed; the read is served from the LSM and is far
// cheaper than the write it avoids.
//
// decision: this widens the transaction's conflict read set. Badger's Txn.Get
// calls addReadKey on an update transaction, so comparing every segment enters
// every segment in the read set, where the previous blind Set entered none.
// Two commits racing on one file's manifest now conflict instead of silently
// taking the later writer's list, which is the outcome RFC 4 §4.4 wants — but
// it is a widening, and the segmentation alone already fixes the unbounded
// growth this change was written for. Withdraw the comparison, and write every
// segment blindly, if commit conflicts on one file are ever measured to cost
// more than the rewrites it saves.
func (tx *badgerTransaction) segmentUnchanged(key, data []byte) (bool, error) {
	item, err := tx.txn.Get(key)
	if errors.Is(err, badgerdb.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if int(item.ValueSize()) != len(data) {
		return false, nil
	}
	same := false
	if err := item.Value(func(val []byte) error {
		same = bytes.Equal(val, data)
		return nil
	}); err != nil {
		return false, err
	}
	return same, nil
}

// UpdateAttrs stores or updates file metadata.
func (s *BadgerMetadataStore) UpdateAttrs(ctx context.Context, file *metadata.File) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.UpdateAttrs(ctx, file)
	})
}

// SetManifest stores or updates file metadata and rewrites the stored block
// manifest from file.Blocks.
func (s *BadgerMetadataStore) SetManifest(ctx context.Context, file *metadata.File) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetManifest(ctx, file)
	})
}

// DeleteFile removes file metadata by handle.
func (s *BadgerMetadataStore) DeleteFile(ctx context.Context, handle metadata.FileHandle) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteFile(ctx, handle)
	})
}

// GetFileByPayloadID retrieves file metadata by its content identifier.
// Steady-state this is an O(1) point lookup via the pl:<payloadID> secondary
// index (#1435); it degrades to an O(n) keyspace scan only for legacy rows
// written before the index existed (those are indexed on their next write).
func (s *BadgerMetadataStore) GetFileByPayloadID(ctx context.Context, payloadID metadata.PayloadID) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var result *metadata.File

	err := s.db.View(func(txn *badgerdb.Txn) error {
		btx := &badgerTransaction{store: s, txn: txn}

		// Fast path: resolve via the pl:<payloadID> secondary index (#1435); an
		// index miss or stale entry falls through to the legacy full scan below.
		if file, found, lookupErr := btx.lookupFileByPayloadIndex(payloadID); lookupErr != nil {
			return lookupErr
		} else if found {
			result = file
			return nil
		}

		opts := badgerdb.DefaultIteratorOptions
		opts.PrefetchSize = 100
		it := txn.NewIterator(opts)
		defer it.Close()

		prefix := []byte(prefixFile)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}

			item := it.Item()
			var matchID uuid.UUID
			err := item.Value(func(val []byte) error {
				file, err := decodeFile(val)
				if err != nil {
					return nil // Skip corrupted entries
				}

				if file.PayloadID == payloadID {
					matchID = file.ID
					return errFound
				}
				return nil
			})

			if err == errFound {
				// Re-load through the shared enrichment path so an unindexed
				// row returns the same shape as the fast path: link count,
				// derived path and the chunk manifest from the fm: key.
				result, err = btx.loadEnrichedFileByID(matchID)
				return err
			}
			if err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	if result == nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: fmt.Sprintf("no file found with content ID: %s", payloadID),
		}
	}

	return result, nil
}

// FindByObjectID looks up a file by its Merkle-root ObjectID via the
// secondary key obj:<hex> -> file UUID (binary-marshaled). Returns
// (nil, nil) on miss (zero-valued input, missing index entry, or index
// drift where the indexed file row no longer exists).
//
// Block list is deep-copied out of the txn-scoped decoded file to avoid
// slice aliasing into Badger's internal buffers (discipline).
func (s *BadgerMetadataStore) FindByObjectID(ctx context.Context, objectID block.ObjectID) ([]block.ChunkRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if objectID.IsZero() {
		return nil, nil
	}

	var blocks []block.ChunkRef
	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyObjectID(objectID))
		if errors.Is(err, badgerdb.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}

		raw, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}

		var fileID uuid.UUID
		if err := fileID.UnmarshalBinary(raw); err != nil {
			return fmt.Errorf("badger FindByObjectID: invalid id bytes: %w", err)
		}

		// Load the file row by primary key.
		fileItem, err := txn.Get(keyFile(fileID))
		if errors.Is(err, badgerdb.ErrKeyNotFound) {
			// Index drift — secondary key points at a removed file.
			// Treat as miss; the audit reconciles drift.
			return nil
		}
		if err != nil {
			return err
		}

		rawFile, err := fileItem.ValueCopy(nil)
		if err != nil {
			return err
		}

		f, err := decodeFile(rawFile)
		if err != nil {
			return err
		}
		if err := loadManifest(txn, f); err != nil {
			return err
		}

		// Deep-copy the ChunkRef slice so the caller's view does not
		// alias the JSON-decoded buffer.
		if len(f.Blocks) > 0 {
			blocks = append([]block.ChunkRef(nil), f.Blocks...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return blocks, nil
}

// CountObjectIDIndexRows implements the storetest.ObjectIDIndexAccessor
// optional capability. Returns 1 if the obj:<hex> secondary key is
// present, 0 otherwise.
//
// Test-only — never call from production code. Used by the
// ConcurrentQuiesceRace scenario to assert exactly one row
// survives the first-committer-wins resolution.
//
// Zero-valued objectID inputs short-circuit to (0, nil) without backend
// access, mirroring FindByObjectID's partial/skip-zero discipline.
func (s *BadgerMetadataStore) CountObjectIDIndexRows(ctx context.Context, objectID block.ObjectID) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if objectID.IsZero() {
		return 0, nil
	}

	var n int
	err := s.db.View(func(txn *badgerdb.Txn) error {
		_, gerr := txn.Get(keyObjectID(objectID))
		if gerr == nil {
			n = 1
			return nil
		}
		if errors.Is(gerr, badgerdb.ErrKeyNotFound) {
			return nil
		}
		return gerr
	})
	if err != nil {
		return 0, fmt.Errorf("badger CountObjectIDIndexRows: %w", err)
	}
	return n, nil
}

// ============================================================================
// Directory Operations
// ============================================================================

// GetChild resolves a name in a directory to a file handle.
func (s *BadgerMetadataStore) GetChild(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var result metadata.FileHandle
	err := s.db.View(func(txn *badgerdb.Txn) error {
		tx := &badgerTransaction{store: s, txn: txn}
		var err error
		result, err = tx.GetChild(ctx, dirHandle, name)
		return err
	})
	return result, err
}

// SetChild adds or updates a child entry in a directory.
func (s *BadgerMetadataStore) SetChild(ctx context.Context, dirHandle metadata.FileHandle, name string, childHandle metadata.FileHandle) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetChild(ctx, dirHandle, name, childHandle)
	})
}

// DeleteChild removes a child entry from a directory.
func (s *BadgerMetadataStore) DeleteChild(ctx context.Context, dirHandle metadata.FileHandle, name string) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteChild(ctx, dirHandle, name)
	})
}

// ListChildren returns directory entries with pagination support.
// Uses a read-only transaction for better concurrency.
func (s *BadgerMetadataStore) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int, attrs metadata.ChildAttrs) ([]metadata.DirEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	var entries []metadata.DirEntry
	var nextCursor string
	err := s.db.View(func(txn *badgerdb.Txn) error {
		tx := &badgerTransaction{store: s, txn: txn}
		var err error
		entries, nextCursor, err = tx.ListChildren(ctx, dirHandle, cursor, limit, attrs)
		return err
	})
	return entries, nextCursor, err
}

// ============================================================================
// Parent/Link Operations
// ============================================================================

// GetParent returns the parent handle for a file/directory.
// Uses a read-only transaction for better concurrency.
func (s *BadgerMetadataStore) GetParent(ctx context.Context, handle metadata.FileHandle) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var result metadata.FileHandle
	err := s.db.View(func(txn *badgerdb.Txn) error {
		tx := &badgerTransaction{store: s, txn: txn}
		var err error
		result, err = tx.GetParent(ctx, handle)
		return err
	})
	return result, err
}

// SetParent sets the parent handle for a file/directory.
func (s *BadgerMetadataStore) SetParent(ctx context.Context, handle metadata.FileHandle, parentHandle metadata.FileHandle) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetParent(ctx, handle, parentHandle)
	})
}

// GetLinkCount returns the hard link count for a file.
// Uses a read-only transaction for better concurrency.
func (s *BadgerMetadataStore) GetLinkCount(ctx context.Context, handle metadata.FileHandle) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var result uint32
	err := s.db.View(func(txn *badgerdb.Txn) error {
		tx := &badgerTransaction{store: s, txn: txn}
		var err error
		result, err = tx.GetLinkCount(ctx, handle)
		return err
	})
	return result, err
}

// SetLinkCount sets the hard link count for a file.
func (s *BadgerMetadataStore) SetLinkCount(ctx context.Context, handle metadata.FileHandle, count uint32) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetLinkCount(ctx, handle, count)
	})
}

// ============================================================================
// Filesystem Metadata
// ============================================================================

// GetFilesystemMeta retrieves filesystem metadata for a share.
func (s *BadgerMetadataStore) GetFilesystemMeta(ctx context.Context, shareName string) (*metadata.FilesystemMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutFilesystemMeta(ctx, shareName, meta)
	})
}

// FileSizeByPayloadID returns just the persisted logical size of the file
// carrying payloadID. found is false when no file row claims the payload.
//
// GetFileByPayloadID answers the same question but also loads the link count,
// the chunk manifest and the derived path, the last of which walks the parent
// chain with two point reads per component. Share start reconciles one size per
// locally-resident file, so on a large store that discarded enrichment is the
// whole cost.
//
// A payload with no pl: index entry reports not-found rather than falling back
// to the keyspace scan GetFileByPayloadID uses for rows written before that
// index existed: the open-time backfill indexes every such row before any
// caller runs, so a miss here means no row holds the payload (an orphan journal
// entry). An index entry that no longer resolves to a row claiming the payload
// does fall back, and does so through GetFileByPayloadID itself, so the answer
// stays identical.
func (s *BadgerMetadataStore) FileSizeByPayloadID(ctx context.Context, payloadID metadata.PayloadID) (uint64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if payloadID == "" {
		return 0, false, nil
	}

	var size uint64
	var found, stale bool
	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyPayloadID(payloadID))
		if errors.Is(err, badgerdb.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var fileID uuid.UUID
		if err := item.Value(func(val []byte) error { return fileID.UnmarshalBinary(val) }); err != nil {
			stale = true
			return nil
		}
		row, err := txn.Get(keyFile(fileID))
		if errors.Is(err, badgerdb.ErrKeyNotFound) {
			stale = true
			return nil
		}
		if err != nil {
			return err
		}
		return row.Value(func(val []byte) error {
			file, err := decodeFile(val)
			if err != nil || file.PayloadID != payloadID {
				stale = true
				return nil
			}
			size, found = file.Size, true
			return nil
		})
	})
	if err != nil {
		return 0, false, err
	}
	if !stale {
		return size, found, nil
	}

	file, err := s.GetFileByPayloadID(ctx, payloadID)
	if err != nil {
		if metadata.IsNotFoundError(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if file == nil {
		return 0, false, nil
	}
	return file.Size, true, nil
}
