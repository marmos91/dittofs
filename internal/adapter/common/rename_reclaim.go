package common

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ReleaseClobberedPayload frees the content of the file a rename silently
// unlinked by renaming over it.
//
// POSIX rename onto an existing name removes whatever was there.
// metadata.Service.Move drops that victim's last link but, like RemoveFile,
// never its bytes — it returns the victim so the caller can coordinate content
// deletion. Without this call the victim's records stay indexed as live in the
// per-share local tier, so no reclamation path (fast-path retire, eviction,
// repack) can treat them as dead and the bytes survive every restart. Only the
// local tier depends on it: the remote block GC derives orphans from
// (synced − live) in the metadata store, which the unlink alone already drives.
//
// This is the single seam every protocol adapter drives after a clobbering
// rename, so one protocol cannot forget it while another gets it right.
//
// No-ops when there is nothing to release: no victim (the rename replaced
// nothing, replaced a directory, or trash recycled the victim), or an empty
// PayloadID, which is the contract's signal that the content must survive
// because another hard link still references it.
//
// Best-effort by contract — the rename has already committed and the client
// already has its answer, so callers log a non-nil error rather than failing
// the operation. `handle` only has to belong to the victim's share; handles are
// opaque and encode share identity, so the destination directory resolves the
// same per-share block store the victim's content lives in.
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
