package common

import (
	goerrors "errors"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/journal"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// ClassifyBlockStoreError maps a block-store error to the metadata error code
// the wire mappers consume:
//
//   - a block store closed under an in-flight op (the share was removed or
//     hot-reloaded mid-transfer) classifies as the stale-handle row, so the
//     client observes the share going away;
//   - a write refused because the local store is full (the cap is pinned by
//     unsynced bytes and the backpressure wait ran out) classifies as the
//     no-space row, so the client can tell a full store from a broken one;
//   - every other block-store failure — CAS integrity (chunk content/ref
//     sentinels), an unavailable remote tier, durability-not-yet, an opaque
//     engine fault, or a canceled context — classifies as the generic I/O
//     row, because none of them is a signal the client can act on beyond
//     retrying.
//
// The block sentinels stay reachable through the returned code's wrap chain:
// the payload choke points (ReadFromBlockStore, WriteToBlockStore,
// CommitBlockStore) wrap the original error as the Cause of a
// *merrs.StoreError carrying this code, so errors.Is/errors.As still find
// the sentinel for logging and tests.
func ClassifyBlockStoreError(err error) merrs.ErrorCode {
	if goerrors.Is(err, engine.ErrStoreClosed) {
		return merrs.ErrStaleHandle
	}
	if goerrors.Is(err, journal.ErrLocalStoreFull) {
		return merrs.ErrNoSpace
	}
	return merrs.ErrIOError
}
