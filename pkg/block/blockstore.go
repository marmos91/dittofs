// Types shared by both block-store tiers.
//
// The tier contracts themselves live with their tiers: the block-keyed remote
// surface on remote.RemoteBlockStore, the payload-keyed local absorber
// (per-file append log + rollup) on pkg/block/journal.LocalStore. What stays here
// is what both sides speak — per-object metadata and the durability capability.
//
// The on-disk format-version stamp and the boot guard that refuses state
// from a newer release (ErrFutureFormat) live in README.md and errors.go.

package block

import (
	"time"
)

// Meta is the minimal per-object metadata a backend reports for a stored
// object — see remote.RemoteBlockStore.WalkBlocks. The lookup key is NEVER
// echoed inside Meta: it is the input, not the output.
//
// No store operation verifies content. A chunk's BLAKE3 is recomputed by the
// engine once the decorator stack has returned its plaintext
// (readChunkVerified, pkg/block/engine/fetch.go) — the only layer holding both
// the plaintext and the hash it must match.
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

// DurabilityReporter is an optional capability a block store (local or
// remote) MAY implement to report whether data it has accepted survives a
// process crash / restart.
//
// Durability is a per-store property and is the foundation of the honest
// CLOSE/COMMIT contract. The commit rule used by the adapter flush
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
//   - local journal store   → true  (bytes are on disk, un-mirrored chunks are
//     not evicted, survive restart, re-mirror async)
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
