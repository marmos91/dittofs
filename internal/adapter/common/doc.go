// Package common provides shared helpers used by every protocol adapter
// (NFSv3, NFSv4, SMB2/3) so that block-store resolution, pooled read buffers,
// and metadata→protocol error mapping live in exactly one place.
//
// # Narrow interfaces, not *runtime.Runtime
//
// Helpers accept BlockStoreRegistry (and a narrow MetadataService interface)
// instead of *runtime.Runtime. This keeps common/ testable with trivial mocks
// and avoids a circular import with pkg/controlplane/runtime; the concrete
// *runtime.Runtime satisfies these interfaces implicitly.
//
// # Block-store helpers
//
// ReadFromBlockStore, WriteToBlockStore, CommitBlockStore, and CopyPayload are
// the single fan-in points for block-store I/O. Every adapter routes its
// READ/WRITE/COMMIT data path through them so the engine contract is exercised
// in exactly one place. ReadFromBlockStore allocates its response buffer from
// internal/adapter/pool (4 KB / 64 KB / 1 MB tiers, with a direct make() fallback
// for sizes above the largest tier).
//
// # Error mapping
//
// The metadata store error → protocol status translation lives in the
// per-adapter types packages (internal/adapter/nfs/types,
// internal/adapter/nfs/v4/types, internal/adapter/smb/types) as StatusFor
// / StatusForErr — the single source of truth per protocol, with a full
// enum-walk test per package. The SMB LOCK path uses StatusForLock /
// StatusForLockErr (lock-vs-general divergence, MS-SMB2 3.3.5.14).
// Block-store errors are classified here: normalizeBlockStoreError wraps
// every error leaving ReadFromBlockStore / WriteToBlockStore /
// CommitBlockStore as a *merrs.StoreError (stale-handle for a closed
// store, I/O for everything else, original preserved as Cause), and
// ClassifyBlockStoreError maps a raw block-store error to its code for
// the raw-engine call sites that bypass the payload helpers.
//
// # Cache invalidation
//
// Cache invalidation is post-transaction by design: the caller commits the
// metadata transaction first, then invokes CacheInvalidator.InvalidateFile so
// the cache reflects committed metadata even if the transaction rolls back.
// The CacheInvalidator interface (cache_invalidator.go) is defined here rather
// than imported from the engine so common helpers stay decoupled from the
// concrete cache type; engine.Cache satisfies it implicitly.
package common
