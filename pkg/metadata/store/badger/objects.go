package badger

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// FileBlockStore Implementation for BadgerDB Store
// ============================================================================
//
// This file implements the FileBlockStore interface for the BadgerDB metadata store.
// It provides content-addressed file block tracking for deduplication and caching.
//
// Key Prefixes:
//   - fb:{id}          - FileBlock data (keyed by UUID)
//   - fb-hash:{hash}   - Hash index: content hash -> block ID
//
// Thread Safety: All operations use BadgerDB transactions for ACID guarantees.
//
// ============================================================================

const (
	fileBlockPrefix       = "fb:"
	fileBlockHashPrefix   = "fb-hash:"
	fileBlockSealedPrefix = "fb-sealed:"
)

// Ensure BadgerMetadataStore implements FileBlockStore
var _ metadata.FileBlockStore = (*BadgerMetadataStore)(nil)

// ============================================================================
// FileBlock Operations
// ============================================================================

// GetFileBlock retrieves a file block by its ID.
func (s *BadgerMetadataStore) GetFileBlock(ctx context.Context, id string) (*metadata.FileBlock, error) {
	var block metadata.FileBlock
	err := s.db.View(func(txn *badger.Txn) error {
		key := []byte(fileBlockPrefix + id)
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrFileBlockNotFound
		}
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		})
	})
	if err != nil {
		return nil, err
	}
	return &block, nil
}

// PutFileBlock stores or updates a file block.
func (s *BadgerMetadataStore) PutFileBlock(ctx context.Context, block *metadata.FileBlock) error {
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte(fileBlockPrefix + block.ID)
		val, err := json.Marshal(block)
		if err != nil {
			return fmt.Errorf("marshal file block: %w", err)
		}
		if err := txn.Set(key, val); err != nil {
			return err
		}

		// Maintain sealed index: add when Sealed, remove otherwise.
		// This allows ListPendingUpload to iterate O(sealed) instead of O(all).
		sealedKey := []byte(fileBlockSealedPrefix + block.ID)
		if block.State == metadata.BlockStateSealed {
			if err := txn.Set(sealedKey, nil); err != nil {
				return err
			}
		} else {
			_ = txn.Delete(sealedKey) // Ignore ErrKeyNotFound
		}

		// Update hash index for finalized blocks
		if block.IsFinalized() {
			hashKey := []byte(fileBlockHashPrefix + block.Hash.String())
			return txn.Set(hashKey, []byte(block.ID))
		}
		return nil
	})
}

// DeleteFileBlock removes a file block by its ID.
func (s *BadgerMetadataStore) DeleteFileBlock(ctx context.Context, id string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte(fileBlockPrefix + id)

		// Get block to find hash for index cleanup
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrFileBlockNotFound
		}
		if err != nil {
			return err
		}

		var block metadata.FileBlock
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		}); err != nil {
			return err
		}

		// Delete block
		if err := txn.Delete(key); err != nil {
			return err
		}

		// Remove sealed index
		_ = txn.Delete([]byte(fileBlockSealedPrefix + id))

		// Remove hash index
		if block.IsFinalized() {
			hashKey := []byte(fileBlockHashPrefix + block.Hash.String())
			_ = txn.Delete(hashKey) // Ignore if not exists
		}
		return nil
	})
}

// IncrementRefCount atomically increments a block's RefCount.
func (s *BadgerMetadataStore) IncrementRefCount(ctx context.Context, id string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte(fileBlockPrefix + id)
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrFileBlockNotFound
		}
		if err != nil {
			return err
		}
		var block metadata.FileBlock
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		}); err != nil {
			return err
		}
		block.RefCount++
		val, err := json.Marshal(&block)
		if err != nil {
			return err
		}
		return txn.Set(key, val)
	})
}

// DecrementRefCount atomically decrements a block's RefCount.
func (s *BadgerMetadataStore) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	var newCount uint32
	err := s.db.Update(func(txn *badger.Txn) error {
		key := []byte(fileBlockPrefix + id)
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrFileBlockNotFound
		}
		if err != nil {
			return err
		}
		var block metadata.FileBlock
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		}); err != nil {
			return err
		}
		if block.RefCount > 0 {
			block.RefCount--
		}
		newCount = block.RefCount
		val, err := json.Marshal(&block)
		if err != nil {
			return err
		}
		return txn.Set(key, val)
	})
	return newCount, err
}

// FindFileBlockByHash looks up a finalized block by its content hash.
// Returns nil without error if not found.
func (s *BadgerMetadataStore) FindFileBlockByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
	var block metadata.FileBlock
	var found bool
	err := s.db.View(func(txn *badger.Txn) error {
		// Look up ID via hash index
		hashKey := []byte(fileBlockHashPrefix + hash.String())
		hashItem, err := txn.Get(hashKey)
		if err == badger.ErrKeyNotFound {
			return nil // Not found
		}
		if err != nil {
			return err
		}

		var id string
		if err := hashItem.Value(func(val []byte) error {
			id = string(val)
			return nil
		}); err != nil {
			return err
		}

		// Fetch the block
		key := []byte(fileBlockPrefix + id)
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return nil // Index stale, block deleted
		}
		if err != nil {
			return err
		}
		found = true
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		})
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	// Only return uploaded blocks for dedup safety
	if !block.IsUploaded() {
		return nil, nil
	}
	return &block, nil
}

// ListPendingUpload returns blocks that are sealed but not yet uploaded
// and older than the given duration. If limit > 0, at most limit blocks are returned.
//
// Uses the fb-sealed: secondary index for O(sealed) iteration instead of
// scanning all fb: entries. This eliminates the BadgerDB full-table scan
// that was the root cause of sequential write throughput degradation.
func (s *BadgerMetadataStore) ListPendingUpload(ctx context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	cutoff := time.Now().Add(-olderThan)
	var result []*metadata.FileBlock
	err := s.db.View(func(txn *badger.Txn) error {
		prefix := []byte(fileBlockSealedPrefix)
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false // Keys only — values are empty
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			// Extract block ID from key: "fb-sealed:{id}" → "{id}"
			id := string(it.Item().Key()[len(prefix):])

			// Look up the actual FileBlock
			fbItem, err := txn.Get([]byte(fileBlockPrefix + id))
			if err != nil {
				continue // Index stale, block deleted
			}

			var block metadata.FileBlock
			if err := fbItem.Value(func(val []byte) error {
				return json.Unmarshal(val, &block)
			}); err != nil {
				continue
			}

			if block.IsCached() && block.LastAccess.Before(cutoff) {
				result = append(result, &block)
				if limit > 0 && len(result) >= limit {
					break
				}
			}
		}
		return nil
	})
	return result, err
}

// ListEvictable returns blocks that are both cached and uploaded,
// ordered by LRU (oldest LastAccess first), up to limit.
func (s *BadgerMetadataStore) ListEvictable(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	var candidates []*metadata.FileBlock
	err := s.db.View(func(txn *badger.Txn) error {
		prefix := []byte(fileBlockPrefix)
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			var block metadata.FileBlock
			if err := item.Value(func(val []byte) error {
				return json.Unmarshal(val, &block)
			}); err != nil {
				continue
			}
			if block.IsCached() && block.State == metadata.BlockStateUploaded {
				candidates = append(candidates, &block)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Sort by LastAccess (oldest first)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].LastAccess.Before(candidates[j].LastAccess)
	})

	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates, nil
}

// ListUnreferenced returns blocks with RefCount=0, up to limit.
func (s *BadgerMetadataStore) ListUnreferenced(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	var result []*metadata.FileBlock
	err := s.db.View(func(txn *badger.Txn) error {
		prefix := []byte(fileBlockPrefix)
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			var block metadata.FileBlock
			if err := item.Value(func(val []byte) error {
				return json.Unmarshal(val, &block)
			}); err != nil {
				continue
			}
			if block.RefCount == 0 {
				result = append(result, &block)
				if limit > 0 && len(result) >= limit {
					break
				}
			}
		}
		return nil
	})
	return result, err
}

// ============================================================================
// Transaction Support
// ============================================================================

// Ensure badgerTransaction implements FileBlockStore
var _ metadata.FileBlockStore = (*badgerTransaction)(nil)

func (tx *badgerTransaction) GetFileBlock(ctx context.Context, id string) (*metadata.FileBlock, error) {
	return tx.store.GetFileBlock(ctx, id)
}

func (tx *badgerTransaction) PutFileBlock(ctx context.Context, block *metadata.FileBlock) error {
	return tx.store.PutFileBlock(ctx, block)
}

func (tx *badgerTransaction) DeleteFileBlock(ctx context.Context, id string) error {
	return tx.store.DeleteFileBlock(ctx, id)
}

func (tx *badgerTransaction) IncrementRefCount(ctx context.Context, id string) error {
	return tx.store.IncrementRefCount(ctx, id)
}

func (tx *badgerTransaction) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	return tx.store.DecrementRefCount(ctx, id)
}

func (tx *badgerTransaction) FindFileBlockByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
	return tx.store.FindFileBlockByHash(ctx, hash)
}

func (tx *badgerTransaction) ListPendingUpload(ctx context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	return tx.store.ListPendingUpload(ctx, olderThan, limit)
}

func (tx *badgerTransaction) ListEvictable(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	return tx.store.ListEvictable(ctx, limit)
}

func (tx *badgerTransaction) ListUnreferenced(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	return tx.store.ListUnreferenced(ctx, limit)
}
