package metadata

import "context"

// Resetable is an optional capability that metadata store backends may
// implement to support truncate-in-place semantics for share restore. It
// is deliberately NOT embedded in MetadataStore so that protocol handlers
// and the runtime never depend on reset support existing.
//
// Call sites discover the capability via a type assertion:
//
//	if r, ok := store.(metadata.Resetable); ok {
//	    if err := r.Reset(ctx); err != nil { ... }
//	}
//
// Reset truncates all metadata-store contents in-place. The same store
// instance is reused — no close/reopen, no Service unregister/register
// dance, no recreate cost. Implementations MUST preserve the live store
// handle (the underlying *badger.DB, *pgx.Pool, in-memory maps' backing
// allocator) so callers can immediately follow up with Backupable.Restore
// or other store operations without re-resolving the share.
//
// Reset is a destructive primitive intended to be invoked only by
// Runtime.RestoreSnapshot after the operator has disabled the share
// (verified at the orchestration boundary). It bypasses the
// ErrRestoreDestinationNotEmpty guard that Backupable.Restore enforces;
// callers must ensure no concurrent serving traffic is in flight.
//
// Implementations MUST preserve engine-persistent configuration that is
// not "data" — for example, capabilities, storage/file limits, and the
// engine's store identifier (GetStoreID). Resetting those would change
// the engine's identity at the API surface and is out of scope.
type Resetable interface {
	// Reset truncates all metadata content in-place. The receiver
	// continues to be usable after a successful Reset; subsequent
	// operations (including Backupable.Restore) observe an empty store
	// with no shares, files, or other metadata records.
	Reset(ctx context.Context) error
}
