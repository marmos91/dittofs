package memory

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// FileBlockStore Implementation for Memory Store
// ============================================================================
//
// This file implements the FileBlockStore interface for the in-memory metadata store.
// It provides content-addressed file block tracking for deduplication and caching.
//
// Thread Safety: All operations are protected by the store's mutex.
//
// ============================================================================

// fileBlockStoreData holds the in-memory data structures for file block tracking.
type fileBlockStoreData struct {
	blocks map[string]*metadata.FileBlock // ID -> FileBlock

	// hashIndex maps content hash -> block ID for dedup lookups.
	// Only populated for finalized blocks (non-zero hash).
	hashIndex map[metadata.ContentHash]string
}

// newFileBlockStoreData creates a new fileBlockStoreData instance.
func newFileBlockStoreData() *fileBlockStoreData {
	return &fileBlockStoreData{
		blocks:    make(map[string]*metadata.FileBlock),
		hashIndex: make(map[metadata.ContentHash]string),
	}
}

// Ensure Store implements FileBlockStore
var _ metadata.FileBlockStore = (*MemoryMetadataStore)(nil)

// ============================================================================
// FileBlock Operations
// ============================================================================

// GetFileBlock retrieves a file block by its ID.
func (s *MemoryMetadataStore) GetFileBlock(ctx context.Context, id string) (*metadata.FileBlock, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getFileBlockLocked(ctx, id)
}

// PutFileBlock stores or updates a file block.
func (s *MemoryMetadataStore) PutFileBlock(ctx context.Context, block *metadata.FileBlock) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putFileBlockLocked(ctx, block)
}

// DeleteFileBlock removes a file block by its ID.
func (s *MemoryMetadataStore) DeleteFileBlock(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteFileBlockLocked(ctx, id)
}

// IncrementRefCount atomically increments a block's RefCount.
func (s *MemoryMetadataStore) IncrementRefCount(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.incrementRefCountLocked(ctx, id)
}

// DecrementRefCount atomically decrements a block's RefCount.
func (s *MemoryMetadataStore) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.decrementRefCountLocked(ctx, id)
}

// FindFileBlockByHash looks up a finalized block by its content hash.
// Returns nil without error if not found.
func (s *MemoryMetadataStore) FindFileBlockByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findFileBlockByHashLocked(ctx, hash)
}

// ListPendingUpload returns blocks that are finalized but not yet uploaded
// and older than the given duration. If limit > 0, at most limit blocks are returned.
func (s *MemoryMetadataStore) ListPendingUpload(ctx context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listPendingUploadLocked(ctx, olderThan, limit)
}

// ListEvictable returns blocks that are both cached and uploaded,
// ordered by LRU (oldest LastAccess first), up to limit.
func (s *MemoryMetadataStore) ListEvictable(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listEvictableLocked(ctx, limit)
}

// ListUnreferenced returns blocks with RefCount=0, up to limit.
func (s *MemoryMetadataStore) ListUnreferenced(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listUnreferencedLocked(ctx, limit)
}

// ============================================================================
// Helper Methods
// ============================================================================

// initFileBlockData initializes the fileBlockStoreData if needed.
// Must be called with the write lock held.
func (s *MemoryMetadataStore) initFileBlockData() {
	if s.fileBlockData == nil {
		s.fileBlockData = newFileBlockStoreData()
	}
}

// ============================================================================
// Transaction Support
// ============================================================================

// Ensure memoryTransaction implements FileBlockStore
var _ metadata.FileBlockStore = (*memoryTransaction)(nil)

func (tx *memoryTransaction) GetFileBlock(ctx context.Context, id string) (*metadata.FileBlock, error) {
	return tx.store.getFileBlockLocked(ctx, id)
}

func (tx *memoryTransaction) PutFileBlock(ctx context.Context, block *metadata.FileBlock) error {
	return tx.store.putFileBlockLocked(ctx, block)
}

func (tx *memoryTransaction) DeleteFileBlock(ctx context.Context, id string) error {
	return tx.store.deleteFileBlockLocked(ctx, id)
}

func (tx *memoryTransaction) IncrementRefCount(ctx context.Context, id string) error {
	return tx.store.incrementRefCountLocked(ctx, id)
}

func (tx *memoryTransaction) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	return tx.store.decrementRefCountLocked(ctx, id)
}

func (tx *memoryTransaction) FindFileBlockByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
	return tx.store.findFileBlockByHashLocked(ctx, hash)
}

func (tx *memoryTransaction) ListPendingUpload(ctx context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	return tx.store.listPendingUploadLocked(ctx, olderThan, limit)
}

func (tx *memoryTransaction) ListEvictable(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	return tx.store.listEvictableLocked(ctx, limit)
}

func (tx *memoryTransaction) ListUnreferenced(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	return tx.store.listUnreferencedLocked(ctx, limit)
}

// ============================================================================
// Locked Helpers (for transaction support)
// ============================================================================

func (s *MemoryMetadataStore) getFileBlockLocked(_ context.Context, id string) (*metadata.FileBlock, error) {
	if s.fileBlockData == nil {
		return nil, metadata.ErrFileBlockNotFound
	}
	block, ok := s.fileBlockData.blocks[id]
	if !ok {
		return nil, metadata.ErrFileBlockNotFound
	}
	result := *block
	return &result, nil
}

func (s *MemoryMetadataStore) putFileBlockLocked(_ context.Context, block *metadata.FileBlock) error {
	s.initFileBlockData()
	stored := *block
	s.fileBlockData.blocks[block.ID] = &stored

	// Update hash index for finalized blocks
	if block.IsFinalized() {
		s.fileBlockData.hashIndex[block.Hash] = block.ID
	}
	return nil
}

func (s *MemoryMetadataStore) deleteFileBlockLocked(_ context.Context, id string) error {
	if s.fileBlockData == nil {
		return metadata.ErrFileBlockNotFound
	}
	block, ok := s.fileBlockData.blocks[id]
	if !ok {
		return metadata.ErrFileBlockNotFound
	}

	// Remove from hash index
	if block.IsFinalized() {
		if s.fileBlockData.hashIndex[block.Hash] == id {
			delete(s.fileBlockData.hashIndex, block.Hash)
		}
	}

	delete(s.fileBlockData.blocks, id)
	return nil
}

func (s *MemoryMetadataStore) incrementRefCountLocked(_ context.Context, id string) error {
	if s.fileBlockData == nil {
		return metadata.ErrFileBlockNotFound
	}
	block, ok := s.fileBlockData.blocks[id]
	if !ok {
		return metadata.ErrFileBlockNotFound
	}
	block.RefCount++
	return nil
}

func (s *MemoryMetadataStore) decrementRefCountLocked(_ context.Context, id string) (uint32, error) {
	if s.fileBlockData == nil {
		return 0, metadata.ErrFileBlockNotFound
	}
	block, ok := s.fileBlockData.blocks[id]
	if !ok {
		return 0, metadata.ErrFileBlockNotFound
	}
	if block.RefCount > 0 {
		block.RefCount--
	}
	return block.RefCount, nil
}

func (s *MemoryMetadataStore) findFileBlockByHashLocked(_ context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
	if s.fileBlockData == nil {
		return nil, nil
	}
	id, ok := s.fileBlockData.hashIndex[hash]
	if !ok {
		return nil, nil
	}
	block, ok := s.fileBlockData.blocks[id]
	if !ok {
		return nil, nil
	}
	// Only return uploaded blocks for dedup safety — prevents matching against
	// blocks that are dirty, being re-written, or mid-upload.
	if !block.IsUploaded() {
		return nil, nil
	}
	result := *block
	return &result, nil
}

func (s *MemoryMetadataStore) listPendingUploadLocked(_ context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	if s.fileBlockData == nil {
		return nil, nil
	}
	cutoff := time.Now().Add(-olderThan)
	var result []*metadata.FileBlock
	for _, block := range s.fileBlockData.blocks {
		if block.State == metadata.BlockStateSealed && block.IsCached() && block.CreatedAt.Before(cutoff) {
			b := *block
			result = append(result, &b)
			if limit > 0 && len(result) >= limit {
				break
			}
		}
	}
	return result, nil
}

func (s *MemoryMetadataStore) listEvictableLocked(_ context.Context, limit int) ([]*metadata.FileBlock, error) {
	if s.fileBlockData == nil {
		return nil, nil
	}
	// Collect all evictable blocks (cached + confirmed uploaded)
	var candidates []*metadata.FileBlock
	for _, block := range s.fileBlockData.blocks {
		if block.IsCached() && block.State == metadata.BlockStateUploaded {
			b := *block
			candidates = append(candidates, &b)
		}
	}

	// Sort by LastAccess (oldest first) for LRU
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].LastAccess.Before(candidates[i].LastAccess) {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}

	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates, nil
}

func (s *MemoryMetadataStore) listUnreferencedLocked(_ context.Context, limit int) ([]*metadata.FileBlock, error) {
	if s.fileBlockData == nil {
		return nil, nil
	}
	var result []*metadata.FileBlock
	for _, block := range s.fileBlockData.blocks {
		if block.RefCount == 0 {
			b := *block
			result = append(result, &b)
			if limit > 0 && len(result) >= limit {
				break
			}
		}
	}
	return result, nil
}
