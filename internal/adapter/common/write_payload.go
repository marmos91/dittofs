package common

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ErrNotDurableYet is returned by CommitBlockStore when a CLOSE / COMMIT cannot
// honestly report success: the share's local store is NOT durable (e.g. an
// in-memory local store) AND the data has not yet reached a durable remote
// (the engine FlushResult is not Finalized, or the remote is itself not
// durable). The bytes are still safe in local CAS and the background syncer
// will keep re-driving the mirror, but a crash before that completes would lose
// data the client was told CLOSE succeeded on — so the protocol adapter returns
// a transient I/O error (NFS3ERR_IO / NFS4ERR_IO / SMB STATUS_UNEXPECTED_IO_ERROR)
// and the client re-drives COMMIT/CLOSE.
//
// In the common production configuration (fs local store) this sentinel NEVER
// occurs: CommitBlockStore returns nil on the fast local-durable path before it
// is ever reached, and the remote mirror stays fully asynchronous. See #1274.
var ErrNotDurableYet = errors.New("block: data not yet durable (local store is volatile and durable remote not reached)")

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
// CommitBlockStore applies the honest per-store durability commit rule (#1274).
// A payload is "committed" (CLOSE/COMMIT may report success) iff
//
//	localDurable || (Finalized && remoteDurable)
//
// where localDurable / remoteDurable come from the engine's per-store
// DurabilityReporter and Finalized comes from the engine FlushResult:
//
//   - Production (fs local store): localDurable=true → ack immediately on the
//     FAST path, with no remote wait. The remote mirror stays fully async (the
//     syncer keeps draining in the background) and ErrNotDurableYet is never
//     returned.
//   - memory local + durable remote (s3): the bytes are only safe once they
//     reach the durable remote, so success requires FlushResult.Finalized.
//     A transient unhealthy-remote / uploader-gate-contention flush returns
//     {Finalized:false} → ErrNotDurableYet so the client re-drives.
//   - memory local with no remote / a non-durable remote: never durable →
//     ErrNotDurableYet (honest failure rather than silent loss).
//
// A hard flush error (I/O fault, remote.Put rejection, metadata error) is
// returned unchanged.
func CommitBlockStore(
	ctx context.Context,
	blockStore *engine.Store,
	payloadID metadata.PayloadID,
) error {
	res, err := blockStore.Flush(ctx, string(payloadID))
	if err != nil {
		// Hard error: unchanged behavior (mapped via the content errmap).
		return err
	}
	// FAST path: a durable local store means the bytes already survive a
	// restart; ack without waiting for the async remote mirror.
	if blockStore.LocalDurable() {
		return nil
	}
	// Local store is volatile: success requires the data to have reached a
	// durable remote (Finalized AND the remote is itself durable).
	if res != nil && res.Finalized && blockStore.RemoteDurable() {
		return nil
	}
	// Not yet durable anywhere that survives a crash — report so the client
	// re-drives. The bytes remain in local CAS and the syncer keeps mirroring.
	return ErrNotDurableYet
}
