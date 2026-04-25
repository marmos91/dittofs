// Package metadata — rollup_store.go (Phase 10).
//
// RollupStore persists the per-file append-log rollup_offset for the hybrid
// local tier. See pkg/blockstore/local/fs/rollup.go for the consumer; see
// .planning/phases/10-fastcdc-chunker-hybrid-local-store-a1/10-CONTEXT.md D-12
// for the atomicity contract.
package metadata

import (
	"context"
	"errors"
)

// RollupStore persists the per-file rollup_offset for the hybrid local
// append-log tier (LSL-05). Introduced by Phase 10; a broader per-file
// metadata row may fold this in during A3 (Phase 12).
//
// INV-03 (rollup_offset monotonicity) is enforced at the STORE layer, not by
// the caller. SetRollupOffset is atomic-monotone: it rejects any update where
// the currently-stored offset > newOffset, returning (storedOffset,
// ErrRollupOffsetRegression). The stored value remains unchanged on rejection.
// This moves the read+compare+write race inside the backend's native
// concurrency primitive (mutex, Badger txn, Postgres conditional UPDATE) so
// metadata never regresses even under concurrent rollup workers.
type RollupStore interface {
	// SetRollupOffset atomically advances payloadID's rollup_offset iff
	// newOffset >= the currently-stored offset. Returns the PREVIOUS stored
	// value for observability on success.
	//
	// On monotone violation (newOffset < stored), returns (storedOffset,
	// ErrRollupOffsetRegression); the stored value is unchanged.
	SetRollupOffset(ctx context.Context, payloadID string, newOffset uint64) (storedOffset uint64, err error)

	// GetRollupOffset returns the persisted rollup_offset for payloadID.
	// Returns (0, nil) if not set (a fresh file is treated as rolled-up-to-0).
	GetRollupOffset(ctx context.Context, payloadID string) (uint64, error)
}

// ErrRollupOffsetRegression is returned by SetRollupOffset when newOffset <
// the currently-stored offset (INV-03 violation). Benign by design: callers
// treat it as "another worker raced ahead of me" and drop the partial work.
var ErrRollupOffsetRegression = errors.New("metadata: rollup offset regression rejected")
