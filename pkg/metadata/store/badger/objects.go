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
// Phase 12 (META-03 / D-09): the FileBlockStore interface narrowed to 6
// methods. The backend retains the legacy GetFileBlock + ListFileBlocks
// helpers as concrete methods on the struct (not on the public interface)
// for engine-internal callers.
//
// Key Prefixes:
//   - fb:{id}          - FileBlock data (keyed by UUID)
//   - fb-hash:{hash}   - Hash index: content hash -> block ID
//   - fb-local:{id}    - Local-state secondary index for ListPending
//   - fb-file:{pid}:{n}- Per-file secondary index for ListFileBlocks
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

// GetFileBlock retrieves a file block by its ID. Not on the narrowed
// FileBlockStore interface (Phase 12 META-03 / D-09); kept as a backend
// method for engine-internal callers.
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

// Put stores or updates a file block. Renamed from PutFileBlock in
// Phase 12 (META-03 / D-09) to match the narrowed interface.
func (s *BadgerMetadataStore) Put(ctx context.Context, block *metadata.FileBlock) error {
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte(fileBlockPrefix + block.ID)
		val, err := json.Marshal(block)
		if err != nil {
			return fmt.Errorf("marshal file block: %w", err)
		}
		if err := txn.Set(key, val); err != nil {
			return err
		}

		// Maintain local index: add when Pending, remove otherwise.
		// This allows ListPending to iterate O(local) instead of O(all).
		localKey := []byte(fileBlockLocalPrefix + block.ID)
		if block.State == metadata.BlockStatePending {
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

// Delete removes a file block by its ID. Renamed from DeleteFileBlock in
// Phase 12 (META-03 / D-09).
func (s *BadgerMetadataStore) Delete(ctx context.Context, id string) error {
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

// GetByHash looks up a finalized block by its content hash.
// Returns nil without error if not found. Renamed from FindFileBlockByHash
// in Phase 12 (META-03 / D-09).
func (s *BadgerMetadataStore) GetByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
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

// ListPending returns blocks in Pending state (complete, on disk, not yet
// synced to remote) older than the given duration. Renamed from
// ListLocalBlocks in Phase 12 (META-03 / D-09); the underlying semantics
// already match Phase 11 STATE-01 ("Local" was renamed Pending).
// If limit > 0, at most limit blocks are returned.
//
// Uses the fb-local: secondary index for O(local) iteration instead of
// scanning all fb: entries. This eliminates the BadgerDB full-table scan
// that was the root cause of sequential write throughput degradation.
func (s *BadgerMetadataStore) ListPending(ctx context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
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

// ListFileBlocks returns all blocks belonging to a file, ordered by block index.
// Uses the fb-file:{payloadID}: secondary index for efficient O(file_blocks) queries.
// Not on the narrowed FileBlockStore interface (Phase 12 META-03 / D-09);
// kept as a backend method for engine-internal callers.
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

// EnumerateFileBlocks streams every FileBlock's ContentHash through fn using
// a Badger prefix iterator over fb:. The iterator yields one row per block
// (no allocation of a full slice in application memory). See GC-01 / D-02.
// Phase 12 (META-03 / D-08): lifted from FileBlockStore to MetadataStore —
// implementation unchanged.
func (s *BadgerMetadataStore) EnumerateFileBlocks(ctx context.Context, fn func(blockstore.ContentHash) error) error {
	return s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = true
		opts.PrefetchSize = 256
		prefix := []byte(fileBlockPrefix)
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("enumerate file blocks: %w", err)
			}
			var block metadata.FileBlock
			if err := it.Item().Value(func(val []byte) error {
				return json.Unmarshal(val, &block)
			}); err != nil {
				return fmt.Errorf("decode file block: %w", err)
			}
			if err := fn(block.Hash); err != nil {
				return err
			}
		}
		return nil
	})
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

func (tx *badgerTransaction) Put(ctx context.Context, block *metadata.FileBlock) error {
	return tx.store.Put(ctx, block)
}

func (tx *badgerTransaction) Delete(ctx context.Context, id string) error {
	return tx.store.Delete(ctx, id)
}

func (tx *badgerTransaction) IncrementRefCount(ctx context.Context, id string) error {
	return tx.store.IncrementRefCount(ctx, id)
}

func (tx *badgerTransaction) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	return tx.store.DecrementRefCount(ctx, id)
}

func (tx *badgerTransaction) GetByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
	return tx.store.GetByHash(ctx, hash)
}

func (tx *badgerTransaction) ListPending(ctx context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	return tx.store.ListPending(ctx, olderThan, limit)
}

func (tx *badgerTransaction) ListFileBlocks(ctx context.Context, payloadID string) ([]*metadata.FileBlock, error) {
	return tx.store.ListFileBlocks(ctx, payloadID)
}

func (tx *badgerTransaction) EnumerateFileBlocks(ctx context.Context, fn func(blockstore.ContentHash) error) error {
	return tx.store.EnumerateFileBlocks(ctx, fn)
}
