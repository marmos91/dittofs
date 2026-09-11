// Unified BlockStore contract.
//
// This file declares the single CAS-keyed surface that replaces the
// split LocalStore (22 methods) + RemoteStore (12 methods) of v0.15.
// It covers the hash-keyed tier only. The local random-write absorber
// (per-file append log + rollup) is payload-keyed, not hash-keyed, and
// is declared on pkg/block/local.LocalStore instead.
//
// The on-disk format-version stamp and the boot guard that refuses state
// from a newer release (ErrFutureFormat) live in doc.go and errors.go.

package block

import (
	"time"
)

// Meta is the minimal per-object metadata returned by BlockStore.Head
// and BlockStore.Walk. The lookup key (ContentHash) is NEVER echoed
// inside Meta — it is the input, not output.
//
// The S3 backend continues to stamp x-amz-meta-content-hash on every
// PutObject as defense-in-depth (ReadBlockVerified compares
// the header against the recomputed BLAKE3 before returning bytes), but
// that header stays inside the s3 backend and is not surfaced through
// Meta. Callers that need integrity verification use BlockStore.Get
// (which performs the verification on backends that support it) rather
// than reading metadata.
type Meta struct {
	// Size is the object body length in bytes.
	Size int64

	// LastModified is the backend's last-modified timestamp. MUST be
	// non-zero for every object the backend reports — the GC sweep
	// fails closed on a zero LastModified. Backends that cannot
	// natively report a timestamp MUST stamp time.Now() at Put time
	// and surface that value here.
	LastModified time.Time
}

// Store is the content-addressed block storage contract. Every
// implementation is keyed by ContentHash (BLAKE3-256, 32 bytes)
// no opaque "block key" strings appear on this surface.
//
// The production REMOTE tier no longer exposes this hash-keyed surface — it is
// block-keyed via remote.RemoteBlockStore (packed blocks/<id> objects, #1414).
// The remote s3/memory backends still implement Store on their concrete types,
// under the hash-keyed cas/<hash> layout. New production code must not depend on
// remote backends implementing this interface.
//
// Implementations
//   - pkg/block/remote/s3.Store, pkg/block/remote/memory.Store (hash-keyed only)
//   - the compression / encryption decorators, which forward this surface
//     inward through remote.Passthrough
//
// The local tier does NOT implement this interface: *journal.Store is
// payload-keyed (WriteAt / ReadAt / Hydrate / Commit) and satisfies
// pkg/block/local.LocalStore instead.
//
// All methods take ctx context.Context as the first argument and MUST
// honor cancellation. All hash arguments are the full 32-byte
// ContentHash; backends translate to their storage-native location
// (log-blob index entry, in-memory map, …) internally.

// DurabilityReporter is an optional capability a block store (local or
// remote) MAY implement to report whether data it has accepted survives a
// process crash / restart.
//
// Durability is a per-store property and is the foundation of the honest
// CLOSE/COMMIT contract (#1274). The commit rule used by the adapter flush
// seam (internal/adapter/common.CommitBlockStore) is:
//
//	committed := localDurable || (Finalized && remoteDurable)
//
// where localDurable / remoteDurable are read from the local / remote store's
// Durable() report and Finalized comes from the engine FlushResult.
//
// Type defaults (resolved at construction; an operator may override via the
// per-store config["durable"] bool):
//
//   - local fs store        → true  (bytes are on disk, un-mirrored chunks are
//     not evicted, survive restart, re-mirror async)
//   - local memory store    → false (lost on crash/restart)
//   - remote s3 store       → true  (durable object storage)
//   - remote memory store   → false (test fixture, lost on restart)
//
// Stores that do NOT implement this interface are treated conservatively by
// callers (assumed NOT durable) so a missing capability never lets the server
// over-promise durability.
//
// Decorators (encryption, compression) MUST delegate Durable() to the wrapped
// store: wrapping a durable backend does not change where the bytes ultimately
// land.
type DurabilityReporter interface {
	// Durable reports whether data accepted by this store survives a
	// process crash or restart of the daemon.
	Durable() bool
}

// IsDurable reports the conservative durability of store. A store that
// implements DurabilityReporter answers for itself; one that does not (or a nil
// store) is treated as NOT durable so callers never over-promise durability.
// This is the single place the missing-capability default lives — engine
// accessors and the encryption / compression decorators all route through it.
func IsDurable(store any) bool {
	if r, ok := store.(DurabilityReporter); ok {
		return r.Durable()
	}
	return false
}
