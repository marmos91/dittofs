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

// SyncedHashStore persists per-hash remote-mirror state: "has this CAS chunk
// been mirrored to the remote store at least once, and where do its bytes live?"
// The marker is logically a set keyed by content hash; each member also carries
// a block.ChunkLocator recording whether the chunk is a standalone CAS object
// (today) or lives inside a pack object (#1414, PR3b). Backends MAY record
// auxiliary metadata (e.g. a first-mirror timestamp).
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

	// MarkSynced records that hash has been mirrored to remote, persisting
	// loc as the chunk's remote location ATOMICALLY with the synced mark
	// (so a crash never leaves a synced hash without a resolvable location).
	// Idempotent: re-applying an already-marked hash is a no-op and returns
	// nil — the first recorded locator wins. Callers do not need to check
	// IsSynced before MarkSynced.
	//
	// A standalone locator (loc.PackID == "") is the common case today and
	// is persisted in the legacy on-disk form — i.e. byte-for-byte identical
	// to pre-locator markers — because a standalone chunk's location is fully
	// implied by its hash (the CAS key, whole object). Only a pack locator
	// (loc.PackID != "") records the extra PackID/Offset/Length, so existing
	// synced rows need no migration and resolve as standalone.
	MarkSynced(ctx context.Context, hash block.ContentHash, loc block.ChunkLocator) error

	// GetLocator returns the recorded remote locator for hash. The second
	// return is true iff hash is synced. A synced hash with no recorded pack
	// locator (a standalone or pre-locator marker) yields the zero
	// block.ChunkLocator (PackID == ""), which the read path resolves to the
	// standalone CAS object. An unsynced hash returns (zero, false, nil).
	GetLocator(ctx context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error)

	// DeleteSynced removes the synced marker for hash. Idempotent:
	// deleting an absent hash returns nil. Used by the refcount cascade
	// when the last reference to a hash is dropped, so the synced set
	// stays a strict subset of local CAS contents.
	DeleteSynced(ctx context.Context, hash block.ContentHash) error
}

// NOTE: backends additionally expose a concrete
//
//	EnumerateSynced(ctx, func(hash block.ContentHash, syncedAt time.Time) error) error
//
// method used by the LIST-free GC sweep (#1433). It is deliberately NOT part of
// the SyncedHashStore contract: only the GC consumer needs it, so the engine
// declares the narrow interface it depends on (see engine.SyncedHashIndex)
// rather than widening this surface, which is consumed by the syncer and
// eviction paths that need only the per-hash CRUD above. The enumerator
// conformance is shared via RunSyncedHashEnumeratorSuite.
