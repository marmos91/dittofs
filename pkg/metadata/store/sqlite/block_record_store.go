package sqlite

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/block"
)

var errNotImplemented = errors.New("not implemented")

// BlockRecordStore stubs — not yet implemented for the sqlite backend.

func (tx *sqliteTransaction) PutBlockRecord(_ context.Context, _ block.BlockRecord) error {
	return errNotImplemented
}

func (tx *sqliteTransaction) GetBlockRecord(_ context.Context, _ string) (block.BlockRecord, bool, error) {
	return block.BlockRecord{}, false, errNotImplemented
}

func (tx *sqliteTransaction) DeleteBlockRecord(_ context.Context, _ string) error {
	return errNotImplemented
}

func (tx *sqliteTransaction) WalkBlockRecords(_ context.Context, _ func(block.BlockRecord) error) error {
	return errNotImplemented
}

func (tx *sqliteTransaction) DecrLiveChunkCount(_ context.Context, _ string, _ uint32) (uint32, error) {
	return 0, errNotImplemented
}

// LocalChunkIndex stubs — not yet implemented for the sqlite backend.

func (tx *sqliteTransaction) PutLocalLocation(_ context.Context, _ block.ContentHash, _ block.LocalChunkLocation) error {
	return errNotImplemented
}

func (tx *sqliteTransaction) GetLocalLocation(_ context.Context, _ block.ContentHash) (block.LocalChunkLocation, bool, error) {
	return block.LocalChunkLocation{}, false, errNotImplemented
}

func (tx *sqliteTransaction) DeleteLocalLocation(_ context.Context, _ block.ContentHash) error {
	return errNotImplemented
}

// Store-level stubs.

func (s *SQLiteMetadataStore) PutBlockRecord(_ context.Context, _ block.BlockRecord) error {
	return errNotImplemented
}

func (s *SQLiteMetadataStore) GetBlockRecord(_ context.Context, _ string) (block.BlockRecord, bool, error) {
	return block.BlockRecord{}, false, errNotImplemented
}

func (s *SQLiteMetadataStore) DeleteBlockRecord(_ context.Context, _ string) error {
	return errNotImplemented
}

func (s *SQLiteMetadataStore) WalkBlockRecords(_ context.Context, _ func(block.BlockRecord) error) error {
	return errNotImplemented
}

func (s *SQLiteMetadataStore) DecrLiveChunkCount(_ context.Context, _ string, _ uint32) (uint32, error) {
	return 0, errNotImplemented
}

func (s *SQLiteMetadataStore) PutLocalLocation(_ context.Context, _ block.ContentHash, _ block.LocalChunkLocation) error {
	return errNotImplemented
}

func (s *SQLiteMetadataStore) GetLocalLocation(_ context.Context, _ block.ContentHash) (block.LocalChunkLocation, bool, error) {
	return block.LocalChunkLocation{}, false, errNotImplemented
}

func (s *SQLiteMetadataStore) DeleteLocalLocation(_ context.Context, _ block.ContentHash) error {
	return errNotImplemented
}

func (s *SQLiteMetadataStore) CommitBlock(_ context.Context, _ block.BlockRecord, _ []block.BlockChunkCommit) error {
	return errNotImplemented
}
