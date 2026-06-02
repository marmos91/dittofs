// Package metadata — synced_hash_store.go.
//
// SyncedHashStore persists per-CAS-hash local→remote sync state markers
// for the hybrid local tier. Presence of a marker means the corresponding
// chunk has been successfully mirrored to the remote store at least once;
// absence means the chunk is local-only (or has been intentionally
// reset). Backed by whichever metadata backend the operator configured
// (memory, badger, postgres) — no new infrastructure layer.
//
// All three methods are idempotent by design: MarkSynced on an already-
// marked hash is a no-op, DeleteSynced on an absent hash returns nil,
// IsSynced on an absent hash returns (false, nil). This idempotency
// keeps crash-replay simple and lets the mirror loop use plain Put-then-
// Mark ordering without two-phase commit.
package metadata

import (
	"context"

	"github.com/marmos91/dittofs/pkg/block"
)

// SyncedHashStore persists a single bit of per-hash state: "has this CAS
// chunk been successfully mirrored to the remote store at least once?"
// The marker is logically a set keyed by content hash; backends MAY
// record auxiliary metadata (e.g. a timestamp) but the contract surface
// is boolean.
//
// Implementations MUST be safe for concurrent use by multiple goroutines.
// Methods MUST respect ctx cancellation and return the ctx error early
// when ctx is already done at entry.
type SyncedHashStore interface {
	// IsSynced reports whether hash has been successfully mirrored to
	// the remote store at least once. Returns (false, nil) when no
	// entry exists for hash — an unset hash is treated as "not yet
	// synced", not as an error.
	IsSynced(ctx context.Context, hash block.ContentHash) (bool, error)

	// MarkSynced records that hash has been mirrored to remote.
	// Idempotent: re-applying the same hash is a no-op and returns
	// nil. Callers do not need to check IsSynced before MarkSynced.
	MarkSynced(ctx context.Context, hash block.ContentHash) error

	// DeleteSynced removes the synced marker for hash. Idempotent:
	// deleting an absent hash returns nil. Used by the refcount cascade
	// when the last reference to a hash is dropped, so the synced set
	// stays a strict subset of local CAS contents.
	DeleteSynced(ctx context.Context, hash block.ContentHash) error
}
