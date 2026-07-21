package badger

import (
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
		if cached, ok := s.readCache.get(key); ok {
			return copyForRead(cached), nil
		}
	}

	// Snapshot the invalidation generation BEFORE the backing read so a write
	// that races this read cannot leave a stale value cached (store() checks it).
	gen := s.readCache.generation()
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
		s.readCache.store(key, result, gen)
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

// loadManifest populates file.Blocks from the fm:<uuid> manifest key. A legacy
// f: blob that still embeds the manifest arrives with Blocks already set and is
// left untouched (the next write migrates it to fm:); new-format blobs carry no
// inline manifest, so the chunk list is read from its sibling key. A missing
// fm: key means an empty manifest (directory, symlink, or empty regular file).
func loadManifest(txn *badgerdb.Txn, file *metadata.File) error {
	if len(file.Blocks) > 0 {
		return nil // legacy embedded manifest — authoritative for this row
	}
	item, err := txn.Get(keyFileManifest(file.ID))
	if errors.Is(err, badgerdb.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return item.Value(func(val []byte) error {
		blocks, derr := decodeManifest(val)
		if derr != nil {
			return derr
		}
		file.Blocks = blocks
		return nil
	})
}

// putManifest persists (or, when empty, removes) the fm:<uuid> block manifest.
// An empty manifest — a truncated/empty regular file, a directory, or a symlink
// — carries no key, so loadManifest reads a missing key as "no blocks". This
// keeps the manifest coherent when a truncate prunes every chunk.
func (tx *badgerTransaction) putManifest(id uuid.UUID, blocks []block.ChunkRef) error {
	if len(blocks) == 0 {
		if err := tx.txn.Delete(keyFileManifest(id)); err != nil && !errors.Is(err, badgerdb.ErrKeyNotFound) {
			return err
		}
		return nil
	}
	data, err := encodeManifest(blocks)
	if err != nil {
		return err
	}
	return tx.txn.Set(keyFileManifest(id), data)
}

// PutFile stores or updates file metadata.
func (s *BadgerMetadataStore) PutFile(ctx context.Context, file *metadata.File) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutFile(ctx, file)
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
			err := item.Value(func(val []byte) error {
				file, err := decodeFile(val)
				if err != nil {
					return nil // Skip corrupted entries
				}

				if file.PayloadID == payloadID {
					// Look up link count for this file
					linkItem, linkErr := txn.Get(keyLinkCount(file.ID))
					switch linkErr {
					case nil:
						_ = linkItem.Value(func(linkVal []byte) error {
							count, countErr := decodeUint32(linkVal)
							if countErr == nil {
								file.Nlink = count
							}
							return nil
						})
					case badgerdb.ErrKeyNotFound:
						// Default based on file type
						if file.Type == metadata.FileTypeDirectory {
							file.Nlink = 2
						} else {
							file.Nlink = 1
						}
					}
					// Derive Path from the parent keyspace (#1166) so a
					// rename/relink can never surface a stale stored path.
					btx := &badgerTransaction{store: s, txn: txn}
					file.Path = btx.derivePath(file.ID)
					if err := loadManifest(txn, file); err != nil {
						return err
					}
					result = file
					return errFound
				}
				return nil
			})

			if err == errFound {
				return nil
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
func (s *BadgerMetadataStore) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int) ([]metadata.DirEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

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
