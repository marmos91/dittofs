package common

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ReclaimTruncatedBlocks physically discards block data past newSize after a
// metadata size-down. metaSvc.SetFileAttributes prunes FileAttr.Blocks and
// updates the size, but the per-share block store still holds the tail bytes;
// without this call a later re-extend re-exposes the discarded data as file
// content instead of a zero-filled hole (silent data-integrity / info-leak
// bug) and the dropped CAS chunks leak on the remote because their RefCount is
// never decremented, so GC never reclaims them (#832).
//
// Callers pass the PRE-truncate file snapshot (fetched via GetFile BEFORE
// SetFileAttributes pruned it) so the engine reaps RefCount on every dropped
// block. This is the single seam all protocol adapters (NFSv3/v4 SETATTR +
// CREATE-truncate, SMB SetEndOfFile) drive so the block-store truncate can
// never be forgotten by one protocol while another gets it right.
//
// No-ops (returns nil) when the operation is not a genuine content shrink:
// nil snapshot, no payload, or newSize >= the pre-op size. Best-effort by
// contract — the metadata mutation is already committed, so callers log a
// non-nil error rather than failing the operation.
func ReclaimTruncatedBlocks(
	ctx context.Context,
	reg BlockStoreRegistry,
	handle metadata.FileHandle,
	preFile *metadata.File,
	newSize uint64,
) error {
	if preFile == nil || preFile.PayloadID == "" || newSize >= preFile.Size {
		return nil
	}

	blockStore, err := ResolveForWrite(ctx, reg, handle)
	if err != nil {
		return err
	}

	_, err = blockStore.Truncate(ctx, string(preFile.PayloadID), preFile.Blocks, newSize)
	return err
}
