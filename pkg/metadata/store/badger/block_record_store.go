package badger

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/block"
)

var errNotImplemented = errors.New("not implemented")

// BlockRecordStore stubs — not yet implemented for the badger backend.

func (tx *badgerTransaction) PutBlockRecord(_ context.Context, _ block.BlockRecord) error {
	return errNotImplemented
}

func (tx *badgerTransaction) GetBlockRecord(_ context.Context, _ string) (block.BlockRecord, bool, error) {
	return block.BlockRecord{}, false, errNotImplemented
}

func (tx *badgerTransaction) DeleteBlockRecord(_ context.Context, _ string) error {
	return errNotImplemented
}

func (tx *badgerTransaction) WalkBlockRecords(_ context.Context, _ func(block.BlockRecord) error) error {
	return errNotImplemented
}

func (tx *badgerTransaction) DecrLiveChunkCount(_ context.Context, _ string, _ uint32) (uint32, error) {
	return 0, errNotImplemented
}

// LocalChunkIndex stubs — not yet implemented for the badger backend.

func (tx *badgerTransaction) PutLocalLocation(_ context.Context, _ block.ContentHash, _ block.LocalChunkLocation) error {
	return errNotImplemented
}

func (tx *badgerTransaction) GetLocalLocation(_ context.Context, _ block.ContentHash) (block.LocalChunkLocation, bool, error) {
	return block.LocalChunkLocation{}, false, errNotImplemented
}

func (tx *badgerTransaction) DeleteLocalLocation(_ context.Context, _ block.ContentHash) error {
	return errNotImplemented
}

// Store-level stubs.

func (s *BadgerMetadataStore) PutBlockRecord(_ context.Context, _ block.BlockRecord) error {
	return errNotImplemented
}

func (s *BadgerMetadataStore) GetBlockRecord(_ context.Context, _ string) (block.BlockRecord, bool, error) {
	return block.BlockRecord{}, false, errNotImplemented
}

func (s *BadgerMetadataStore) DeleteBlockRecord(_ context.Context, _ string) error {
	return errNotImplemented
}

func (s *BadgerMetadataStore) WalkBlockRecords(_ context.Context, _ func(block.BlockRecord) error) error {
	return errNotImplemented
}

func (s *BadgerMetadataStore) DecrLiveChunkCount(_ context.Context, _ string, _ uint32) (uint32, error) {
	return 0, errNotImplemented
}

func (s *BadgerMetadataStore) PutLocalLocation(_ context.Context, _ block.ContentHash, _ block.LocalChunkLocation) error {
	return errNotImplemented
}

func (s *BadgerMetadataStore) GetLocalLocation(_ context.Context, _ block.ContentHash) (block.LocalChunkLocation, bool, error) {
	return block.LocalChunkLocation{}, false, errNotImplemented
}

func (s *BadgerMetadataStore) DeleteLocalLocation(_ context.Context, _ block.ContentHash) error {
	return errNotImplemented
}

func (s *BadgerMetadataStore) CommitBlock(_ context.Context, _ block.BlockRecord, _ []block.BlockChunkCommit) error {
	return errNotImplemented
}
