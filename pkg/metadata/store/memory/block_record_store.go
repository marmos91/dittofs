package memory

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Transaction-level BlockRecordStore (runs under store.mu write lock)
// ============================================================================

func (tx *memoryTransaction) PutBlockRecord(_ context.Context, rec block.BlockRecord) error {
	cp := rec
	tx.store.blockRecords[rec.BlockID] = &cp
	return nil
}

func (tx *memoryTransaction) GetBlockRecord(_ context.Context, blockID string) (block.BlockRecord, bool, error) {
	r, ok := tx.store.blockRecords[blockID]
	if !ok {
		return block.BlockRecord{}, false, nil
	}
	return *r, true, nil
}

func (tx *memoryTransaction) DeleteBlockRecord(_ context.Context, blockID string) error {
	delete(tx.store.blockRecords, blockID)
	return nil
}

func (tx *memoryTransaction) WalkBlockRecords(_ context.Context, fn func(block.BlockRecord) error) error {
	for _, r := range tx.store.blockRecords {
		if err := fn(*r); err != nil {
			return err
		}
	}
	return nil
}

func (tx *memoryTransaction) DecrLiveChunkCount(_ context.Context, blockID string, delta uint32) (uint32, error) {
	r, ok := tx.store.blockRecords[blockID]
	if !ok {
		return 0, fmt.Errorf("block record %q not found", blockID)
	}
	if delta >= r.LiveChunkCount {
		r.LiveChunkCount = 0
	} else {
		r.LiveChunkCount -= delta
	}
	return r.LiveChunkCount, nil
}

// Transaction-level LocalChunkIndex (runs under store.mu write lock)

func (tx *memoryTransaction) PutLocalLocation(_ context.Context, hash block.ContentHash, loc block.LocalChunkLocation) error {
	tx.store.localChunks[hash] = loc
	return nil
}

func (tx *memoryTransaction) GetLocalLocation(_ context.Context, hash block.ContentHash) (block.LocalChunkLocation, bool, error) {
	loc, ok := tx.store.localChunks[hash]
	if !ok {
		return block.LocalChunkLocation{}, false, nil
	}
	return loc, true, nil
}

func (tx *memoryTransaction) DeleteLocalLocation(_ context.Context, hash block.ContentHash) error {
	delete(tx.store.localChunks, hash)
	return nil
}

// ============================================================================
// Store-level BlockRecordStore (delegates through WithTransaction for writes)
// ============================================================================

func (s *MemoryMetadataStore) PutBlockRecord(ctx context.Context, rec block.BlockRecord) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutBlockRecord(ctx, rec)
	})
}

func (s *MemoryMetadataStore) GetBlockRecord(ctx context.Context, blockID string) (block.BlockRecord, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.blockRecords[blockID]
	if !ok {
		return block.BlockRecord{}, false, nil
	}
	return *r, true, nil
}

func (s *MemoryMetadataStore) DeleteBlockRecord(ctx context.Context, blockID string) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteBlockRecord(ctx, blockID)
	})
}

func (s *MemoryMetadataStore) WalkBlockRecords(ctx context.Context, fn func(block.BlockRecord) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.blockRecords {
		if err := fn(*r); err != nil {
			return err
		}
	}
	return nil
}

func (s *MemoryMetadataStore) DecrLiveChunkCount(ctx context.Context, blockID string, delta uint32) (uint32, error) {
	var remaining uint32
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		remaining, err = tx.DecrLiveChunkCount(ctx, blockID, delta)
		return err
	})
	return remaining, err
}

// ============================================================================
// Store-level LocalChunkIndex
// ============================================================================

func (s *MemoryMetadataStore) PutLocalLocation(ctx context.Context, hash block.ContentHash, loc block.LocalChunkLocation) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutLocalLocation(ctx, hash, loc)
	})
}

func (s *MemoryMetadataStore) GetLocalLocation(ctx context.Context, hash block.ContentHash) (block.LocalChunkLocation, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	loc, ok := s.localChunks[hash]
	if !ok {
		return block.LocalChunkLocation{}, false, nil
	}
	return loc, true, nil
}

func (s *MemoryMetadataStore) DeleteLocalLocation(ctx context.Context, hash block.ContentHash) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteLocalLocation(ctx, hash)
	})
}

// ============================================================================
// CommitBlock
// ============================================================================

// CommitBlock atomically writes rec and all chunk local locations, then marks
// each chunk synced. Delegates to DefaultCommitBlock for idempotency logic.
func (s *MemoryMetadataStore) CommitBlock(ctx context.Context, rec block.BlockRecord, chunks []block.BlockChunkCommit) error {
	return metadata.DefaultCommitBlock(ctx, s, rec, chunks)
}
