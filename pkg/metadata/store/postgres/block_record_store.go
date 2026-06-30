package postgres

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/block"
)

var errNotImplemented = errors.New("not implemented")

// BlockRecordStore stubs — not yet implemented for the postgres backend.

func (tx *postgresTransaction) PutBlockRecord(_ context.Context, _ block.BlockRecord) error {
	return errNotImplemented
}

func (tx *postgresTransaction) GetBlockRecord(_ context.Context, _ string) (block.BlockRecord, bool, error) {
	return block.BlockRecord{}, false, errNotImplemented
}

func (tx *postgresTransaction) DeleteBlockRecord(_ context.Context, _ string) error {
	return errNotImplemented
}

func (tx *postgresTransaction) WalkBlockRecords(_ context.Context, _ func(block.BlockRecord) error) error {
	return errNotImplemented
}

func (tx *postgresTransaction) DecrLiveChunkCount(_ context.Context, _ string, _ uint32) (uint32, error) {
	return 0, errNotImplemented
}

// LocalChunkIndex stubs — not yet implemented for the postgres backend.

func (tx *postgresTransaction) PutLocalLocation(_ context.Context, _ block.ContentHash, _ block.LocalChunkLocation) error {
	return errNotImplemented
}

func (tx *postgresTransaction) GetLocalLocation(_ context.Context, _ block.ContentHash) (block.LocalChunkLocation, bool, error) {
	return block.LocalChunkLocation{}, false, errNotImplemented
}

func (tx *postgresTransaction) DeleteLocalLocation(_ context.Context, _ block.ContentHash) error {
	return errNotImplemented
}

// Store-level stubs.

func (s *PostgresMetadataStore) PutBlockRecord(_ context.Context, _ block.BlockRecord) error {
	return errNotImplemented
}

func (s *PostgresMetadataStore) GetBlockRecord(_ context.Context, _ string) (block.BlockRecord, bool, error) {
	return block.BlockRecord{}, false, errNotImplemented
}

func (s *PostgresMetadataStore) DeleteBlockRecord(_ context.Context, _ string) error {
	return errNotImplemented
}

func (s *PostgresMetadataStore) WalkBlockRecords(_ context.Context, _ func(block.BlockRecord) error) error {
	return errNotImplemented
}

func (s *PostgresMetadataStore) DecrLiveChunkCount(_ context.Context, _ string, _ uint32) (uint32, error) {
	return 0, errNotImplemented
}

func (s *PostgresMetadataStore) PutLocalLocation(_ context.Context, _ block.ContentHash, _ block.LocalChunkLocation) error {
	return errNotImplemented
}

func (s *PostgresMetadataStore) GetLocalLocation(_ context.Context, _ block.ContentHash) (block.LocalChunkLocation, bool, error) {
	return block.LocalChunkLocation{}, false, errNotImplemented
}

func (s *PostgresMetadataStore) DeleteLocalLocation(_ context.Context, _ block.ContentHash) error {
	return errNotImplemented
}

func (s *PostgresMetadataStore) CommitBlock(_ context.Context, _ block.BlockRecord, _ []block.BlockChunkCommit) error {
	return errNotImplemented
}
