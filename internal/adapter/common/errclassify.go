package common

import (
	goerrors "errors"

	"github.com/marmos91/dittofs/pkg/block/engine"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// ClassifyBlockStoreError maps a block-store error to the metadata error code
// the wire mappers consume. A block store closed under an in-flight op (the
// share was removed or hot-reloaded mid-transfer) classifies as the
// stale-handle row so the client observes the share going away; every other
// block-store failure — CAS integrity (chunk content/ref sentinels), an
// unavailable remote tier, local-store backpressure, durability-not-yet, an
// opaque engine fault, or a canceled context — classifies as the generic
// I/O row, because none of them is a dead-handle signal the client can act
// on beyond retrying.
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
	return merrs.ErrIOError
}
