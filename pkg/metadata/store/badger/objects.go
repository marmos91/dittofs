package badger

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/blockstore"
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
	fileBlockPrefix      = "fb:"
	fileBlockHashPrefix  = "fb-hash:"
	fileBlockLocalPrefix = "fb-local:"
	fileBlockFilePrefix  = "fb-file:"
)

// Ensure BadgerMetadataStore implements FileBlockStore
var _ blockstore.FileBlockStore = (*BadgerMetadataStore)(nil)

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

		// Maintain local index: add when Local, remove otherwise.
		// This allows ListLocalBlocks to iterate O(local) instead of O(all).
		localKey := []byte(fileBlockLocalPrefix + block.ID)
		if block.State == metadata.BlockStateLocal {
			if err := txn.Set(localKey, nil); err != nil {
				return err
			}
		} else {
			_ = txn.Delete(localKey) // Ignore ErrKeyNotFound
		}

		// Maintain file index: fb-file:{payloadID}:{blockIdx} -> block.ID
		// This allows ListFileBlocks to iterate O(file_blocks) via prefix scan.
		if parts := strings.SplitN(block.ID, "/", 2); len(parts) == 2 {
			fileKey := []byte(fileBlockFilePrefix + parts[0] + ":" + parts[1])
			if err := txn.Set(fileKey, []byte(block.ID)); err != nil {
				return err
			}
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

		// Remove local index
		_ = txn.Delete([]byte(fileBlockLocalPrefix + id))

		// Remove file index
		if parts := strings.SplitN(id, "/", 2); len(parts) == 2 {
			_ = txn.Delete([]byte(fileBlockFilePrefix + parts[0] + ":" + parts[1]))
		}

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
	// Only return remote blocks for dedup safety
	if !block.IsRemote() {
		return nil, nil
	}
	return &block, nil
}

// ListLocalBlocks returns blocks in Local state (complete, on disk, not yet
// synced to remote) older than the given duration.
// If limit > 0, at most limit blocks are returned.
//
// Uses the fb-local: secondary index for O(local) iteration instead of
// scanning all fb: entries. This eliminates the BadgerDB full-table scan
// that was the root cause of sequential write throughput degradation.
func (s *BadgerMetadataStore) ListLocalBlocks(ctx context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	// olderThan <= 0 means "no age filter" — return every local block.
	var cutoff time.Time
	filterByAge := olderThan > 0
	if filterByAge {
		cutoff = time.Now().Add(-olderThan)
	}
	var result []*metadata.FileBlock
	err := s.db.View(func(txn *badger.Txn) error {
		prefix := []byte(fileBlockLocalPrefix)
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false // Keys only — values are empty
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			// Extract block ID from key: "fb-local:{id}" → "{id}"
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

			if !block.HasLocalFile() {
				continue
			}
			if filterByAge && !block.LastAccess.Before(cutoff) {
				continue
			}
			result = append(result, &block)
			if limit > 0 && len(result) >= limit {
				break
			}
		}
		return nil
	})
	return result, err
}

// ListRemoteBlocks returns blocks that are both cached locally and confirmed
// in remote store, ordered by LRU (oldest LastAccess first), up to limit.
func (s *BadgerMetadataStore) ListRemoteBlocks(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
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
			if block.HasLocalFile() && block.State == metadata.BlockStateRemote {
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

// ListFileBlocks returns all blocks belonging to a file, ordered by block index.
// Uses the fb-file:{payloadID}: secondary index for efficient O(file_blocks) queries.
func (s *BadgerMetadataStore) ListFileBlocks(ctx context.Context, payloadID string) ([]*metadata.FileBlock, error) {
	var result []*metadata.FileBlock
	err := s.db.View(func(txn *badger.Txn) error {
		prefix := []byte(fileBlockFilePrefix + payloadID + ":")
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = true
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			// Value is the block ID
			var blockID string
			if err := it.Item().Value(func(val []byte) error {
				blockID = string(val)
				return nil
			}); err != nil {
				continue
			}

			// Fetch the actual FileBlock
			fbItem, err := txn.Get([]byte(fileBlockPrefix + blockID))
			if err != nil {
				continue // Index stale, block deleted
			}

			var block metadata.FileBlock
			if err := fbItem.Value(func(val []byte) error {
				return json.Unmarshal(val, &block)
			}); err != nil {
				continue
			}
			result = append(result, &block)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Keys are lexicographically sorted (fb-file:{payloadID}:0, :1, :10, :2...)
	// which gives wrong numeric order for multi-digit indices. Sort by parsed index.
	sort.Slice(result, func(i, j int) bool {
		return parseBlockIdx(result[i].ID) < parseBlockIdx(result[j].ID)
	})
	if result == nil {
		return []*metadata.FileBlock{}, nil
	}
	return result, nil
}

// parseBlockIdx extracts the numeric block index from a block ID ("{payloadID}/{blockIdx}").
func parseBlockIdx(id string) int {
	if idx := strings.LastIndex(id, "/"); idx >= 0 {
		var v int
		if _, err := fmt.Sscanf(id[idx+1:], "%d", &v); err == nil {
			return v
		}
	}
	return 0
}

// ============================================================================
// Transaction Support
// ============================================================================

// Ensure badgerTransaction implements FileBlockStore
var _ blockstore.FileBlockStore = (*badgerTransaction)(nil)

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

func (tx *badgerTransaction) ListLocalBlocks(ctx context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	return tx.store.ListLocalBlocks(ctx, olderThan, limit)
}

func (tx *badgerTransaction) ListRemoteBlocks(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	return tx.store.ListRemoteBlocks(ctx, limit)
}

func (tx *badgerTransaction) ListUnreferenced(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	return tx.store.ListUnreferenced(ctx, limit)
}

func (tx *badgerTransaction) ListFileBlocks(ctx context.Context, payloadID string) ([]*metadata.FileBlock, error) {
	return tx.store.ListFileBlocks(ctx, payloadID)
}
