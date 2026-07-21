package badger

import (
	goerrors "errors"
	"fmt"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// mapBadgerError maps a BadgerDB error to a metadata.StoreError, giving the
// badger backend the same single classification point the SQL backends have in
// mapDBError (SQLite) and mapPgError (Postgres). Routing the badger sentinels
// through one helper keeps the conformance suite and the runtime backend-
// agnostic: a missing key, an SSI abort, and any other failure all surface as
// the shared metadata codes rather than leaking the raw badger sentinel.
//
// op names the entity or operation the error concerns and is woven into the
// message ("<op> not found" for a missing key), so a routed call reproduces the
// text the inline literals emitted. path is the caller-facing path, if any.
func mapBadgerError(err error, op, path string) error {
	if err == nil {
		return nil
	}

	// Already a StoreError (e.g. from a validation helper): pass through so a
	// classified error keeps its original code, message, and path.
	var storeErr *metadata.StoreError
	if goerrors.As(err, &storeErr) {
		return storeErr
	}

	switch {
	// Missing key: the looked-up entity does not exist.
	case goerrors.Is(err, badgerdb.ErrKeyNotFound):
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: fmt.Sprintf("%s not found", op),
			Path:    path,
		}

	// SSI optimistic-concurrency abort. The raw sentinel is preserved via Cause
	// so codebase-wide conflict detection (errors.Is / IsConflictError) still
	// recognizes it through the unwrap chain and the transaction layer retries.
	case goerrors.Is(err, badgerdb.ErrConflict):
		return &metadata.StoreError{
			Code:    metadata.ErrConflict,
			Message: fmt.Sprintf("%s: transaction conflict", op),
			Path:    path,
			Cause:   err,
		}

	// Any other failure is an I/O error; keep the raw error reachable via Cause.
	default:
		return &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: fmt.Sprintf("%s: %v", op, err),
			Path:    path,
			Cause:   err,
		}
	}
}
