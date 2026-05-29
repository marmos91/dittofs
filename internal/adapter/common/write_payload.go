package common

import (
	"context"

	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// WriteToBlockStore is the structural twin of ReadFromBlockStore. It is a
// direct passthrough to engine.WriteAt today — there is no FileAttr.Blocks
// to fetch yet. The helper exists so that when FileAttr.Blocks is
// reintroduced as []BlockRef and the engine signature changes, the
// fetch-and-slice logic lands here in exactly one place — every protocol
// handler (NFSv3, NFSv4, SMB v2) calls this function and therefore stays
// unchanged.
//
// Returns error only — mirrors engine.Store.WriteAt. The engine contract
// guarantees that a nil error means the full `data` slice was persisted at
// `offset`; partial writes surface as an error.
//
// Wire protocol fidelity: the wire carries (offset, length); NFS3/4 and SMB2/3
// do not know about blocks. The (payloadID, data, offset) signature is kept
// identical to the current engine contract. BlockRef resolution can later be
// added INSIDE this function body (fetch FileAttr.Blocks → slice to
// [offset, offset+len(data)) → pass resolved []BlockRef to engine.WriteAt)
// without disturbing call-site code.
//
// Unlike ReadFromBlockStore, WriteToBlockStore does NOT take a pooled
// buffer: the `data []byte` is owned by the caller (typically the wire decode
// layer), and common/ never retains a reference past the engine.WriteAt call.
func WriteToBlockStore(
	ctx context.Context,
	blockStore *engine.Store,
	payloadID metadata.PayloadID,
	data []byte,
	offset uint64,
) error {
	// Pass nil currentBlocks so the engine runs the legacy/dual-read
	// path; discard the returned []BlockRef. Caller-snapshot []BlockRef
	// threading lands in a later refactor.
	_, err := blockStore.WriteAt(ctx, string(payloadID), nil, data, offset)
	return err
}

// CommitBlockStore is the COMMIT/flush seam used by NFSv3 COMMIT, NFSv4
// COMMIT, and SMB CLOSE. All three protocols today flush via
// engine.Flush(ctx, payloadID) with identical signatures; this helper wraps
// that call so a later refactor can add BlockRef-aware plumbing once,
// keeping protocol-handler code unchanged.
//
// It is currently a direct passthrough; the *blockstore.FlushResult from
// the engine is dropped because every existing call site already ignores
// it (only the error is acted on). If per-file flush telemetry becomes
// needed, widen the signature then.
func CommitBlockStore(
	ctx context.Context,
	blockStore *engine.Store,
	payloadID metadata.PayloadID,
) error {
	_, err := blockStore.Flush(ctx, string(payloadID))
	return err
}
