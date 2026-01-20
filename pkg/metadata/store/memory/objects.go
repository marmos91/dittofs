package memory

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// ObjectStore Implementation for Memory Store
// ============================================================================
//
// This file implements the ObjectStore interface for the in-memory metadata store.
// It provides content-addressed object, chunk, and block tracking for deduplication.
//
// Thread Safety: All operations are protected by the store's mutex.
//
// ============================================================================

// objectStoreData holds the in-memory data structures for object tracking.
type objectStoreData struct {
	objects map[metadata.ContentHash]*metadata.Object
	chunks  map[metadata.ContentHash]*metadata.ObjectChunk
	blocks  map[metadata.ContentHash]*metadata.ObjectBlock

	// Index: objectID -> chunk hashes (ordered by Index)
	chunksByObject map[metadata.ContentHash][]metadata.ContentHash

	// Index: chunkHash -> block hashes (ordered by Index)
	blocksByChunk map[metadata.ContentHash][]metadata.ContentHash
}

// newObjectStoreData creates a new objectStoreData instance.
func newObjectStoreData() *objectStoreData {
	return &objectStoreData{
		objects:        make(map[metadata.ContentHash]*metadata.Object),
		chunks:         make(map[metadata.ContentHash]*metadata.ObjectChunk),
		blocks:         make(map[metadata.ContentHash]*metadata.ObjectBlock),
		chunksByObject: make(map[metadata.ContentHash][]metadata.ContentHash),
		blocksByChunk:  make(map[metadata.ContentHash][]metadata.ContentHash),
	}
}

// Ensure Store implements ObjectStore
var _ metadata.ObjectStore = (*MemoryMetadataStore)(nil)

// ============================================================================
// Object Operations
// ============================================================================

// GetObject retrieves an object by its content hash.
func (s *MemoryMetadataStore) GetObject(ctx context.Context, id metadata.ContentHash) (*metadata.Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.objectData == nil {
		return nil, metadata.ErrObjectNotFound
	}

	obj, ok := s.objectData.objects[id]
	if !ok {
		return nil, metadata.ErrObjectNotFound
	}

	// Return a copy to prevent external modification
	result := *obj
	return &result, nil
}

// PutObject stores or updates an object.
func (s *MemoryMetadataStore) PutObject(ctx context.Context, obj *metadata.Object) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.initObjectData()

	// Store a copy
	stored := *obj
	s.objectData.objects[obj.ID] = &stored
	return nil
}

// DeleteObject removes an object by its content hash.
func (s *MemoryMetadataStore) DeleteObject(ctx context.Context, id metadata.ContentHash) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.objectData == nil {
		return metadata.ErrObjectNotFound
	}

	if _, ok := s.objectData.objects[id]; !ok {
		return metadata.ErrObjectNotFound
	}

	delete(s.objectData.objects, id)
	delete(s.objectData.chunksByObject, id)
	return nil
}

// IncrementObjectRefCount atomically increments an object's RefCount.
func (s *MemoryMetadataStore) IncrementObjectRefCount(ctx context.Context, id metadata.ContentHash) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.objectData == nil {
		return 0, metadata.ErrObjectNotFound
	}

	obj, ok := s.objectData.objects[id]
	if !ok {
		return 0, metadata.ErrObjectNotFound
	}

	obj.RefCount++
	return obj.RefCount, nil
}

// DecrementObjectRefCount atomically decrements an object's RefCount.
func (s *MemoryMetadataStore) DecrementObjectRefCount(ctx context.Context, id metadata.ContentHash) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.objectData == nil {
		return 0, metadata.ErrObjectNotFound
	}

	obj, ok := s.objectData.objects[id]
	if !ok {
		return 0, metadata.ErrObjectNotFound
	}

	if obj.RefCount > 0 {
		obj.RefCount--
	}
	return obj.RefCount, nil
}

// ============================================================================
// Chunk Operations
// ============================================================================

// GetChunk retrieves a chunk by its content hash.
func (s *MemoryMetadataStore) GetChunk(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectChunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.objectData == nil {
		return nil, metadata.ErrChunkNotFound
	}

	chunk, ok := s.objectData.chunks[hash]
	if !ok {
		return nil, metadata.ErrChunkNotFound
	}

	// Return a copy
	result := *chunk
	return &result, nil
}

// GetChunksByObject retrieves all chunks for an object, ordered by Index.
func (s *MemoryMetadataStore) GetChunksByObject(ctx context.Context, objectID metadata.ContentHash) ([]*metadata.ObjectChunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.objectData == nil {
		return nil, nil
	}

	hashes, ok := s.objectData.chunksByObject[objectID]
	if !ok {
		return nil, nil
	}

	result := make([]*metadata.ObjectChunk, 0, len(hashes))
	for _, h := range hashes {
		if chunk, ok := s.objectData.chunks[h]; ok {
			c := *chunk
			result = append(result, &c)
		}
	}
	return result, nil
}

// PutChunk stores or updates a chunk.
func (s *MemoryMetadataStore) PutChunk(ctx context.Context, chunk *metadata.ObjectChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.initObjectData()

	// Check if this is a new chunk for this object
	_, exists := s.objectData.chunks[chunk.Hash]

	// Store a copy
	stored := *chunk
	s.objectData.chunks[chunk.Hash] = &stored

	// Update the index if this is a new chunk
	if !exists {
		hashes := s.objectData.chunksByObject[chunk.ObjectID]
		// Insert at correct position based on Index
		inserted := false
		for i, h := range hashes {
			if existing, ok := s.objectData.chunks[h]; ok && existing.Index > chunk.Index {
				// Insert before this position
				hashes = append(hashes[:i], append([]metadata.ContentHash{chunk.Hash}, hashes[i:]...)...)
				inserted = true
				break
			}
		}
		if !inserted {
			hashes = append(hashes, chunk.Hash)
		}
		s.objectData.chunksByObject[chunk.ObjectID] = hashes
	}

	return nil
}

// DeleteChunk removes a chunk by its content hash.
func (s *MemoryMetadataStore) DeleteChunk(ctx context.Context, hash metadata.ContentHash) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.objectData == nil {
		return metadata.ErrChunkNotFound
	}

	chunk, ok := s.objectData.chunks[hash]
	if !ok {
		return metadata.ErrChunkNotFound
	}

	// Remove from index
	if hashes, ok := s.objectData.chunksByObject[chunk.ObjectID]; ok {
		for i, h := range hashes {
			if h == hash {
				s.objectData.chunksByObject[chunk.ObjectID] = append(hashes[:i], hashes[i+1:]...)
				break
			}
		}
	}

	delete(s.objectData.chunks, hash)
	delete(s.objectData.blocksByChunk, hash)
	return nil
}

// IncrementChunkRefCount atomically increments a chunk's RefCount.
func (s *MemoryMetadataStore) IncrementChunkRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.objectData == nil {
		return 0, metadata.ErrChunkNotFound
	}

	chunk, ok := s.objectData.chunks[hash]
	if !ok {
		return 0, metadata.ErrChunkNotFound
	}

	chunk.RefCount++
	return chunk.RefCount, nil
}

// DecrementChunkRefCount atomically decrements a chunk's RefCount.
func (s *MemoryMetadataStore) DecrementChunkRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.objectData == nil {
		return 0, metadata.ErrChunkNotFound
	}

	chunk, ok := s.objectData.chunks[hash]
	if !ok {
		return 0, metadata.ErrChunkNotFound
	}

	if chunk.RefCount > 0 {
		chunk.RefCount--
	}
	return chunk.RefCount, nil
}

// ============================================================================
// Block Operations
// ============================================================================

// GetBlock retrieves a block by its content hash.
func (s *MemoryMetadataStore) GetBlock(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectBlock, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.objectData == nil {
		return nil, metadata.ErrBlockNotFound
	}

	block, ok := s.objectData.blocks[hash]
	if !ok {
		return nil, metadata.ErrBlockNotFound
	}

	// Return a copy
	result := *block
	return &result, nil
}

// GetBlocksByChunk retrieves all blocks for a chunk, ordered by Index.
func (s *MemoryMetadataStore) GetBlocksByChunk(ctx context.Context, chunkHash metadata.ContentHash) ([]*metadata.ObjectBlock, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.objectData == nil {
		return nil, nil
	}

	hashes, ok := s.objectData.blocksByChunk[chunkHash]
	if !ok {
		return nil, nil
	}

	result := make([]*metadata.ObjectBlock, 0, len(hashes))
	for _, h := range hashes {
		if block, ok := s.objectData.blocks[h]; ok {
			b := *block
			result = append(result, &b)
		}
	}
	return result, nil
}

// PutBlock stores or updates a block.
func (s *MemoryMetadataStore) PutBlock(ctx context.Context, block *metadata.ObjectBlock) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.initObjectData()

	// Check if this is a new block for this chunk
	_, exists := s.objectData.blocks[block.Hash]

	// Store a copy
	stored := *block
	s.objectData.blocks[block.Hash] = &stored

	// Update the index if this is a new block
	if !exists {
		hashes := s.objectData.blocksByChunk[block.ChunkHash]
		// Insert at correct position based on Index
		inserted := false
		for i, h := range hashes {
			if existing, ok := s.objectData.blocks[h]; ok && existing.Index > block.Index {
				// Insert before this position
				hashes = append(hashes[:i], append([]metadata.ContentHash{block.Hash}, hashes[i:]...)...)
				inserted = true
				break
			}
		}
		if !inserted {
			hashes = append(hashes, block.Hash)
		}
		s.objectData.blocksByChunk[block.ChunkHash] = hashes
	}

	return nil
}

// DeleteBlock removes a block by its content hash.
func (s *MemoryMetadataStore) DeleteBlock(ctx context.Context, hash metadata.ContentHash) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.objectData == nil {
		return metadata.ErrBlockNotFound
	}

	block, ok := s.objectData.blocks[hash]
	if !ok {
		return metadata.ErrBlockNotFound
	}

	// Remove from index
	if hashes, ok := s.objectData.blocksByChunk[block.ChunkHash]; ok {
		for i, h := range hashes {
			if h == hash {
				s.objectData.blocksByChunk[block.ChunkHash] = append(hashes[:i], hashes[i+1:]...)
				break
			}
		}
	}

	delete(s.objectData.blocks, hash)
	return nil
}

// FindBlockByHash looks up a block by its content hash.
// Returns nil without error if not found.
func (s *MemoryMetadataStore) FindBlockByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectBlock, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.objectData == nil {
		return nil, nil
	}

	block, ok := s.objectData.blocks[hash]
	if !ok {
		return nil, nil
	}

	// Return a copy
	result := *block
	return &result, nil
}

// IncrementBlockRefCount atomically increments a block's RefCount.
func (s *MemoryMetadataStore) IncrementBlockRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.objectData == nil {
		return 0, metadata.ErrBlockNotFound
	}

	block, ok := s.objectData.blocks[hash]
	if !ok {
		return 0, metadata.ErrBlockNotFound
	}

	block.RefCount++
	return block.RefCount, nil
}

// DecrementBlockRefCount atomically decrements a block's RefCount.
func (s *MemoryMetadataStore) DecrementBlockRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.objectData == nil {
		return 0, metadata.ErrBlockNotFound
	}

	block, ok := s.objectData.blocks[hash]
	if !ok {
		return 0, metadata.ErrBlockNotFound
	}

	if block.RefCount > 0 {
		block.RefCount--
	}
	return block.RefCount, nil
}

// MarkBlockUploaded marks a block as uploaded to the block store.
func (s *MemoryMetadataStore) MarkBlockUploaded(ctx context.Context, hash metadata.ContentHash) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.objectData == nil {
		return metadata.ErrBlockNotFound
	}

	block, ok := s.objectData.blocks[hash]
	if !ok {
		return metadata.ErrBlockNotFound
	}

	block.MarkUploaded()
	return nil
}

// ============================================================================
// Helper Methods
// ============================================================================

// initObjectData initializes the objectStoreData if needed.
// Must be called with the write lock held.
func (s *MemoryMetadataStore) initObjectData() {
	if s.objectData == nil {
		s.objectData = newObjectStoreData()
	}
}

// ============================================================================
// Transaction Support
// ============================================================================

// The memoryTransaction type needs to implement ObjectStore as well.
// We'll delegate to the store's methods since memory store uses a single mutex.

// Ensure memoryTransaction implements ObjectStore
var _ metadata.ObjectStore = (*memoryTransaction)(nil)

// memoryTransactionObjectStore provides ObjectStore methods for memoryTransaction.
// These delegate to the parent store since memory transactions use the store's mutex.

func (tx *memoryTransaction) GetObject(ctx context.Context, id metadata.ContentHash) (*metadata.Object, error) {
	return tx.store.getObjectLocked(ctx, id)
}

func (tx *memoryTransaction) PutObject(ctx context.Context, obj *metadata.Object) error {
	return tx.store.putObjectLocked(ctx, obj)
}

func (tx *memoryTransaction) DeleteObject(ctx context.Context, id metadata.ContentHash) error {
	return tx.store.deleteObjectLocked(ctx, id)
}

func (tx *memoryTransaction) IncrementObjectRefCount(ctx context.Context, id metadata.ContentHash) (uint32, error) {
	return tx.store.incrementObjectRefCountLocked(ctx, id)
}

func (tx *memoryTransaction) DecrementObjectRefCount(ctx context.Context, id metadata.ContentHash) (uint32, error) {
	return tx.store.decrementObjectRefCountLocked(ctx, id)
}

func (tx *memoryTransaction) GetChunk(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectChunk, error) {
	return tx.store.getChunkLocked(ctx, hash)
}

func (tx *memoryTransaction) GetChunksByObject(ctx context.Context, objectID metadata.ContentHash) ([]*metadata.ObjectChunk, error) {
	return tx.store.getChunksByObjectLocked(ctx, objectID)
}

func (tx *memoryTransaction) PutChunk(ctx context.Context, chunk *metadata.ObjectChunk) error {
	return tx.store.putChunkLocked(ctx, chunk)
}

func (tx *memoryTransaction) DeleteChunk(ctx context.Context, hash metadata.ContentHash) error {
	return tx.store.deleteChunkLocked(ctx, hash)
}

func (tx *memoryTransaction) IncrementChunkRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	return tx.store.incrementChunkRefCountLocked(ctx, hash)
}

func (tx *memoryTransaction) DecrementChunkRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	return tx.store.decrementChunkRefCountLocked(ctx, hash)
}

func (tx *memoryTransaction) GetBlock(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectBlock, error) {
	return tx.store.getBlockLocked(ctx, hash)
}

func (tx *memoryTransaction) GetBlocksByChunk(ctx context.Context, chunkHash metadata.ContentHash) ([]*metadata.ObjectBlock, error) {
	return tx.store.getBlocksByChunkLocked(ctx, chunkHash)
}

func (tx *memoryTransaction) PutBlock(ctx context.Context, block *metadata.ObjectBlock) error {
	return tx.store.putBlockLocked(ctx, block)
}

func (tx *memoryTransaction) DeleteBlock(ctx context.Context, hash metadata.ContentHash) error {
	return tx.store.deleteBlockLocked(ctx, hash)
}

func (tx *memoryTransaction) FindBlockByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectBlock, error) {
	return tx.store.findBlockByHashLocked(ctx, hash)
}

func (tx *memoryTransaction) IncrementBlockRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	return tx.store.incrementBlockRefCountLocked(ctx, hash)
}

func (tx *memoryTransaction) DecrementBlockRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	return tx.store.decrementBlockRefCountLocked(ctx, hash)
}

func (tx *memoryTransaction) MarkBlockUploaded(ctx context.Context, hash metadata.ContentHash) error {
	return tx.store.markBlockUploadedLocked(ctx, hash)
}

// ============================================================================
// Locked Helpers (for transaction support)
// ============================================================================

// These methods assume the lock is already held (called from transaction context).

func (s *MemoryMetadataStore) getObjectLocked(_ context.Context, id metadata.ContentHash) (*metadata.Object, error) {
	if s.objectData == nil {
		return nil, metadata.ErrObjectNotFound
	}
	obj, ok := s.objectData.objects[id]
	if !ok {
		return nil, metadata.ErrObjectNotFound
	}
	result := *obj
	return &result, nil
}

func (s *MemoryMetadataStore) putObjectLocked(_ context.Context, obj *metadata.Object) error {
	s.initObjectData()
	stored := *obj
	s.objectData.objects[obj.ID] = &stored
	return nil
}

func (s *MemoryMetadataStore) deleteObjectLocked(_ context.Context, id metadata.ContentHash) error {
	if s.objectData == nil {
		return metadata.ErrObjectNotFound
	}
	if _, ok := s.objectData.objects[id]; !ok {
		return metadata.ErrObjectNotFound
	}
	delete(s.objectData.objects, id)
	delete(s.objectData.chunksByObject, id)
	return nil
}

func (s *MemoryMetadataStore) incrementObjectRefCountLocked(_ context.Context, id metadata.ContentHash) (uint32, error) {
	if s.objectData == nil {
		return 0, metadata.ErrObjectNotFound
	}
	obj, ok := s.objectData.objects[id]
	if !ok {
		return 0, metadata.ErrObjectNotFound
	}
	obj.RefCount++
	return obj.RefCount, nil
}

func (s *MemoryMetadataStore) decrementObjectRefCountLocked(_ context.Context, id metadata.ContentHash) (uint32, error) {
	if s.objectData == nil {
		return 0, metadata.ErrObjectNotFound
	}
	obj, ok := s.objectData.objects[id]
	if !ok {
		return 0, metadata.ErrObjectNotFound
	}
	if obj.RefCount > 0 {
		obj.RefCount--
	}
	return obj.RefCount, nil
}

func (s *MemoryMetadataStore) getChunkLocked(_ context.Context, hash metadata.ContentHash) (*metadata.ObjectChunk, error) {
	if s.objectData == nil {
		return nil, metadata.ErrChunkNotFound
	}
	chunk, ok := s.objectData.chunks[hash]
	if !ok {
		return nil, metadata.ErrChunkNotFound
	}
	result := *chunk
	return &result, nil
}

func (s *MemoryMetadataStore) getChunksByObjectLocked(_ context.Context, objectID metadata.ContentHash) ([]*metadata.ObjectChunk, error) {
	if s.objectData == nil {
		return nil, nil
	}
	hashes, ok := s.objectData.chunksByObject[objectID]
	if !ok {
		return nil, nil
	}
	result := make([]*metadata.ObjectChunk, 0, len(hashes))
	for _, h := range hashes {
		if chunk, ok := s.objectData.chunks[h]; ok {
			c := *chunk
			result = append(result, &c)
		}
	}
	return result, nil
}

func (s *MemoryMetadataStore) putChunkLocked(_ context.Context, chunk *metadata.ObjectChunk) error {
	s.initObjectData()
	_, exists := s.objectData.chunks[chunk.Hash]
	stored := *chunk
	s.objectData.chunks[chunk.Hash] = &stored
	if !exists {
		hashes := s.objectData.chunksByObject[chunk.ObjectID]
		inserted := false
		for i, h := range hashes {
			if existing, ok := s.objectData.chunks[h]; ok && existing.Index > chunk.Index {
				hashes = append(hashes[:i], append([]metadata.ContentHash{chunk.Hash}, hashes[i:]...)...)
				inserted = true
				break
			}
		}
		if !inserted {
			hashes = append(hashes, chunk.Hash)
		}
		s.objectData.chunksByObject[chunk.ObjectID] = hashes
	}
	return nil
}

func (s *MemoryMetadataStore) deleteChunkLocked(_ context.Context, hash metadata.ContentHash) error {
	if s.objectData == nil {
		return metadata.ErrChunkNotFound
	}
	chunk, ok := s.objectData.chunks[hash]
	if !ok {
		return metadata.ErrChunkNotFound
	}
	if hashes, ok := s.objectData.chunksByObject[chunk.ObjectID]; ok {
		for i, h := range hashes {
			if h == hash {
				s.objectData.chunksByObject[chunk.ObjectID] = append(hashes[:i], hashes[i+1:]...)
				break
			}
		}
	}
	delete(s.objectData.chunks, hash)
	delete(s.objectData.blocksByChunk, hash)
	return nil
}

func (s *MemoryMetadataStore) incrementChunkRefCountLocked(_ context.Context, hash metadata.ContentHash) (uint32, error) {
	if s.objectData == nil {
		return 0, metadata.ErrChunkNotFound
	}
	chunk, ok := s.objectData.chunks[hash]
	if !ok {
		return 0, metadata.ErrChunkNotFound
	}
	chunk.RefCount++
	return chunk.RefCount, nil
}

func (s *MemoryMetadataStore) decrementChunkRefCountLocked(_ context.Context, hash metadata.ContentHash) (uint32, error) {
	if s.objectData == nil {
		return 0, metadata.ErrChunkNotFound
	}
	chunk, ok := s.objectData.chunks[hash]
	if !ok {
		return 0, metadata.ErrChunkNotFound
	}
	if chunk.RefCount > 0 {
		chunk.RefCount--
	}
	return chunk.RefCount, nil
}

func (s *MemoryMetadataStore) getBlockLocked(_ context.Context, hash metadata.ContentHash) (*metadata.ObjectBlock, error) {
	if s.objectData == nil {
		return nil, metadata.ErrBlockNotFound
	}
	block, ok := s.objectData.blocks[hash]
	if !ok {
		return nil, metadata.ErrBlockNotFound
	}
	result := *block
	return &result, nil
}

func (s *MemoryMetadataStore) getBlocksByChunkLocked(_ context.Context, chunkHash metadata.ContentHash) ([]*metadata.ObjectBlock, error) {
	if s.objectData == nil {
		return nil, nil
	}
	hashes, ok := s.objectData.blocksByChunk[chunkHash]
	if !ok {
		return nil, nil
	}
	result := make([]*metadata.ObjectBlock, 0, len(hashes))
	for _, h := range hashes {
		if block, ok := s.objectData.blocks[h]; ok {
			b := *block
			result = append(result, &b)
		}
	}
	return result, nil
}

func (s *MemoryMetadataStore) putBlockLocked(_ context.Context, block *metadata.ObjectBlock) error {
	s.initObjectData()
	_, exists := s.objectData.blocks[block.Hash]
	stored := *block
	s.objectData.blocks[block.Hash] = &stored
	if !exists {
		hashes := s.objectData.blocksByChunk[block.ChunkHash]
		inserted := false
		for i, h := range hashes {
			if existing, ok := s.objectData.blocks[h]; ok && existing.Index > block.Index {
				hashes = append(hashes[:i], append([]metadata.ContentHash{block.Hash}, hashes[i:]...)...)
				inserted = true
				break
			}
		}
		if !inserted {
			hashes = append(hashes, block.Hash)
		}
		s.objectData.blocksByChunk[block.ChunkHash] = hashes
	}
	return nil
}

func (s *MemoryMetadataStore) deleteBlockLocked(_ context.Context, hash metadata.ContentHash) error {
	if s.objectData == nil {
		return metadata.ErrBlockNotFound
	}
	block, ok := s.objectData.blocks[hash]
	if !ok {
		return metadata.ErrBlockNotFound
	}
	if hashes, ok := s.objectData.blocksByChunk[block.ChunkHash]; ok {
		for i, h := range hashes {
			if h == hash {
				s.objectData.blocksByChunk[block.ChunkHash] = append(hashes[:i], hashes[i+1:]...)
				break
			}
		}
	}
	delete(s.objectData.blocks, hash)
	return nil
}

func (s *MemoryMetadataStore) findBlockByHashLocked(_ context.Context, hash metadata.ContentHash) (*metadata.ObjectBlock, error) {
	if s.objectData == nil {
		return nil, nil
	}
	block, ok := s.objectData.blocks[hash]
	if !ok {
		return nil, nil
	}
	result := *block
	return &result, nil
}

func (s *MemoryMetadataStore) incrementBlockRefCountLocked(_ context.Context, hash metadata.ContentHash) (uint32, error) {
	if s.objectData == nil {
		return 0, metadata.ErrBlockNotFound
	}
	block, ok := s.objectData.blocks[hash]
	if !ok {
		return 0, metadata.ErrBlockNotFound
	}
	block.RefCount++
	return block.RefCount, nil
}

func (s *MemoryMetadataStore) decrementBlockRefCountLocked(_ context.Context, hash metadata.ContentHash) (uint32, error) {
	if s.objectData == nil {
		return 0, metadata.ErrBlockNotFound
	}
	block, ok := s.objectData.blocks[hash]
	if !ok {
		return 0, metadata.ErrBlockNotFound
	}
	if block.RefCount > 0 {
		block.RefCount--
	}
	return block.RefCount, nil
}

func (s *MemoryMetadataStore) markBlockUploadedLocked(_ context.Context, hash metadata.ContentHash) error {
	if s.objectData == nil {
		return metadata.ErrBlockNotFound
	}
	block, ok := s.objectData.blocks[hash]
	if !ok {
		return metadata.ErrBlockNotFound
	}
	block.MarkUploaded()
	return nil
}

