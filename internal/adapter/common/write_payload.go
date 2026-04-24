package common

import (
	"context"

	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// WriteToBlockStore is the structural twin of ReadFromBlockStore. In Phase 09
// it is a direct passthrough to engine.WriteAt — there is no FileAttr.Blocks
// to fetch (deleted in Phase 08 TD-03; reintroduced in Phase 12 META-01 as
// []BlockRef). The helper exists so that when Phase 12 changes the engine
// signature (API-01) and reintroduces the Blocks field, the fetch-and-slice
// logic lands here in exactly one place — every protocol handler (NFSv3,
// NFSv4, SMB v2) calls this function and is therefore unchanged by Phase 12.
//
// Returns error only — mirrors engine.BlockStore.WriteAt. The engine contract
// guarantees that a nil error means the full `data` slice was persisted at
// `offset`; partial writes surface as an error.
//
// Wire protocol fidelity: the wire carries (offset, length); NFS3/4 and SMB2/3
// do not know about blocks. Phase 09 preserves that by keeping the
// (payloadID, data, offset) signature identical to the current engine
// contract. Phase 12 adds the BlockRef resolution INSIDE this function body
// (fetch FileAttr.Blocks → slice to [offset, offset+len(data)) → pass
// resolved []BlockRef to engine.WriteAt). Call-site code will not change.
//
// Unlike ReadFromBlockStore, WriteToBlockStore does NOT take a pooled
// buffer: the `data []byte` is owned by the caller (typically the wire decode
// layer), and common/ never retains a reference past the engine.WriteAt call.
func WriteToBlockStore(
	ctx context.Context,
	blockStore *engine.BlockStore,
	payloadID metadata.PayloadID,
	data []byte,
	offset uint64,
) error {
	return blockStore.WriteAt(ctx, string(payloadID), data, offset)
}

// CommitBlockStore is the COMMIT/flush seam used by NFSv3 COMMIT, NFSv4
// COMMIT, and SMB CLOSE. All three protocols today flush via
// engine.Flush(ctx, payloadID) with identical signatures; this helper wraps
// that call so Phase 12 can add the same BlockRef-aware plumbing once,
// keeping protocol handler code unchanged.
//
// Phase 09 is a direct passthrough; the *blockstore.FlushResult from the
// engine is dropped because every existing call site already ignores it
// (only the error is acted on). If a future phase needs per-file flush
// telemetry, widen the signature then.
//
// See D-12 (call-site refactor only) and the Phase-12 seam section in
// doc.go for the full rationale.
func CommitBlockStore(
	ctx context.Context,
	blockStore *engine.BlockStore,
	payloadID metadata.PayloadID,
) error {
	_, err := blockStore.Flush(ctx, string(payloadID))
	return err
}
