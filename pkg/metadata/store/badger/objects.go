package badger

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// ObjectStore Implementation for BadgerDB Store
// ============================================================================
//
// This file implements the ObjectStore interface for the BadgerDB metadata store.
// It provides content-addressed object, chunk, and block tracking for deduplication.
//
// Key Prefixes:
//   - obj:{hash}         - Object data
//   - chunk:{hash}       - Chunk data
//   - block:{hash}       - Block data
//   - obj-chunks:{hash}  - Index: object ID -> chunk hashes
//   - chunk-blocks:{hash} - Index: chunk hash -> block hashes
//
// Thread Safety: All operations use BadgerDB transactions for ACID guarantees.
//
// ============================================================================

const (
	objectPrefix      = "obj:"
	chunkPrefix       = "chunk:"
	blockPrefix       = "block:"
	objChunksPrefix   = "obj-chunks:"
	chunkBlocksPrefix = "chunk-blocks:"
)

// Ensure BadgerMetadataStore implements ObjectStore
var _ metadata.ObjectStore = (*BadgerMetadataStore)(nil)

// ============================================================================
// Object Operations
// ============================================================================

// GetObject retrieves an object by its content hash.
func (s *BadgerMetadataStore) GetObject(ctx context.Context, id metadata.ContentHash) (*metadata.Object, error) {
	var obj metadata.Object
	err := s.db.View(func(txn *badger.Txn) error {
		key := []byte(objectPrefix + id.String())
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrObjectNotFound
		}
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &obj)
		})
	})
	if err != nil {
		return nil, err
	}
	return &obj, nil
}

// PutObject stores or updates an object.
func (s *BadgerMetadataStore) PutObject(ctx context.Context, obj *metadata.Object) error {
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte(objectPrefix + obj.ID.String())
		val, err := json.Marshal(obj)
		if err != nil {
			return fmt.Errorf("marshal object: %w", err)
		}
		return txn.Set(key, val)
	})
}

// DeleteObject removes an object by its content hash.
func (s *BadgerMetadataStore) DeleteObject(ctx context.Context, id metadata.ContentHash) error {
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte(objectPrefix + id.String())
		// Check if exists first
		_, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrObjectNotFound
		}
		if err != nil {
			return err
		}
		// Delete object and its chunk index
		if err := txn.Delete(key); err != nil {
			return err
		}
		indexKey := []byte(objChunksPrefix + id.String())
		_ = txn.Delete(indexKey) // Ignore error if index doesn't exist
		return nil
	})
}

// IncrementObjectRefCount atomically increments an object's RefCount.
func (s *BadgerMetadataStore) IncrementObjectRefCount(ctx context.Context, id metadata.ContentHash) (uint32, error) {
	var newCount uint32
	err := s.db.Update(func(txn *badger.Txn) error {
		key := []byte(objectPrefix + id.String())
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrObjectNotFound
		}
		if err != nil {
			return err
		}
		var obj metadata.Object
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &obj)
		}); err != nil {
			return err
		}
		obj.RefCount++
		newCount = obj.RefCount
		val, err := json.Marshal(&obj)
		if err != nil {
			return err
		}
		return txn.Set(key, val)
	})
	return newCount, err
}

// DecrementObjectRefCount atomically decrements an object's RefCount.
func (s *BadgerMetadataStore) DecrementObjectRefCount(ctx context.Context, id metadata.ContentHash) (uint32, error) {
	var newCount uint32
	err := s.db.Update(func(txn *badger.Txn) error {
		key := []byte(objectPrefix + id.String())
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrObjectNotFound
		}
		if err != nil {
			return err
		}
		var obj metadata.Object
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &obj)
		}); err != nil {
			return err
		}
		if obj.RefCount > 0 {
			obj.RefCount--
		}
		newCount = obj.RefCount
		val, err := json.Marshal(&obj)
		if err != nil {
			return err
		}
		return txn.Set(key, val)
	})
	return newCount, err
}

// ============================================================================
// Chunk Operations
// ============================================================================

// GetChunk retrieves a chunk by its content hash.
func (s *BadgerMetadataStore) GetChunk(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectChunk, error) {
	var chunk metadata.ObjectChunk
	err := s.db.View(func(txn *badger.Txn) error {
		key := []byte(chunkPrefix + hash.String())
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrChunkNotFound
		}
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &chunk)
		})
	})
	if err != nil {
		return nil, err
	}
	return &chunk, nil
}

// GetChunksByObject retrieves all chunks for an object, ordered by Index.
func (s *BadgerMetadataStore) GetChunksByObject(ctx context.Context, objectID metadata.ContentHash) ([]*metadata.ObjectChunk, error) {
	var chunks []*metadata.ObjectChunk
	err := s.db.View(func(txn *badger.Txn) error {
		// Get the index
		indexKey := []byte(objChunksPrefix + objectID.String())
		item, err := txn.Get(indexKey)
		if err == badger.ErrKeyNotFound {
			return nil // No chunks for this object
		}
		if err != nil {
			return err
		}

		var hashes []string
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &hashes)
		}); err != nil {
			return err
		}

		// Fetch each chunk
		for _, hashStr := range hashes {
			chunkKey := []byte(chunkPrefix + hashStr)
			chunkItem, err := txn.Get(chunkKey)
			if err != nil {
				continue // Skip missing chunks
			}
			var chunk metadata.ObjectChunk
			if err := chunkItem.Value(func(val []byte) error {
				return json.Unmarshal(val, &chunk)
			}); err != nil {
				continue
			}
			chunks = append(chunks, &chunk)
		}
		return nil
	})
	return chunks, err
}

// PutChunk stores or updates a chunk.
func (s *BadgerMetadataStore) PutChunk(ctx context.Context, chunk *metadata.ObjectChunk) error {
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte(chunkPrefix + chunk.Hash.String())

		// Check if new
		_, err := txn.Get(key)
		isNew := err == badger.ErrKeyNotFound

		// Store chunk
		val, err := json.Marshal(chunk)
		if err != nil {
			return fmt.Errorf("marshal chunk: %w", err)
		}
		if err := txn.Set(key, val); err != nil {
			return err
		}

		// Update index if new
		if isNew {
			indexKey := []byte(objChunksPrefix + chunk.ObjectID.String())
			var hashes []string
			item, err := txn.Get(indexKey)
			if err == nil {
				_ = item.Value(func(val []byte) error {
					return json.Unmarshal(val, &hashes)
				})
			}
			hashes = append(hashes, chunk.Hash.String())
			indexVal, _ := json.Marshal(hashes)
			if err := txn.Set(indexKey, indexVal); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteChunk removes a chunk by its content hash.
func (s *BadgerMetadataStore) DeleteChunk(ctx context.Context, hash metadata.ContentHash) error {
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte(chunkPrefix + hash.String())

		// Get chunk to find object ID
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrChunkNotFound
		}
		if err != nil {
			return err
		}

		var chunk metadata.ObjectChunk
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &chunk)
		}); err != nil {
			return err
		}

		// Delete chunk
		if err := txn.Delete(key); err != nil {
			return err
		}

		// Update index
		indexKey := []byte(objChunksPrefix + chunk.ObjectID.String())
		indexItem, err := txn.Get(indexKey)
		if err == nil {
			var hashes []string
			_ = indexItem.Value(func(val []byte) error {
				return json.Unmarshal(val, &hashes)
			})
			// Remove hash from list
			hashStr := hash.String()
			for i, h := range hashes {
				if h == hashStr {
					hashes = append(hashes[:i], hashes[i+1:]...)
					break
				}
			}
			if len(hashes) == 0 {
				_ = txn.Delete(indexKey)
			} else {
				indexVal, _ := json.Marshal(hashes)
				_ = txn.Set(indexKey, indexVal)
			}
		}

		// Delete block index
		blockIndexKey := []byte(chunkBlocksPrefix + hash.String())
		_ = txn.Delete(blockIndexKey)

		return nil
	})
}

// IncrementChunkRefCount atomically increments a chunk's RefCount.
func (s *BadgerMetadataStore) IncrementChunkRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	var newCount uint32
	err := s.db.Update(func(txn *badger.Txn) error {
		key := []byte(chunkPrefix + hash.String())
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrChunkNotFound
		}
		if err != nil {
			return err
		}
		var chunk metadata.ObjectChunk
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &chunk)
		}); err != nil {
			return err
		}
		chunk.RefCount++
		newCount = chunk.RefCount
		val, err := json.Marshal(&chunk)
		if err != nil {
			return err
		}
		return txn.Set(key, val)
	})
	return newCount, err
}

// DecrementChunkRefCount atomically decrements a chunk's RefCount.
func (s *BadgerMetadataStore) DecrementChunkRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	var newCount uint32
	err := s.db.Update(func(txn *badger.Txn) error {
		key := []byte(chunkPrefix + hash.String())
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrChunkNotFound
		}
		if err != nil {
			return err
		}
		var chunk metadata.ObjectChunk
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &chunk)
		}); err != nil {
			return err
		}
		if chunk.RefCount > 0 {
			chunk.RefCount--
		}
		newCount = chunk.RefCount
		val, err := json.Marshal(&chunk)
		if err != nil {
			return err
		}
		return txn.Set(key, val)
	})
	return newCount, err
}

// ============================================================================
// Block Operations
// ============================================================================

// GetBlock retrieves a block by its content hash.
func (s *BadgerMetadataStore) GetBlock(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectBlock, error) {
	var block metadata.ObjectBlock
	err := s.db.View(func(txn *badger.Txn) error {
		key := []byte(blockPrefix + hash.String())
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrBlockNotFound
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

// GetBlocksByChunk retrieves all blocks for a chunk, ordered by Index.
func (s *BadgerMetadataStore) GetBlocksByChunk(ctx context.Context, chunkHash metadata.ContentHash) ([]*metadata.ObjectBlock, error) {
	var blocks []*metadata.ObjectBlock
	err := s.db.View(func(txn *badger.Txn) error {
		// Get the index
		indexKey := []byte(chunkBlocksPrefix + chunkHash.String())
		item, err := txn.Get(indexKey)
		if err == badger.ErrKeyNotFound {
			return nil // No blocks for this chunk
		}
		if err != nil {
			return err
		}

		var hashes []string
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &hashes)
		}); err != nil {
			return err
		}

		// Fetch each block
		for _, hashStr := range hashes {
			blockKey := []byte(blockPrefix + hashStr)
			blockItem, err := txn.Get(blockKey)
			if err != nil {
				continue // Skip missing blocks
			}
			var block metadata.ObjectBlock
			if err := blockItem.Value(func(val []byte) error {
				return json.Unmarshal(val, &block)
			}); err != nil {
				continue
			}
			blocks = append(blocks, &block)
		}
		return nil
	})
	return blocks, err
}

// PutBlock stores or updates a block.
func (s *BadgerMetadataStore) PutBlock(ctx context.Context, block *metadata.ObjectBlock) error {
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte(blockPrefix + block.Hash.String())

		// Check if new
		_, err := txn.Get(key)
		isNew := err == badger.ErrKeyNotFound

		// Store block
		val, err := json.Marshal(block)
		if err != nil {
			return fmt.Errorf("marshal block: %w", err)
		}
		if err := txn.Set(key, val); err != nil {
			return err
		}

		// Update index if new
		if isNew {
			indexKey := []byte(chunkBlocksPrefix + block.ChunkHash.String())
			var hashes []string
			item, err := txn.Get(indexKey)
			if err == nil {
				_ = item.Value(func(val []byte) error {
					return json.Unmarshal(val, &hashes)
				})
			}
			hashes = append(hashes, block.Hash.String())
			indexVal, _ := json.Marshal(hashes)
			if err := txn.Set(indexKey, indexVal); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteBlock removes a block by its content hash.
func (s *BadgerMetadataStore) DeleteBlock(ctx context.Context, hash metadata.ContentHash) error {
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte(blockPrefix + hash.String())

		// Get block to find chunk hash
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrBlockNotFound
		}
		if err != nil {
			return err
		}

		var block metadata.ObjectBlock
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		}); err != nil {
			return err
		}

		// Delete block
		if err := txn.Delete(key); err != nil {
			return err
		}

		// Update index
		indexKey := []byte(chunkBlocksPrefix + block.ChunkHash.String())
		indexItem, err := txn.Get(indexKey)
		if err == nil {
			var hashes []string
			_ = indexItem.Value(func(val []byte) error {
				return json.Unmarshal(val, &hashes)
			})
			// Remove hash from list
			hashStr := hash.String()
			for i, h := range hashes {
				if h == hashStr {
					hashes = append(hashes[:i], hashes[i+1:]...)
					break
				}
			}
			if len(hashes) == 0 {
				_ = txn.Delete(indexKey)
			} else {
				indexVal, _ := json.Marshal(hashes)
				_ = txn.Set(indexKey, indexVal)
			}
		}

		return nil
	})
}

// FindBlockByHash looks up a block by its content hash.
// Returns nil without error if not found.
func (s *BadgerMetadataStore) FindBlockByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectBlock, error) {
	var block metadata.ObjectBlock
	err := s.db.View(func(txn *badger.Txn) error {
		key := []byte(blockPrefix + hash.String())
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return nil // Not found, but not an error
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
	if block.Hash.IsZero() {
		return nil, nil // Not found
	}
	return &block, nil
}

// IncrementBlockRefCount atomically increments a block's RefCount.
func (s *BadgerMetadataStore) IncrementBlockRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	var newCount uint32
	err := s.db.Update(func(txn *badger.Txn) error {
		key := []byte(blockPrefix + hash.String())
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrBlockNotFound
		}
		if err != nil {
			return err
		}
		var block metadata.ObjectBlock
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		}); err != nil {
			return err
		}
		block.RefCount++
		newCount = block.RefCount
		val, err := json.Marshal(&block)
		if err != nil {
			return err
		}
		return txn.Set(key, val)
	})
	return newCount, err
}

// DecrementBlockRefCount atomically decrements a block's RefCount.
func (s *BadgerMetadataStore) DecrementBlockRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	var newCount uint32
	err := s.db.Update(func(txn *badger.Txn) error {
		key := []byte(blockPrefix + hash.String())
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrBlockNotFound
		}
		if err != nil {
			return err
		}
		var block metadata.ObjectBlock
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

// MarkBlockUploaded marks a block as uploaded to the block store.
func (s *BadgerMetadataStore) MarkBlockUploaded(ctx context.Context, hash metadata.ContentHash) error {
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte(blockPrefix + hash.String())
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrBlockNotFound
		}
		if err != nil {
			return err
		}
		var block metadata.ObjectBlock
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		}); err != nil {
			return err
		}
		block.UploadedAt = time.Now()
		val, err := json.Marshal(&block)
		if err != nil {
			return err
		}
		return txn.Set(key, val)
	})
}

// ============================================================================
// Transaction Support
// ============================================================================

// Ensure badgerTransaction implements ObjectStore
var _ metadata.ObjectStore = (*badgerTransaction)(nil)

// Transaction wrapper methods delegate to store with transaction context

func (tx *badgerTransaction) GetObject(ctx context.Context, id metadata.ContentHash) (*metadata.Object, error) {
	return tx.store.GetObject(ctx, id)
}

func (tx *badgerTransaction) PutObject(ctx context.Context, obj *metadata.Object) error {
	return tx.store.PutObject(ctx, obj)
}

func (tx *badgerTransaction) DeleteObject(ctx context.Context, id metadata.ContentHash) error {
	return tx.store.DeleteObject(ctx, id)
}

func (tx *badgerTransaction) IncrementObjectRefCount(ctx context.Context, id metadata.ContentHash) (uint32, error) {
	return tx.store.IncrementObjectRefCount(ctx, id)
}

func (tx *badgerTransaction) DecrementObjectRefCount(ctx context.Context, id metadata.ContentHash) (uint32, error) {
	return tx.store.DecrementObjectRefCount(ctx, id)
}

func (tx *badgerTransaction) GetChunk(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectChunk, error) {
	return tx.store.GetChunk(ctx, hash)
}

func (tx *badgerTransaction) GetChunksByObject(ctx context.Context, objectID metadata.ContentHash) ([]*metadata.ObjectChunk, error) {
	return tx.store.GetChunksByObject(ctx, objectID)
}

func (tx *badgerTransaction) PutChunk(ctx context.Context, chunk *metadata.ObjectChunk) error {
	return tx.store.PutChunk(ctx, chunk)
}

func (tx *badgerTransaction) DeleteChunk(ctx context.Context, hash metadata.ContentHash) error {
	return tx.store.DeleteChunk(ctx, hash)
}

func (tx *badgerTransaction) IncrementChunkRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	return tx.store.IncrementChunkRefCount(ctx, hash)
}

func (tx *badgerTransaction) DecrementChunkRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	return tx.store.DecrementChunkRefCount(ctx, hash)
}

func (tx *badgerTransaction) GetBlock(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectBlock, error) {
	return tx.store.GetBlock(ctx, hash)
}

func (tx *badgerTransaction) GetBlocksByChunk(ctx context.Context, chunkHash metadata.ContentHash) ([]*metadata.ObjectBlock, error) {
	return tx.store.GetBlocksByChunk(ctx, chunkHash)
}

func (tx *badgerTransaction) PutBlock(ctx context.Context, block *metadata.ObjectBlock) error {
	return tx.store.PutBlock(ctx, block)
}

func (tx *badgerTransaction) DeleteBlock(ctx context.Context, hash metadata.ContentHash) error {
	return tx.store.DeleteBlock(ctx, hash)
}

func (tx *badgerTransaction) FindBlockByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectBlock, error) {
	return tx.store.FindBlockByHash(ctx, hash)
}

func (tx *badgerTransaction) IncrementBlockRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	return tx.store.IncrementBlockRefCount(ctx, hash)
}

func (tx *badgerTransaction) DecrementBlockRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	return tx.store.DecrementBlockRefCount(ctx, hash)
}

func (tx *badgerTransaction) MarkBlockUploaded(ctx context.Context, hash metadata.ContentHash) error {
	return tx.store.MarkBlockUploaded(ctx, hash)
}
