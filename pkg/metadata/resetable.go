package metadata

import "context"

// Resetable is an optional capability that metadata stores may implement
// to support truncate-in-place semantics for share restore. It is
// deliberately not embedded in MetadataStore so protocol handlers never
// depend on reset support existing; callers discover it via type
// assertion.
//
// Reset is a destructive primitive intended to be invoked only by
// Runtime.RestoreSnapshot after the operator has disabled the share.
// It bypasses Backupable.Restore's ErrRestoreDestinationNotEmpty guard;
// callers must ensure no concurrent serving traffic is in flight.
//
// Implementations must preserve the live store handle so callers can
// immediately follow up with Backupable.Restore. Engine-persistent
// configuration that is not data (capabilities, storage/file limits,
// GetStoreID) must also be preserved — resetting those would change
// engine identity at the API surface.
type Resetable interface {
	// Reset truncates all metadata content in-place. The receiver
	// continues to be usable; subsequent operations observe an empty
	// store with no shares, files, or other metadata records.
	Reset(ctx context.Context) error
}
