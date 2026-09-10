package common

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ReleaseClobberedPayload frees the content of the file a rename silently
// unlinked by renaming over it. metadata.Service.Move drops that victim's last
// link but, like RemoveFile, never its bytes — it returns the victim so the
// caller can coordinate content deletion. Without this call the victim's
// records stay indexed as live in the per-share local tier, so no reclamation
// path (fast-path retire, eviction, repack) can treat them as dead and the
// bytes survive every restart. The remote tier needs nothing: its GC derives
// orphans from (synced − live) in the metadata store, which the unlink already
// drives.
//
// SMB reaches the same block-store delete through its own
// purgeBlockStorePayload, which additionally resolves the handle it is given.
//
// No-ops when there is nothing to release: no victim (the rename replaced
// nothing, replaced a directory, or trash recycled the victim), or an empty
// PayloadID, Move's signal that the content must survive a remaining hard link.
// Best-effort by contract — the rename is already committed and the client has
// its answer, so callers log a non-nil error rather than failing the operation.
// `handle` only has to belong to the victim's share, which the destination
// directory does.
func ReleaseClobberedPayload(
	ctx context.Context,
	reg BlockStoreRegistry,
	handle metadata.FileHandle,
	clobbered *metadata.File,
) error {
	if clobbered == nil || clobbered.PayloadID == "" {
		return nil
	}

	blockStore, err := ResolveForWrite(ctx, reg, handle)
	if err != nil {
		return err
	}

	// Pass nil blocks: the block store resolves this payload's manifest itself
	// and reaps each row's refcount so the chunks become GC-eligible, in
	// addition to purging the append log and the tracked size.
	return blockStore.Delete(ctx, string(clobbered.PayloadID), nil)
}
