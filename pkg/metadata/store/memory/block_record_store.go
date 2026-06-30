package memory

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Transaction-level stubs — replaced by full implementation in Phase 4.

func (tx *memoryTransaction) PutBlockRecord(_ context.Context, _ block.BlockRecord) error {
	return errors.New("not implemented")
}

func (tx *memoryTransaction) GetBlockRecord(_ context.Context, _ string) (block.BlockRecord, bool, error) {
	return block.BlockRecord{}, false, errors.New("not implemented")
}

func (tx *memoryTransaction) DeleteBlockRecord(_ context.Context, _ string) error {
	return errors.New("not implemented")
}

func (tx *memoryTransaction) WalkBlockRecords(_ context.Context, _ func(block.BlockRecord) error) error {
	return errors.New("not implemented")
}

func (tx *memoryTransaction) DecrLiveChunkCount(_ context.Context, _ string, _ uint32) (uint32, error) {
	return 0, errors.New("not implemented")
}

func (tx *memoryTransaction) PutLocalLocation(_ context.Context, _ block.ContentHash, _ block.LocalChunkLocation) error {
	return errors.New("not implemented")
}

func (tx *memoryTransaction) GetLocalLocation(_ context.Context, _ block.ContentHash) (block.LocalChunkLocation, bool, error) {
	return block.LocalChunkLocation{}, false, errors.New("not implemented")
}

func (tx *memoryTransaction) DeleteLocalLocation(_ context.Context, _ block.ContentHash) error {
	return errors.New("not implemented")
}

// Store-level stubs — replaced by full implementation in Phase 4.

func (s *MemoryMetadataStore) PutBlockRecord(_ context.Context, _ block.BlockRecord) error {
	return errors.New("not implemented")
}

func (s *MemoryMetadataStore) GetBlockRecord(_ context.Context, _ string) (block.BlockRecord, bool, error) {
	return block.BlockRecord{}, false, errors.New("not implemented")
}

func (s *MemoryMetadataStore) DeleteBlockRecord(_ context.Context, _ string) error {
	return errors.New("not implemented")
}

func (s *MemoryMetadataStore) WalkBlockRecords(_ context.Context, _ func(block.BlockRecord) error) error {
	return errors.New("not implemented")
}

func (s *MemoryMetadataStore) DecrLiveChunkCount(_ context.Context, _ string, _ uint32) (uint32, error) {
	return 0, errors.New("not implemented")
}

func (s *MemoryMetadataStore) PutLocalLocation(_ context.Context, _ block.ContentHash, _ block.LocalChunkLocation) error {
	return errors.New("not implemented")
}

func (s *MemoryMetadataStore) GetLocalLocation(_ context.Context, _ block.ContentHash) (block.LocalChunkLocation, bool, error) {
	return block.LocalChunkLocation{}, false, errors.New("not implemented")
}

func (s *MemoryMetadataStore) DeleteLocalLocation(_ context.Context, _ block.ContentHash) error {
	return errors.New("not implemented")
}

func (s *MemoryMetadataStore) CommitBlock(ctx context.Context, rec block.BlockRecord, chunks []block.BlockChunkCommit) error {
	return metadata.CommitBlockImpl(ctx, s, rec, chunks)
}
