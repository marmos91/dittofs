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
// errmap.go is the single source of truth mapping metadata store errors to
// per-protocol status codes (NFS3 / NFS4 / SMB) for the general path;
// lock_errmap.go and content_errmap.go cover the lock and content paths.
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
