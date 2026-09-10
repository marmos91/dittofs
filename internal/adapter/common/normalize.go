package common

import (
	goerrors "errors"

	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// normalizeBlockStoreError converts a raw block-store error into a
// *merrs.StoreError so the wire mappers (the per-adapter StatusFor family)
// can see its code. Every non-nil error from ReadFromBlockStore,
// WriteToBlockStore, and CommitBlockStore passes through here exactly once:
//
//   - engine.ErrStoreClosed (the share was removed or hot-reloaded
//     mid-transfer) wraps as the stale-handle row.
//   - every other failure — CAS integrity sentinels, an unavailable remote
//     tier, local-store backpressure, durability-not-yet, an opaque engine
//     fault, a canceled context — wraps as the generic I/O row.
//
// The original error is preserved as Cause (multi-%w), so errors.Is and
// errors.As still traverse to the block sentinel for logging and tests.
// Callers that need the code directly use ClassifyBlockStoreError.
func normalizeBlockStoreError(err error) error {
	var storeErr *merrs.StoreError
	if goerrors.As(err, &storeErr) {
		// Already a StoreError (e.g. an engine op that surfaced a metadata
		// error directly) — pass through so the code is not double-wrapped.
		return err
	}
	code := ClassifyBlockStoreError(err)
	return &merrs.StoreError{
		Code:    code,
		Message: err.Error(),
		Cause:   err,
	}
}
