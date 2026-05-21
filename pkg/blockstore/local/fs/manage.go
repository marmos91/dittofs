package fs

import (
	"context"
)

// SetEvictionEnabled controls whether ensureSpace can evict blocks to make room.
// When disabled (false), ensureSpace returns ErrDiskFull if the store is over its
// disk limit instead of evicting remote blocks. This is used by local-only mode
// where there is no remote store to re-fetch evicted blocks from.
//
// Defaults to true (eviction enabled).
func (bc *FSStore) SetEvictionEnabled(enabled bool) {
	bc.evictionEnabled.Store(enabled)
}

// GetStoredFileSize returns the total stored data size for a file by summing
// the DataSize of all FileBlock records in the metadata store.
// Returns 0 for unknown files (no error).
func (bc *FSStore) GetStoredFileSize(ctx context.Context, payloadID string) (uint64, error) {
	blocks, err := bc.blockStore.ListFileBlocks(ctx, payloadID)
	if err != nil {
		return 0, err
	}

	var total uint64
	for _, fb := range blocks {
		total += uint64(fb.DataSize)
	}
	return total, nil
}
