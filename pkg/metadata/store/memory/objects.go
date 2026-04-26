package memory

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
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
var _ blockstore.FileBlockStore = (*MemoryMetadataStore)(nil)

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

// ListLocalBlocks returns blocks in Local state (complete, on disk, not yet
// synced to remote) older than the given duration.
// If limit > 0, at most limit blocks are returned.
func (s *MemoryMetadataStore) ListLocalBlocks(ctx context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listLocalBlocksLocked(ctx, olderThan, limit)
}

// ListRemoteBlocks returns blocks that are both cached locally and confirmed
// in remote store, ordered by LRU (oldest LastAccess first), up to limit.
func (s *MemoryMetadataStore) ListRemoteBlocks(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listRemoteBlocksLocked(ctx, limit)
}

// ListUnreferenced returns blocks with RefCount=0, up to limit.
func (s *MemoryMetadataStore) ListUnreferenced(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listUnreferencedLocked(ctx, limit)
}

// ListFileBlocks returns all blocks belonging to a file, ordered by block index.
func (s *MemoryMetadataStore) ListFileBlocks(ctx context.Context, payloadID string) ([]*metadata.FileBlock, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listFileBlocksLocked(ctx, payloadID)
}

// EnumerateFileBlocks streams every FileBlock's ContentHash through fn.
// The memory backend snapshots hashes under the read lock then releases the
// lock before invoking fn so callers can issue further metadata operations.
// See GC-01 / D-02.
func (s *MemoryMetadataStore) EnumerateFileBlocks(ctx context.Context, fn func(blockstore.ContentHash) error) error {
	s.mu.RLock()
	var snapshot []blockstore.ContentHash
	if s.fileBlockData != nil {
		snapshot = make([]blockstore.ContentHash, 0, len(s.fileBlockData.blocks))
		for _, b := range s.fileBlockData.blocks {
			snapshot = append(snapshot, b.Hash)
		}
	}
	s.mu.RUnlock()
	for _, h := range snapshot {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("enumerate file blocks: %w", err)
		}
		if err := fn(h); err != nil {
			return err
		}
	}
	return nil
}

// EnumerateSyncingBlocks returns every FileBlock currently in
// BlockStateSyncing. Phase 11 D-14: the engine.Syncer janitor uses this to
// requeue rows abandoned by a previous syncer instance. The memory backend
// implements this via direct map iteration; other backends may opt in
// when their query surface allows.
func (s *MemoryMetadataStore) EnumerateSyncingBlocks(_ context.Context) ([]*metadata.FileBlock, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.fileBlockData == nil {
		return nil, nil
	}
	var result []*metadata.FileBlock
	for _, block := range s.fileBlockData.blocks {
		if block.State == metadata.BlockStateSyncing {
			b := *block
			result = append(result, &b)
		}
	}
	return result, nil
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
var _ blockstore.FileBlockStore = (*memoryTransaction)(nil)

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

func (tx *memoryTransaction) ListLocalBlocks(ctx context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	return tx.store.listLocalBlocksLocked(ctx, olderThan, limit)
}

func (tx *memoryTransaction) ListRemoteBlocks(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	return tx.store.listRemoteBlocksLocked(ctx, limit)
}

func (tx *memoryTransaction) ListUnreferenced(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	return tx.store.listUnreferencedLocked(ctx, limit)
}

func (tx *memoryTransaction) ListFileBlocks(ctx context.Context, payloadID string) ([]*metadata.FileBlock, error) {
	return tx.store.listFileBlocksLocked(ctx, payloadID)
}

func (tx *memoryTransaction) EnumerateFileBlocks(ctx context.Context, fn func(blockstore.ContentHash) error) error {
	return tx.store.EnumerateFileBlocks(ctx, fn)
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
	// Only return remote blocks for dedup safety — prevents matching against
	// blocks that are dirty, being re-written, or mid-sync.
	if !block.IsRemote() {
		return nil, nil
	}
	result := *block
	return &result, nil
}

func (s *MemoryMetadataStore) listLocalBlocksLocked(_ context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	if s.fileBlockData == nil {
		return nil, nil
	}
	// olderThan <= 0 means "no age filter" — return every local block.
	// Using LastAccess.Before(time.Now()) is unreliable under tight scheduling
	// (freshly-flushed blocks may tie or beat the cutoff), which flaked
	// TestSyncer_ConcurrentOperations_Memory.
	var cutoff time.Time
	filterByAge := olderThan > 0
	if filterByAge {
		cutoff = time.Now().Add(-olderThan)
	}
	var result []*metadata.FileBlock
	for _, block := range s.fileBlockData.blocks {
		if block.State != metadata.BlockStatePending || !block.HasLocalFile() {
			continue
		}
		if filterByAge && !block.LastAccess.Before(cutoff) {
			continue
		}
		b := *block
		result = append(result, &b)
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result, nil
}

func (s *MemoryMetadataStore) listRemoteBlocksLocked(_ context.Context, limit int) ([]*metadata.FileBlock, error) {
	if s.fileBlockData == nil {
		return nil, nil
	}
	// Collect all remote blocks (cached + confirmed in remote store)
	var candidates []*metadata.FileBlock
	for _, block := range s.fileBlockData.blocks {
		if block.HasLocalFile() && block.State == metadata.BlockStateRemote {
			b := *block
			candidates = append(candidates, &b)
		}
	}

	// Sort by LastAccess (oldest first) for LRU
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].LastAccess.Before(candidates[j].LastAccess)
	})

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

func (s *MemoryMetadataStore) listFileBlocksLocked(_ context.Context, payloadID string) ([]*metadata.FileBlock, error) {
	if s.fileBlockData == nil {
		return []*metadata.FileBlock{}, nil
	}
	prefix := payloadID + "/"
	type indexedBlock struct {
		block *metadata.FileBlock
		idx   int
	}
	var candidates []indexedBlock
	for id, block := range s.fileBlockData.blocks {
		if strings.HasPrefix(id, prefix) {
			suffix := id[len(prefix):]
			blockIdx, err := strconv.Atoi(suffix)
			if err != nil {
				continue // Skip entries with non-numeric suffix
			}
			b := *block
			candidates = append(candidates, indexedBlock{block: &b, idx: blockIdx})
		}
	}
	// Sort by block index ascending
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].idx < candidates[j].idx
	})
	result := make([]*metadata.FileBlock, len(candidates))
	for i, c := range candidates {
		result[i] = c.block
	}
	return result, nil
}
