package common

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ErrNotDurableYet is returned by CommitBlockStore ONLY when a share has
// opted into strict honest-durability enforcement
// (config["require_durable_commit"] = true) and a CLOSE / COMMIT cannot
// honestly report success: the share's local store is NOT durable (e.g. an
// in-memory local store) AND the data has not yet reached a durable remote
// (the engine FlushResult is not Finalized, or the remote is itself not
// durable). The bytes are still safe in local CAS and the background syncer
// will keep re-driving the mirror, but a crash before that completes would lose
// data the client was told CLOSE succeeded on — so the protocol adapter returns
// a transient I/O error (NFS3ERR_IO / NFS4ERR_IO / SMB STATUS_UNEXPECTED_IO_ERROR)
// and the client re-drives COMMIT/CLOSE.
//
// This sentinel is NEVER returned in the default configuration
// (require_durable_commit = false): CommitBlockStore acks once engine.Flush
// succeeds and the remote mirror stays fully asynchronous and observable via
// the unsynced-bytes metric. Even when strict enforcement IS enabled, the
// common production case (fs local store) returns nil on the fast
// local-durable path before this is ever reached.
var ErrNotDurableYet = errors.New("block: data not yet durable (local store is volatile and durable remote not reached)")

// WriteToBlockStore is the structural twin of ReadFromBlockStore. It is a
// direct passthrough to engine.WriteAt today — there is no FileAttr.Blocks
// to fetch yet. The helper exists so that when FileAttr.Blocks is
// reintroduced as []ChunkRef and the engine signature changes, the
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
// identical to the current engine contract. ChunkRef resolution can later be
// added INSIDE this function body (fetch FileAttr.Blocks → slice to
// [offset, offset+len(data)) → pass resolved []ChunkRef to engine.WriteAt)
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
	// path; discard the returned []ChunkRef. Caller-snapshot []ChunkRef
	// threading lands in a later refactor.
	_, err := blockStore.WriteAt(ctx, string(payloadID), nil, data, offset)
	if err != nil {
		return normalizeBlockStoreError(err)
	}
	return nil
}

// CommitBlockStore is the COMMIT/flush seam used by NFSv3 COMMIT, NFSv4
// COMMIT, and SMB CLOSE. All three protocols today flush via
// engine.Flush(ctx, payloadID) with identical signatures; this helper wraps
// that call so a later refactor can add ChunkRef-aware plumbing once,
// keeping protocol-handler code unchanged.
//
// A hard flush error (I/O fault, remote.Put rejection, metadata error) is
// ALWAYS surfaced unchanged — engine.Flush returning a non-nil error
// propagates regardless of policy.
//
// Beyond that, durability acknowledgement is governed by the per-share policy
// flag RequireDurableCommit (config["require_durable_commit"], default false):
//
//   - DEFAULT (RequireDurableCommit == false): once engine.Flush succeeds,
//     CommitBlockStore returns nil unconditionally — CLOSE/COMMIT acknowledge
//     regardless of local/remote durability. The remote mirror stays fully
//     asynchronous and observable via the unsynced-bytes metric. This
//     preserves the fast async design and the normal NFS/POSIX path (ordinary
//     writes never EIO).
//
//   - STRICT (RequireDurableCommit == true): apply the honest per-store
//     durability rule — a payload is "committed" iff
//
//     localDurable || (Finalized && remoteDurable)
//
//     where localDurable / remoteDurable come from the engine's per-store
//     DurabilityReporter and Finalized comes from the engine FlushResult:
//
//   - fs local store: localDurable=true → ack immediately on the FAST
//     path, no remote wait (the flag is a no-op for fs-local).
//
//   - memory local + durable remote (s3): success requires
//     FlushResult.Finalized; a transient unhealthy-remote flush returns
//     {Finalized:false} → ErrNotDurableYet so the client re-drives.
//
//   - memory local with no remote / a non-durable remote: never durable →
//     ErrNotDurableYet (the operator opted into honest failure).
func CommitBlockStore(
	ctx context.Context,
	blockStore *engine.Store,
	payloadID metadata.PayloadID,
) error {
	res, err := blockStore.Flush(ctx, string(payloadID))
	if err != nil {
		// Hard error: unchanged behavior, normalized so the wire mappers see
		// the code (a closed store wraps as the stale-handle row, the rest as
		// the I/O row; the original error stays reachable via Cause).
		return normalizeBlockStoreError(err)
	}

	// Observability: record the per-store durability decision at the
	// ack point. engine.Flush returning nil only means the flush pump did not
	// fault; in the DEFAULT (async) policy Finalized may still be false while
	// the remote mirror catches up, and localDurable=true (fs local store)
	// means the bytes already survive a restart regardless of Finalized. This
	// single DEBUG line surfaces the exact (Finalized, local/remote durable)
	// inputs so a saturated-CLOSE trace can confirm there is no silent window
	// without adding a log line to the common (durable) hot path's behavior.
	// The booleans below feed the decision logic regardless of log level, so
	// only the DebugCtx call itself (the string conversion + variadic boxing)
	// is guarded to keep the ack path allocation-free when DEBUG is disabled.
	finalized := res != nil && res.Finalized
	localDurable := blockStore.LocalDurable()
	remoteDurable := blockStore.RemoteDurable()
	requireDurable := blockStore.RequireDurableCommit()
	if logger.IsDebugEnabled() {
		logger.DebugCtx(ctx, "COMMIT/CLOSE flush decision",
			"payloadID", string(payloadID),
			"finalized", finalized,
			"localDurable", localDurable,
			"remoteDurable", remoteDurable,
			"requireDurableCommit", requireDurable,
			"durableAtAck", localDurable || (finalized && remoteDurable),
		)
	}

	// DEFAULT: strict durability enforcement is opt-in. A successful flush is
	// enough to ack — the remote mirror stays async and observable.
	if !requireDurable {
		return nil
	}
	// FAST path: a durable local store means the bytes already survive a
	// restart; ack without waiting for the async remote mirror.
	if localDurable {
		return nil
	}
	// Local store is volatile: success requires the data to have reached a
	// durable remote (Finalized AND the remote is itself durable).
	if finalized && remoteDurable {
		return nil
	}
	// Not yet durable anywhere that survives a crash — report so the client
	// re-drives. The bytes remain in local CAS and the syncer keeps mirroring.
	// Normalized so the wire mappers see the I/O-class code while the
	// ErrNotDurableYet sentinel stays reachable via Cause for tests and the
	// durability policy's own retry classification.
	return normalizeBlockStoreError(ErrNotDurableYet)
}

// FlushStableWrite forces a file's cached data — and, when fileSync is set, its
// metadata — to stable storage so a WRITE that asked for more than UNSTABLE can
// be acknowledged at the level it asked for. It mirrors the COMMIT path: flush
// the block store, then persist the file's pending metadata.
//
// NFSv3 (RFC 1813 Section 3.3.7) and NFSv4 (RFC 7530 Section 16.36.4) draw the
// same line between the two stable levels:
//
//   - DATA_SYNC: only the file data must be on stable storage. A metadata flush
//     failure is tolerated — a later COMMIT or the share-start journal reconcile
//     makes the size durable — and the write is still reported at DATA_SYNC.
//   - FILE_SYNC: data AND metadata must be on stable storage before the reply,
//     so a metadata flush failure propagates and the caller must report a weaker
//     level rather than claim a durability it did not provide.
func FlushStableWrite(
	authCtx *metadata.AuthContext,
	metaSvc *metadata.Service,
	blockStore *engine.Store,
	handle metadata.FileHandle,
	payloadID metadata.PayloadID,
	fileSync bool,
) error {
	if err := CommitBlockStore(authCtx.Context, blockStore, payloadID); err != nil {
		return err
	}
	if _, err := metaSvc.FlushPendingWriteForFile(authCtx, handle, fileSync); err != nil {
		if fileSync {
			return err
		}
		logger.WarnCtx(authCtx.Context, "WRITE: DATA_SYNC metadata flush failed (data durable, will reconcile)",
			"error", err)
	}
	return nil
}
